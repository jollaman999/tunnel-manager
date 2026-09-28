package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// transferWakes stands in for the tunnel manager. An import asks for a
// reconcile pass once it has committed, and nothing else of the manager is
// reached from there.
type transferWakes struct {
	mu    sync.Mutex
	wakes int
}

func (m *transferWakes) WakeReconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.wakes++
}

func (m *transferWakes) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.wakes
}

func (m *transferWakes) DesiredTunnelCount() (int, error) {
	return 0, nil
}

func (m *transferWakes) DesiredLocalForwardCount() (int, error) {
	return 0, nil
}

func (m *transferWakes) GetAllTunnels() (*[]models.Tunnel, error) {
	return &[]models.Tunnel{}, nil
}

func (m *transferWakes) GetHostTunnels(hostID uint) (*[]models.Tunnel, error) {
	return &[]models.Tunnel{}, nil
}

func (m *transferWakes) LocalForwardStatuses() map[tunnel.LocalForwardKey]tunnel.LocalForwardState {
	return nil
}

func (m *transferWakes) SocksStatuses() map[uint]tunnel.SocksState {
	return nil
}

// transferInstall is one tunnel-manager: a database file of its own, the
// encryption key its secrets are sealed with, and the handlers served over it.
//
// Two of them are built where an export has to cross installations. That is the
// whole point of the file: the secrets are sealed in the database with a key
// that never leaves the machine, so a test that moves a file between two
// handlers sharing one key would pass while the feature does not work.
type transferInstall struct {
	db      *gorm.DB
	cipher  *crypto.Cipher
	handler *TransferHandler
	manager *transferWakes
	logs    *observer.ObservedLogs
	// account is the one account of the installation, whose password is
	// testPassword. The export and the import of the tunnels ask for it.
	account uint
}

// transferAccountHash is the hash of testPassword every transferInstall
// stores. It is made once and at the lowest cost bcrypt takes, because every
// export and every import of the tunnels compares against it and this package
// has a great many of them: at the default cost the comparisons alone would
// add minutes to a run under the race detector.
var transferAccountHash = sync.OnceValues(func() ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
})

func newTransferInstall(t *testing.T) *transferInstall {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	err = db.AutoMigrate(&models.Host{}, &models.HostJump{}, &models.ServicePort{}, &models.HostServicePort{},
		&models.LocalForward{}, &models.Tunnel{}, &settings.Settings{}, &models.User{}, &models.APIToken{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	hash, err := transferAccountHash()
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	account := models.User{Username: testUsername, PasswordHash: string(hash)}

	err = db.Create(&account).Error
	if err != nil {
		t.Fatalf("failed to create the account: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	manager := &transferWakes{}
	cipher := newTestCipher(t)

	return &transferInstall{
		db:      db,
		cipher:  cipher,
		handler: NewTransferHandler(NewHandler(db, manager, logger, cipher), "0.0.0-test", testDatabaseFile),
		manager: manager,
		logs:    logs,
		account: account.ID,
	}
}

// platformAbsolutePath spells the Unix path p as an absolute path of the
// platform the test runs on. filepath decides what is absolute by the rules of
// that platform, and on Windows a path without a drive is not.
func platformAbsolutePath(p string) string {
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(p)
	}

	return p
}

// count is how many rows of a model are stored. An import that was refused and
// still wrote a row is worse than the refusal it answered with.
func (i *transferInstall) count(t *testing.T, model interface{}) int64 {
	t.Helper()

	var rows int64

	err := i.db.Model(model).Count(&rows).Error
	if err != nil {
		t.Fatalf("failed to count the rows: %v", err)
	}

	return rows
}

// call runs one of the four handlers with the body given and hands back what it
// wrote. The account and the limiter are left on the context the way the
// session middleware leaves them, since three of the four ask for the password
// of the account.
func (i *transferInstall) call(t *testing.T, handler func(echo.Context) error, body string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	req := httptest.NewRequest(http.MethodPost, "/api/transfer", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	leaveSessionOnContext(c, i.account)

	err := handler(c)
	if err != nil {
		t.Fatalf("the handler returned error: %v", err)
	}

	return rec
}

// transferAnswer is the answer of one of the four calls, with the data left as
// it was written so that each test reads the fields it is about.
type transferAnswer struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func decodeTransfer(t *testing.T, rec *httptest.ResponseRecorder) transferAnswer {
	t.Helper()

	var answer transferAnswer

	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	return answer
}

// into reads the data of an answer.
func (a transferAnswer) into(t *testing.T, target interface{}) {
	t.Helper()

	err := json.Unmarshal(a.Data, target)
	if err != nil {
		t.Fatalf("failed to read the data of the answer: %v", err)
	}
}

// testExportPassword is what the files of these tests are sealed with. It is
// long enough for the rule the export holds it to.
const testExportPassword = "correct horse battery staple"

// registerHost stores one Host the way a create does, secrets sealed with the
// key of that installation.
func (i *transferInstall) registerHost(t *testing.T, host hostContent) models.Host {
	t.Helper()

	password, err := i.handler.hosts.sealPassword(host.Password)
	if err != nil {
		t.Fatalf("failed to seal the password: %v", err)
	}

	privateKey, keyPassphrase, err := i.handler.hosts.sealPrivateKey(host.PrivateKey, host.KeyPassphrase)
	if err != nil {
		t.Fatalf("failed to seal the private key: %v", err)
	}

	stored := models.Host{
		Address:       host.Address,
		Port:          host.Port,
		User:          host.User,
		Password:      password,
		PrivateKey:    privateKey,
		KeyPassphrase: keyPassphrase,
		HostKey:       host.HostKey,
		Description:   host.Description,
		Enabled:       host.Enabled,
	}

	err = i.db.Create(&stored).Error
	if err != nil {
		t.Fatalf("failed to store the Host: %v", err)
	}

	return stored
}

func (i *transferInstall) registerServicePort(t *testing.T, sp servicePortContent) {
	t.Helper()

	stored := models.ServicePort{
		ServiceAddress: sp.ServiceAddress,
		ServicePort:    sp.ServicePort,
		LocalPort:      sp.LocalPort,
		Description:    sp.Description,
	}

	err := i.db.Create(&stored).Error
	if err != nil {
		t.Fatalf("failed to store the service port: %v", err)
	}
}

// exportBody is what an export is asked for with: the password the file is
// sealed with, and the password of the account the export is made under.
func exportBody(t *testing.T, password string) string {
	t.Helper()

	return `{"password":` + jsonString(t, password) + `,"account_password":` + jsonString(t, testPassword) + `}`
}

// exportTunnels runs the export and hands back the sealed file.
func (i *transferInstall) exportTunnels(t *testing.T, password string) string {
	t.Helper()

	rec := i.call(t, i.handler.ExportTunnels, exportBody(t, password))
	if rec.Code != http.StatusOK {
		t.Fatalf("the export answered %d: %s", rec.Code, rec.Body.String())
	}

	var exported exportedTunnels

	decodeTransfer(t, rec).into(t, &exported)

	return exported.File
}

// importTunnels runs the import of a file and hands back what it answered.
func (i *transferInstall) importTunnels(t *testing.T, file string, password string) *httptest.ResponseRecorder {
	t.Helper()

	return i.importTunnelsWith(t, map[string]interface{}{
		"password":         password,
		"account_password": testPassword,
		"file":             file,
	})
}

// importTunnelsWith runs the import with the body given, for the tests that
// send a field the ordinary call does not.
func (i *transferInstall) importTunnelsWith(t *testing.T, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to write the body: %v", err)
	}

	return i.call(t, i.handler.ImportTunnels, string(encoded))
}

// twoHosts is the pair every test that moves a configuration starts from: one
// Host that is logged in to with a key, one with a password. The two ways in
// are sealed differently and have to survive the trip in the same file.
func twoHosts(t *testing.T) (withKey hostContent, withPassword hostContent) {
	t.Helper()

	withKey = hostContent{
		Address:       "192.0.2.10",
		Port:          22,
		User:          "operator",
		PrivateKey:    testPrivateKeyPEM(t, "the passphrase of the key"),
		KeyPassphrase: "the passphrase of the key",
		Description:   "the Host with a key",
		Enabled:       true,
	}

	withPassword = hostContent{
		Address:     "192.0.2.11",
		Port:        22,
		User:        "operator",
		Password:    "the password of the Host",
		Description: "the Host with a password",
		Enabled:     true,
	}

	return withKey, withPassword
}

// TestTheSettingsContentCarriesEverySetting holds the settings in a file
// against the settings of the database. A setting added to one and not to the
// other is carried by nothing and would be found by whoever imports a file and
// sees the setting fall back to what was already stored.
//
// The row id and the time the row was written are left out on purpose: both
// describe the row an export was read from. So is AlertSecrets, which is the
// sealed form of six settings the file carries one by one in the clear.
func TestTheSettingsContentCarriesEverySetting(t *testing.T) {
	left := map[string]bool{"ID": true, "UpdatedAt": true, "AlertSecrets": true}

	stored := reflect.TypeOf(settings.Settings{})
	carried := reflect.TypeOf(settingsContent{})

	for i := 0; i < stored.NumField(); i++ {
		name := stored.Field(i).Name
		if left[name] {
			continue
		}

		_, found := carried.FieldByName(name)
		if !found {
			t.Errorf("settings.Settings has %s and settingsContent does not, so the setting is "+
				"not carried by an export", name)
		}
	}

	for i := 0; i < carried.NumField(); i++ {
		name := carried.Field(i).Name

		_, found := stored.FieldByName(name)
		if !found {
			t.Errorf("settingsContent has %s and settings.Settings does not", name)
		}
	}
}

// TestTheHostContentCarriesEveryFieldOfAHost does for a Host what the test
// above does for the settings. Every field is carried, the id, the times and
// the key waiting for approval included, so that an import puts the row back
// as it was.
func TestTheHostContentCarriesEveryFieldOfAHost(t *testing.T) {
	stored := reflect.TypeOf(models.Host{})
	carried := reflect.TypeOf(hostContent{})

	for i := 0; i < stored.NumField(); i++ {
		name := stored.Field(i).Name

		_, found := carried.FieldByName(name)
		if !found {
			t.Errorf("models.Host has %s and hostContent does not, so it is not carried by an export", name)
		}
	}
}

// TestTheServicePortContentCarriesEveryFieldOfAServicePort is the other half of
// the check above.
//
// A field added to the model and not here would be dropped by an export
// without a word, and the installation that imported the file would come up
// running something other than what was exported.
func TestTheServicePortContentCarriesEveryFieldOfAServicePort(t *testing.T) {
	stored := reflect.TypeOf(models.ServicePort{})
	carried := reflect.TypeOf(servicePortContent{})

	for i := 0; i < stored.NumField(); i++ {
		name := stored.Field(i).Name

		_, found := carried.FieldByName(name)
		if !found {
			t.Errorf("models.ServicePort has %s and servicePortContent does not, "+
				"so it is not carried by an export", name)
		}
	}
}

// TestTheAssignmentContentCarriesEveryFieldOfAnAssignment is the same for an
// assignment. The Host is the one it is written under, and the service port is
// named by its id under a name of its own.
func TestTheAssignmentContentCarriesEveryFieldOfAnAssignment(t *testing.T) {
	left := map[string]string{"HostID": "", "SPID": "ServicePortID"}

	stored := reflect.TypeOf(models.HostServicePort{})
	carried := reflect.TypeOf(assignmentContent{})

	for i := 0; i < stored.NumField(); i++ {
		name := stored.Field(i).Name

		as, renamed := left[name]
		if renamed {
			if as == "" {
				continue
			}

			name = as
		}

		_, found := carried.FieldByName(name)
		if !found {
			t.Errorf("models.HostServicePort has %s and assignmentContent does not, "+
				"so it is not carried by an export", name)
		}
	}
}

func TestAnExportedFileHoldsNoSecretInTheClear(t *testing.T) {
	source := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)

	file := source.exportTunnels(t, testExportPassword)

	secrets := map[string]string{
		"the SSH password":       withPassword.Password,
		"the private key":        withKey.PrivateKey,
		"the passphrase":         withKey.KeyPassphrase,
		"the sealing password":   testExportPassword,
		"a line of the PEM body": strings.Split(strings.TrimSpace(withKey.PrivateKey), "\n")[1],
	}

	for what, secret := range secrets {
		if strings.Contains(file, secret) {
			t.Errorf("the exported file holds %s in the clear", what)
		}
	}

	// The other half of the rule: with the password the secrets are there. A
	// file that simply dropped them would pass the check above as well.
	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the exported file does not open with the password it was sealed with: %v", err)
	}

	// The key is looked for as JSON writes it, since the newlines of a PEM
	// block arrive inside the file as the two characters JSON escapes them to.
	writtenKey := strings.Trim(jsonString(t, strings.TrimSpace(withKey.PrivateKey)), `"`)

	if !strings.Contains(opened, withPassword.Password) || !strings.Contains(opened, writtenKey) {
		t.Fatalf("the opened file does not carry the secrets of the Hosts")
	}

	if !strings.Contains(opened, `"id":1,`) || !strings.Contains(opened, `"created_at"`) ||
		!strings.Contains(opened, `"updated_at"`) || !strings.Contains(opened, `"format_version":2`) {
		t.Errorf("the file does not carry the row ids and the timestamps of the installation: %s", opened)
	}
}

// TestAnExportIsRefusedWithoutAPasswordThatHolds keeps the file to the length
// the account is held to, since the file carries the credentials of every Host
// for as long as it is kept.
func TestAnExportIsRefusedWithoutAPasswordThatHolds(t *testing.T) {
	source := newTransferInstall(t)

	for _, password := range []string{"", "short", strings.Repeat("a", maxPasswordBytes+1)} {
		rec := source.call(t, source.handler.ExportTunnels, exportBody(t, password))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("the export of a %d byte password answered %d, want %d",
				len(password), rec.Code, http.StatusBadRequest)
		}
	}
}

func TestAnExportedConfigurationIsReadableOnAnotherInstallation(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)
	source.registerServicePort(t, servicePortContent{
		ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "a service",
	})

	file := source.exportTunnels(t, testExportPassword)

	// The two installations must not share a key, or this test would pass with
	// the sealed values carried across as they are.
	sealedHere, err := source.cipher.Encrypt("a value")
	if err != nil {
		t.Fatalf("failed to seal a value: %v", err)
	}

	_, err = target.cipher.Decrypt(sealedHere)
	if err == nil {
		t.Fatalf("the two installations were built with the same encryption key")
	}

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	want := importedTunnels{File: transferCounts{Hosts: 2, ServicePorts: 1}}
	if imported != want {
		t.Fatalf("the import answered %+v, want %+v", imported, want)
	}

	for _, want := range []hostContent{withKey, withPassword} {
		var stored models.Host

		err = target.db.Where("address = ?", want.Address).First(&stored).Error
		if err != nil {
			t.Fatalf("the Host %s was not stored: %v", want.Address, err)
		}

		for _, secret := range []struct {
			what   string
			stored string
			want   string
		}{
			{"password", stored.Password, want.Password},
			{"private key", stored.PrivateKey, want.PrivateKey},
			{"key passphrase", stored.KeyPassphrase, want.KeyPassphrase},
		} {
			if secret.want == "" {
				if secret.stored != "" {
					t.Errorf("the %s of the Host %s was stored while the file carried none",
						secret.what, want.Address)
				}

				continue
			}

			if !crypto.IsEncrypted(secret.stored) {
				t.Errorf("the %s of the Host %s is not sealed with the key of this installation",
					secret.what, want.Address)
			}

			opened, err := target.cipher.Decrypt(secret.stored)
			if err != nil {
				t.Fatalf("the %s of the Host %s does not open with the key of this installation: %v",
					secret.what, want.Address, err)
			}

			if strings.TrimSpace(opened) != strings.TrimSpace(secret.want) {
				t.Errorf("the %s of the Host %s came across altered", secret.what, want.Address)
			}
		}

		if stored.Port != want.Port || stored.User != want.User ||
			stored.Description != want.Description || stored.Enabled != want.Enabled {
			t.Errorf("the Host %s came across with other fields than it was exported with", want.Address)
		}
	}

	var sp models.ServicePort

	err = target.db.Where("local_port = ?", 18080).First(&sp).Error
	if err != nil {
		t.Fatalf("the service port was not stored: %v", err)
	}

	if sp.ServiceAddress != "192.0.2.20" || sp.ServicePort != 80 {
		t.Errorf("the service port came across as %s:%d", sp.ServiceAddress, sp.ServicePort)
	}

	if target.manager.count() != 1 {
		t.Errorf("the import asked for %d reconcile passes, want 1", target.manager.count())
	}
}

// TestTheSameFileImportedTwiceLeavesTheSameConfiguration is what an import
// that replaces rather than adds does with a file sent twice: the second time
// replaces what the first wrote with the same rows, and the answer counts what
// was there as well as what the file holds.
func TestTheSameFileImportedTwiceLeavesTheSameConfiguration(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)
	source.registerServicePort(t, servicePortContent{
		ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18080,
	})
	source.assign(t, withKey.Address, 18080)

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the first import answered %d: %s", rec.Code, rec.Body.String())
	}

	first := target.configuration(t)

	rec = target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the second import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	counts := transferCounts{Hosts: 2, ServicePorts: 1, Assignments: 1}
	if imported != (importedTunnels{Current: counts, File: counts}) {
		t.Fatalf("the second import answered %+v, want %+v on both sides", imported, counts)
	}

	if !reflect.DeepEqual(target.configuration(t), first) {
		t.Fatalf("the second import left another configuration:\n%v\nwant\n%v", target.configuration(t), first)
	}
}

// TestAnImportReplacesEverythingStoredHere is what the import is: every Host,
// service port, assignment, local forward and jump route stored here is gone
// and what the file holds is stored in its place, under the ids of the file.
func TestAnImportReplacesEverythingStoredHere(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)
	source.registerServicePort(t, servicePortContent{
		ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "as exported",
	})

	file := source.exportTunnels(t, testExportPassword)

	// The target holds the same address under other values, a Host the file
	// does not name, and something of every table.
	stale := withPassword
	stale.Port = 2222
	stale.User = "somebody-else"
	stale.Password = "the password that is stored here"
	target.registerHost(t, stale)
	target.registerHost(t, passwordHost("192.0.2.99"))
	target.registerHost(t, passwordHost("192.0.2.98"))
	target.registerServicePort(t, servicePortContent{
		ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "as stored here",
	})
	target.registerServicePort(t, servicePortContent{
		ServiceAddress: "192.0.2.21", ServicePort: 80, LocalPort: 18081,
	})
	target.assign(t, "192.0.2.99", 18081)
	target.forward(t, "192.0.2.99", localForwardContent{LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 5432})
	target.jump(t, "192.0.2.99", "192.0.2.98")

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	want := importedTunnels{
		Current: transferCounts{Hosts: 3, ServicePorts: 2, Assignments: 1, LocalForwards: 1, JumpHosts: 1},
		File:    transferCounts{Hosts: 2, ServicePorts: 1},
	}
	if imported != want {
		t.Fatalf("the import answered %+v, want %+v", imported, want)
	}

	if !reflect.DeepEqual(target.configuration(t), source.configuration(t)) {
		t.Fatalf("after the import the installation holds\n%v\nwant what the file came from\n%v",
			target.configuration(t), source.configuration(t))
	}

	if target.count(t, &models.LocalForward{}) != 0 || target.count(t, &models.HostJump{}) != 0 ||
		target.count(t, &models.HostServicePort{}) != 0 {
		t.Fatalf("the import left rows of the configuration it replaced")
	}
}

// TestAnImportThatIsRefusedWritesNothing is the transaction. The second Host of
// the file cannot be stored, and what the import has to leave behind is the
// database as it was, what was stored here included.
func TestAnImportThatIsRefusedWritesNothing(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("192.0.2.99"))

	before := target.configuration(t)

	withKey, withPassword := twoHosts(t)

	// The file is built by hand rather than exported, because what is being
	// tested is a file an export would never write: the port of the second Host
	// is out of range, which the rules of a create refuse.
	broken := withPassword
	broken.Port = 70000

	file := sealedTunnelsFile(t, source, tunnelsContent{
		Hosts: []hostContent{withKey, broken},
		ServicePorts: []servicePortContent{
			{ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18080},
		},
	}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	answer := decodeTransfer(t, rec)
	if !strings.Contains(answer.Error, broken.Address) {
		t.Errorf("the refusal does not name the Host that stopped the import: %q", answer.Error)
	}

	if !reflect.DeepEqual(target.configuration(t), before) {
		t.Fatalf("the refused import changed what is stored to\n%v\nwant\n%v", target.configuration(t), before)
	}

	if target.manager.count() != 0 {
		t.Errorf("a reconcile pass was asked for although nothing was imported")
	}
}

// TestAHostWithNoWayInIsRefused holds the import to the rule a create is held
// to: a Host with neither a key nor a password is one nothing can log in with.
func TestAHostWithNoWayInIsRefused(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	file := sealedTunnelsFile(t, source, tunnelsContent{
		Hosts: []hostContent{{Address: "192.0.2.10", Port: 22, User: "operator", Enabled: true}},
	}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the import answered %d, want %d", rec.Code, http.StatusBadRequest)
	}

	if !strings.Contains(decodeTransfer(t, rec).Error, "no way to log in") {
		t.Errorf("the refusal does not say what is wrong: %q", decodeTransfer(t, rec).Error)
	}

	if target.count(t, &models.Host{}) != 0 {
		t.Fatalf("the Host was stored")
	}
}

// TestAServicePortTheFileHoldsTwiceIsRefused is a file whose two service ports
// meet on the service address or on the local port. Either would be refused by
// the database halfway through the import, so the file is refused before it,
// with the second of the two named.
func TestAServicePortTheFileHoldsTwiceIsRefused(t *testing.T) {
	for _, second := range []servicePortContent{
		{ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18081},
		{ServiceAddress: "192.0.2.21", ServicePort: 80, LocalPort: 18080},
	} {
		t.Run(second.ServiceAddress, func(t *testing.T) {
			source := newTransferInstall(t)
			target := newTransferInstall(t)

			target.registerServicePort(t, servicePortContent{ServiceAddress: "192.0.2.30", ServicePort: 80, LocalPort: 18090})

			before := target.configuration(t)

			file := sealedTunnelsFile(t, source, tunnelsContent{
				ServicePorts: []servicePortContent{
					{ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18080},
					second,
				},
			}, testExportPassword)

			rec := target.importTunnels(t, file, testExportPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			var refused struct {
				Code string            `json:"error_code"`
				Args map[string]string `json:"error_args"`
			}

			if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
				t.Fatalf("the refusal is not JSON: %v", err)
			}

			if refused.Code != string(errImportServicePortDuplicate) {
				t.Fatalf("the refusal is %q, want %q", refused.Code, errImportServicePortDuplicate)
			}

			name := second.ServiceAddress + ":80 on " + strconv.Itoa(second.LocalPort)
			if refused.Args["service_port"] != name ||
				refused.Args["service_port_code"] != string(textImportNameServicePort) {
				t.Errorf("the refusal carries %v, want the service port %q named by its code", refused.Args, name)
			}

			if !reflect.DeepEqual(target.configuration(t), before) {
				t.Fatalf("the refused import changed what is stored")
			}
		})
	}
}

// TestARefusedServicePortIsNamedByCode reads the refusal of a service port the
// file carries with a value no service port may have. The name of the service
// port is the server's own English, so it is carried with a code beside it and
// the values that code's phrase is written with.
func TestARefusedServicePortIsNamedByCode(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	file := sealedTunnelsFile(t, source, tunnelsContent{
		ServicePorts: []servicePortContent{
			{ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 0},
		},
	}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var body struct {
		Error string            `json:"error"`
		Code  string            `json:"error_code"`
		Args  map[string]string `json:"error_args"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}

	if body.Code != string(errImportServicePortRefused) {
		t.Fatalf("the refusal is %q, want %q", body.Code, errImportServicePortRefused)
	}

	want := map[string]string{
		"service_port":      "192.0.2.20:80 on 0",
		"service_port_code": string(textImportNameServicePort),
		"service_address":   "192.0.2.20:80",
		"local_port":        "0",
	}

	for name, value := range want {
		if body.Args[name] != value {
			t.Errorf("the refusal carries %q under %q, want %q", body.Args[name], name, value)
		}
	}

	if body.Args["reason"] == "" {
		t.Errorf("the refusal carries no reason: %v", body.Args)
	}

	if !strings.HasPrefix(body.Error, "Nothing was imported. The service port 192.0.2.20:80 on 0 in the file was refused: ") {
		t.Errorf("the English sentence no longer says what it did: %q", body.Error)
	}
}

// TestEveryWayOfNotOpeningAFileIsAnsweredApart is what tells the operator what
// to do next: type the password again, pick another file, fetch the file again,
// or go to the other import. One message for all four would leave them guessing.
func TestEveryWayOfNotOpeningAFileIsAnsweredApart(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, _ := twoHosts(t)
	source.registerHost(t, withKey)

	file := source.exportTunnels(t, testExportPassword)

	settingsFile := source.exportSettings(t, testExportPassword)

	damaged := file[:len(file)-6] + "AAAA"

	type refusedFile struct {
		what   string
		answer *httptest.ResponseRecorder
	}

	cases := []refusedFile{
		{"a wrong password", target.importTunnels(t, file, "another password entirely")},
		{"not a file of ours", target.importTunnels(t, "just some text", testExportPassword)},
		{"a damaged file", target.importTunnels(t, damaged, testExportPassword)},
		{"the other kind", target.importTunnels(t, settingsFile, testExportPassword)},
	}

	seen := map[string]string{}

	for _, one := range cases {
		if one.answer.Code != http.StatusBadRequest {
			t.Fatalf("%s answered %d, want %d: %s", one.what, one.answer.Code,
				http.StatusBadRequest, one.answer.Body.String())
		}

		message := decodeTransfer(t, one.answer).Error
		if message == "" {
			t.Fatalf("%s was refused without a word", one.what)
		}

		already, repeated := seen[message]
		if repeated {
			t.Errorf("%s and %s are answered with the same message: %q", one.what, already, message)
		}

		seen[message] = one.what
	}

	if target.count(t, &models.Host{}) != 0 {
		t.Fatalf("a file that did not open wrote rows")
	}

	// The settings import refuses the tunnel file the same way round, so that
	// neither call reads what the other one wrote.
	rec := target.call(t, target.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the settings import took a tunnel file: %d %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(decodeTransfer(t, rec).Error, "tunnel configuration") {
		t.Errorf("the refusal does not say what the file holds: %q", decodeTransfer(t, rec).Error)
	}
}

// TestAWrongKindNamesBothKinds reads the values of the wrong-kind refusal. The
// two phrases in it are the server's own English, so each is carried with a
// code beside it under which a screen says it in its own language, and the
// kind a file names that this version has no phrase for is carried as it is.
func TestAWrongKindNamesBothKinds(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	settingsFile := source.exportSettings(t, testExportPassword)

	rec := target.importTunnels(t, settingsFile, testExportPassword)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the tunnel import took a settings file: %d %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Error string            `json:"error"`
		Code  string            `json:"error_code"`
		Args  map[string]string `json:"error_args"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}

	if body.Code != string(errImportFileWrongKind) {
		t.Fatalf("the refusal is %q, want %q", body.Code, errImportFileWrongKind)
	}

	want := map[string]string{
		"found":       "the settings of the manager",
		"found_code":  string(textImportKindSettings),
		"wanted":      "the tunnel configuration",
		"wanted_code": string(textImportKindTunnels),
	}

	if !reflect.DeepEqual(body.Args, want) {
		t.Errorf("the refusal carries %v, want %v", body.Args, want)
	}

	if !strings.Contains(body.Error, want["found"]) || !strings.Contains(body.Error, want["wanted"]) {
		t.Errorf("the English sentence %q no longer says what it did", body.Error)
	}

	// A kind this version has no phrase for is said with the kind itself, so
	// the kind travels on its own beside the code of the phrase.
	found, code := whatIsIn("something-else")
	if code != textImportKindUnknown || !strings.Contains(found, "(something-else)") {
		t.Errorf("an unknown kind is said as %q under %q", found, code)
	}

	found, code = whatIsIn("")
	if code != textImportKindNone || found == "" {
		t.Errorf("no kind at all is said as %q under %q", found, code)
	}
}

// TestAnUnknownKindCarriesTheKind seals a file of a kind this version does not
// know and reads the refusal, which is the one path that writes "kind" into
// the values.
func TestAnUnknownKindCarriesTheKind(t *testing.T) {
	source := newTransferInstall(t)

	file, err := source.handler.seal("somebody-elses-kind", tunnelsContent{}, testExportPassword, time.Now())
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec := source.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the import took a file of an unknown kind: %d %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Error string            `json:"error"`
		Args  map[string]string `json:"error_args"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}

	if body.Args["found_code"] != string(textImportKindUnknown) {
		t.Errorf("the file is named %q, want %q", body.Args["found_code"], textImportKindUnknown)
	}
	if body.Args["kind"] != "somebody-elses-kind" {
		t.Errorf("the kind the file names is carried as %q", body.Args["kind"])
	}
	if !strings.Contains(body.Error, "(somebody-elses-kind)") {
		t.Errorf("the English sentence %q does not name the kind", body.Error)
	}
}

// exportSettings runs the settings export and hands back the sealed file.
func (i *transferInstall) exportSettings(t *testing.T, password string) string {
	t.Helper()

	rec := i.call(t, i.handler.ExportSettings, exportBody(t, password))
	if rec.Code != http.StatusOK {
		t.Fatalf("the export answered %d: %s", rec.Code, rec.Body.String())
	}

	var exported exportedSettings

	decodeTransfer(t, rec).into(t, &exported)

	return exported.File
}

// sealedTunnelsFile builds a file the way the release before the ids built
// one, for the tests that need a content no export would write. Such a
// content carries no ids, which is what that release wrote.
func sealedTunnelsFile(t *testing.T, i *transferInstall, content tunnelsContent, password string) string {
	t.Helper()

	return sealedAtFormat(t, 1, content, password)
}

// sealedAtFormat seals a content as a file of the format version given.
func sealedAtFormat(t *testing.T, version int, content interface{}, password string) string {
	t.Helper()

	body, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("failed to write the content: %v", err)
	}

	plaintext, err := json.Marshal(transferFile{
		Kind:          transferKindTunnels,
		FormatVersion: version,
		ExportedAt:    time.Now(),
		ExportedBy:    "0.0.0-test",
		Content:       body,
	})
	if err != nil {
		t.Fatalf("failed to write the file: %v", err)
	}

	file, err := crypto.EncryptWithPassword(string(plaintext), password)
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	return file
}

// TestTheImportedSettingsAreStoredAndNotPutIntoPlace is the safety this import
// rests on. A file from another installation names the port to listen on and
// where the logs go, and a process that took those on while it was answering
// the request would be a process nobody can reach afterwards.
func TestTheImportedSettingsAreStoredAndNotPutIntoPlace(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored.APIPort = 9999
	stored.MonitoringIntervalSec = 47
	stored.LoggingFormat = "console"

	err = settings.Save(source.db, stored, source.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	file := source.exportSettings(t, testExportPassword)

	// The Settings screen of the target is built before the import, the way a
	// startup builds it: it holds the settings this process is running on.
	screen, _, _, _ := newSettingsHandler(t, target.db)

	running, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedSettings

	decodeTransfer(t, rec).into(t, &imported)

	if !imported.RestartRequired {
		t.Errorf("the import does not say a restart is needed although the port changed")
	}

	if imported.Settings.APIPort != 9999 || imported.Settings.MonitoringIntervalSec != 47 ||
		imported.Settings.LoggingFormat != "console" {
		t.Fatalf("the answer does not carry the settings of the file: %+v", imported.Settings)
	}

	after, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.APIPort != 9999 || after.MonitoringIntervalSec != 47 || after.LoggingFormat != "console" {
		t.Fatalf("the settings of the file were not stored: %+v", after)
	}

	// What the process is running on is untouched, and the Settings screen says
	// so: every setting the file changed is waiting for a restart.
	rec = settingsRequest(t, screen, "")

	var view struct {
		Data struct {
			APIPort        int `json:"api_port"`
			PendingRestart []struct {
				Name    string `json:"name"`
				Running string `json:"running"`
				Stored  string `json:"stored"`
			} `json:"pending_restart"`
		} `json:"data"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &view)
	if err != nil {
		t.Fatalf("failed to read the settings screen: %v", err)
	}

	waiting := map[string]string{}
	for _, pending := range view.Data.PendingRestart {
		waiting[pending.Name] = pending.Running + " -> " + pending.Stored
	}

	for _, name := range []string{"api.port", "monitoring.interval_sec", "logging.format"} {
		_, found := waiting[name]
		if !found {
			t.Errorf("%s is not reported as waiting for a restart, and it is: %v", name, waiting)
		}
	}

	if waiting["api.port"] != "8888 -> 9999" {
		t.Errorf("the port waiting for a restart is reported as %q", waiting["api.port"])
	}

	// The running value is what the process was started on. The screen reads it
	// out of the handler that was built before the import, so a screen that
	// showed the stored value as the running one would fail here.
	if running.APIPort != 8888 {
		t.Fatalf("the settings this process runs on were read as %d", running.APIPort)
	}
}

// TestImportedSettingsThatAreRefusedAreNotStored holds the file to the rules
// the Settings screen is held to. A stored setting that does not pass them
// keeps the process from starting again, and the Settings screen that would put
// it right is served by the server that will not start.
func TestImportedSettingsThatAreRefusedAreNotStored(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	content := settingsOf(stored)
	content.LoggingLevel = "chatty"

	file, err := source.handler.seal(transferKindSettings, content, testExportPassword, time.Now())
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	if !strings.Contains(decodeTransfer(t, rec).Error, "chatty") {
		t.Errorf("the refusal does not name the value that was refused: %q", decodeTransfer(t, rec).Error)
	}

	after, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.LoggingLevel != "info" {
		t.Fatalf("the refused settings were stored: the level is %q", after.LoggingLevel)
	}
}

// TestASettingTheFileDoesNotNameIsLeftAsItIs is what a file written by an older
// version arrives as: the settings it names are stored and the rest keep the
// value this installation has, rather than being stored as zeros.
func TestASettingTheFileDoesNotNameIsLeftAsItIs(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	file, err := source.handler.seal(transferKindSettings,
		map[string]int{"monitoring_interval_sec": 11}, testExportPassword, time.Now())
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	after, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.MonitoringIntervalSec != 11 {
		t.Errorf("the setting the file named was not stored: %d", after.MonitoringIntervalSec)
	}

	if after.APIPort != 8888 || after.LoggingLevel != "info" {
		t.Errorf("a setting the file did not name was overwritten: %+v", after)
	}
}

// TestAnImportThatPutsTheLogOnTheKeyIsRefused holds an imported file to the
// rule a save is held to. Unlike a path outside the installation it is not
// dropped to the default: no version stored it, so no installation ran on it.
func TestAnImportThatPutsTheLogOnTheKeyIsRefused(t *testing.T) {
	cases := []struct{ key, log string }{
		{"keys/tunnel-manager.key", "./Keys/tunnel-manager.KEY"},
		{"keys/tunnel-manager.key", "tunnel-manager.db-wal"},
		{"initial-password", "logs/tunnel-manager.log"},
	}

	for _, c := range cases {
		t.Run(c.key+" "+c.log, func(t *testing.T) {
			source := newTransferInstall(t)
			target := newTransferInstall(t)

			stored, err := settings.Load(source.db)
			if err != nil {
				t.Fatalf("failed to read the settings: %v", err)
			}

			content := settingsOf(stored)
			content.SecurityKeyFile = c.key
			content.LoggingFilePath = c.log
			content.MonitoringIntervalSec = 47

			file, err := source.handler.seal(transferKindSettings, content, testExportPassword, time.Now())
			if err != nil {
				t.Fatalf("failed to seal a file: %v", err)
			}

			rec := target.call(t, target.handler.ImportSettings,
				`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != errImportSettingsRefused {
				t.Errorf("error_code = %q, want %q", code, errImportSettingsRefused)
			}

			after, err := settings.Load(target.db)
			if err != nil {
				t.Fatalf("failed to read the settings: %v", err)
			}
			if after.MonitoringIntervalSec == 47 {
				t.Errorf("a refused import stored the rest of the file")
			}
		})
	}
}

// TestAPathOutsideTheInstallationIsStoredAsTheDefault is what keeps an older
// file importable at all.
//
// The encryption key file and the log file used to be free paths, and every
// installation set up before they were held inside the installation directory
// has an absolute one, which is what its export carries. Held to the rule the
// way every other setting is, such a file would be refused whole: the Hosts,
// the intervals and the language would move nowhere because of where a log
// file used to go, and moving a configuration is the one thing this call is
// for. So the two paths are dropped to their defaults, the way the startup
// repairs a stored one, and the rest of the file is stored.
func TestAPathOutsideTheInstallationIsStoredAsTheDefault(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	// The file is built rather than exported. An installation running on these
	// paths is one from before the rule, and this one cannot be made to store
	// them any more.
	outside := platformAbsolutePath("/var/lib/tunnel-manager/tunnel-manager.key")

	content := settingsOf(stored)
	content.SecurityKeyFile = outside
	content.LoggingFilePath = "../../etc/cron.d/tunnel-manager"
	content.MonitoringIntervalSec = 47

	file, err := source.handler.seal(transferKindSettings, content, testExportPassword, time.Now())
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var imported importedSettings

	decodeTransfer(t, rec).into(t, &imported)

	defaults := settings.Defaults()

	after, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.SecurityKeyFile != defaults.SecurityKeyFile {
		t.Errorf("security.key_file was stored as %q, want the default %q",
			after.SecurityKeyFile, defaults.SecurityKeyFile)
	}
	if after.LoggingFilePath != defaults.LoggingFilePath {
		t.Errorf("logging.file.path was stored as %q, want the default %q",
			after.LoggingFilePath, defaults.LoggingFilePath)
	}

	// The answer says what is stored, so it carries the defaults too and not
	// the paths that were asked for.
	if imported.Settings.SecurityKeyFile != defaults.SecurityKeyFile ||
		imported.Settings.LoggingFilePath != defaults.LoggingFilePath {
		t.Errorf("the answer carries the paths of the file: %q and %q",
			imported.Settings.SecurityKeyFile, imported.Settings.LoggingFilePath)
	}

	// The rest of the file landed. A file that is taken in for the settings it
	// names and silently loses them is no better than the refusal.
	if after.MonitoringIntervalSec != 47 {
		t.Errorf("monitoring.interval_sec = %d, want the 47 the file names", after.MonitoringIntervalSec)
	}

	// What was dropped is in the answer, with the path that is gone in it. The
	// operator is standing in front of the screen that asked for the import,
	// and the key file that was named is where the secrets of the other
	// installation are unsealed from.
	got := map[string]transferItem{}
	for _, item := range imported.Items {
		got[item.Name] = item
	}

	if len(imported.Items) != 2 {
		t.Fatalf("the answer lists %d dropped paths, want both: %+v", len(imported.Items), imported.Items)
	}

	for name, carried := range map[string]string{
		"security.key_file": outside,
		"logging.file.path": "../../etc/cron.d/tunnel-manager",
	} {
		item, found := got[name]
		if !found {
			t.Errorf("%s is not in the answer: %+v", name, imported.Items)
			continue
		}

		if item.Kind != "setting" || item.Action != transferSkipped {
			t.Errorf("%s is reported as %q %q, want a skipped setting", name, item.Kind, item.Action)
		}
		if !strings.Contains(item.Reason, carried) {
			t.Errorf("the reason for %s does not name the path that was dropped: %q", name, item.Reason)
		}
	}

	// It is in the log as well, under the id the startup writes a repaired
	// path under, because the answer is read once and the log is what is left
	// afterwards.
	dropped := target.logs.FilterField(zap.String(logid.FieldKey,
		string(logid.SettingsSettingPutBackToDefault)))
	if dropped.Len() != 2 {
		t.Errorf("the log holds %d lines about a dropped path, want 2", dropped.Len())
	}
}

// TestAPathInsideTheInstallationIsImportedAsItIs is the other half: the
// dropping is for the paths the rule refuses and for nothing else, so a file
// that names a place under the installation directory is stored as it is
// rather than being flattened onto the default.
func TestAPathInsideTheInstallationIsImportedAsItIs(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored.SecurityKeyFile = "secrets/tunnel-manager.key"
	stored.LoggingFilePath = "logs/tunnel-manager.log"

	err = settings.Save(source.db, stored, source.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	file := source.exportSettings(t, testExportPassword)

	rec := target.call(t, target.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var imported importedSettings

	decodeTransfer(t, rec).into(t, &imported)

	if len(imported.Items) != 0 {
		t.Errorf("the answer drops a path the rules take: %+v", imported.Items)
	}

	after, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.SecurityKeyFile != "secrets/tunnel-manager.key" {
		t.Errorf("security.key_file = %q, want the path the file names", after.SecurityKeyFile)
	}
	if after.LoggingFilePath != "logs/tunnel-manager.log" {
		t.Errorf("logging.file.path = %q, want the path the file names", after.LoggingFilePath)
	}
}

// TestAFileFromALaterFormatIsRefused keeps this version from reading a layout
// it does not know as far as it happens to parse.
func TestAFileFromALaterFormatIsRefused(t *testing.T) {
	target := newTransferInstall(t)

	later, err := json.Marshal(transferFile{
		Kind:          transferKindTunnels,
		FormatVersion: transferFormatVersion + 1,
		ExportedBy:    "99.0.0",
		Content:       json.RawMessage(`{"hosts":[]}`),
	})
	if err != nil {
		t.Fatalf("failed to build a file: %v", err)
	}

	sealed, err := crypto.EncryptWithPassword(string(later), testExportPassword)
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec := target.importTunnels(t, sealed, testExportPassword)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the import answered %d, want %d", rec.Code, http.StatusBadRequest)
	}

	message := decodeTransfer(t, rec).Error
	if !strings.Contains(message, "99.0.0") || !strings.Contains(message, "newer version") {
		t.Errorf("the refusal does not say where the file came from: %q", message)
	}
}

// TestNoSecretIsWrittenToTheLogOrTheAnswer is the other half of the rule that
// the file is the only place the secrets are in the clear. The log is kept and
// rotated and is read by more people than hold the password of the file.
func TestNoSecretIsWrittenToTheLogOrTheAnswer(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	secrets := map[string]string{
		"the SSH password":     withPassword.Password,
		"the private key":      strings.Split(strings.TrimSpace(withKey.PrivateKey), "\n")[1],
		"the key passphrase":   withKey.KeyPassphrase,
		"the sealing password": testExportPassword,
		"the account password": testPassword,
	}

	// The answer of the import says what was written and nothing of what is in
	// the rows. The answer of the export carries the file, which is sealed, so
	// it is read for the secrets in the clear rather than left out.
	answers := map[string]string{
		"the answer of the import": rec.Body.String(),
		"the answer of the export": source.call(t, source.handler.ExportTunnels,
			exportBody(t, testExportPassword)).Body.String(),
	}

	for where, text := range answers {
		for what, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s carries %s", where, what)
			}
		}
	}

	for _, where := range []struct {
		what string
		logs *observer.ObservedLogs
	}{{"the log of the export", source.logs}, {"the log of the import", target.logs}} {
		var written strings.Builder

		for _, entry := range where.logs.All() {
			written.WriteString(entry.Message)

			for _, field := range entry.Context {
				written.WriteString(" ")
				written.WriteString(field.String)
			}
		}

		for what, secret := range secrets {
			if strings.Contains(written.String(), secret) {
				t.Errorf("%s carries %s", where.what, what)
			}
		}
	}

	// The two facts worth keeping are kept: that it happened and how much of it.
	if source.logs.FilterMessage("exported the tunnel configuration").Len() != 2 {
		t.Errorf("the exports were not logged")
	}

	if target.logs.FilterMessage("imported a tunnel configuration").Len() != 1 {
		t.Errorf("the import was not logged")
	}
}

// TestAnImportedHostThatWasDisabledStaysDisabled pins down that a Host carried
// in a file as disabled arrives disabled.
//
// It did not. The column held a database default of true, and gorm leaves a
// field out of an insert when it holds the zero value and the column has a
// default, so every Host in a file arrived enabled whatever the file said. A
// Host that somebody turned off on one machine would start connecting from the
// moment it landed on another, which is the last thing an import should do on
// its own.
func TestAnImportedHostThatWasDisabledStaysDisabled(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	off := hostContent{
		Address:     "192.0.2.30",
		Port:        22,
		User:        "operator",
		Password:    "the password of the Host",
		Description: "the Host somebody turned off",
		Enabled:     false,
	}
	on := hostContent{
		Address:     "192.0.2.31",
		Port:        22,
		User:        "operator",
		Password:    "the password of the Host",
		Description: "the Host that is in use",
		Enabled:     true,
	}

	source.registerHost(t, off)
	source.registerHost(t, on)

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	for _, want := range []hostContent{off, on} {
		var stored models.Host

		err := target.db.Where("address = ?", want.Address).First(&stored).Error
		if err != nil {
			t.Fatalf("the Host %s did not arrive: %v", want.Address, err)
		}
		if stored.Enabled != want.Enabled {
			t.Errorf("the Host %s arrived with enabled %v, want %v",
				want.Address, stored.Enabled, want.Enabled)
		}
	}
}

// testTrustedHostKey and testPresentedHostKey are two keys of the form a Host
// row holds one in, "<algorithm> <base64>". Nothing in these tests speaks SSH,
// so what matters about them is that they are two different strings that look
// like what the tunnels write.
const (
	testTrustedHostKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHRoZVRydXN0ZWRLZXlPZlRoZVNlcnZlcg"
	testPresentedHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHRoZVByZXNlbnRlZEtleU9mQVNlcnZlcg"
)

// presentedAKeyNobodyHasApproved writes onto a stored Host what a server
// presented on a connection that was refused, which is what the tunnels do
// when the key they are offered is not the one the Host is trusted on.
func (i *transferInstall) presentedAKeyNobodyHasApproved(t *testing.T, hostIP string, key string) {
	t.Helper()

	err := i.db.Model(&models.Host{}).Where("address = ?", hostIP).
		Update("pending_host_key", key).Error
	if err != nil {
		t.Fatalf("failed to store the pending key of the Host %s: %v", hostIP, err)
	}
}

// TestBothHostKeysOfAHostComeAcross is the round trip of the two keys of a
// Host. The file puts back the configuration as it was, so the key waiting for
// approval comes across with the one the Host is trusted on, and an import over
// a Host waiting on another key leaves the one of the file.
func TestBothHostKeysOfAHostComeAcross(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, _ := twoHosts(t)
	withKey.HostKey = testTrustedHostKey

	source.registerHost(t, withKey)
	source.presentedAKeyNobodyHasApproved(t, withKey.Address, testPresentedHostKey)

	here := withKey
	here.HostKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCtheKeyThisInstallationTrusted"
	target.registerHost(t, here)
	target.presentedAKeyNobodyHasApproved(t, here.Address, "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCaKeyPresentedHere")

	file := source.exportTunnels(t, testExportPassword)

	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the exported file does not open: %v", err)
	}

	if !strings.Contains(opened, `"host_key":"`+testTrustedHostKey+`","pending_host_key":"`+testPresentedHostKey+`"`) {
		t.Errorf("the file does not carry both keys of the Host: %s", opened)
	}

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var stored models.Host

	err = target.db.Where("address = ?", withKey.Address).First(&stored).Error
	if err != nil {
		t.Fatalf("the Host %s did not arrive: %v", withKey.Address, err)
	}

	if stored.HostKey != testTrustedHostKey || stored.PendingHostKey != testPresentedHostKey {
		t.Errorf("the Host arrived trusted on %q and waiting on %q, want %q and %q",
			stored.HostKey, stored.PendingHostKey, testTrustedHostKey, testPresentedHostKey)
	}
}

// assign makes a Host carry a service port, the way an installation stores it:
// one row holding the two ids of this installation.
func (i *transferInstall) assign(t *testing.T, hostIP string, localPort int) {
	t.Helper()

	var host models.Host

	err := i.db.Where("address = ?", hostIP).First(&host).Error
	if err != nil {
		t.Fatalf("the Host %s is not registered here: %v", hostIP, err)
	}

	var sp models.ServicePort

	err = i.db.Where("local_port = ?", localPort).First(&sp).Error
	if err != nil {
		t.Fatalf("no service port is on the local port %d here: %v", localPort, err)
	}

	err = i.db.Create(&models.HostServicePort{HostID: host.ID, SPID: sp.ID, Enabled: true}).Error
	if err != nil {
		t.Fatalf("failed to store the assignment: %v", err)
	}
}

// openTo moves one stored assignment of this installation to a bind scope, the
// way the assignment screen of a Host does. It is written here rather than
// given to assign, because most of these tests are about what a Host carries
// and not about how far it reaches, and those read better with the scopes left
// where every installation starts: on the wildcard.
func (i *transferInstall) openTo(t *testing.T, hostIP string, localPort int, scope string) {
	t.Helper()

	var host models.Host

	err := i.db.Where("address = ?", hostIP).First(&host).Error
	if err != nil {
		t.Fatalf("the Host %s is not registered here: %v", hostIP, err)
	}

	var sp models.ServicePort

	err = i.db.Where("local_port = ?", localPort).First(&sp).Error
	if err != nil {
		t.Fatalf("no service port is on the local port %d here: %v", localPort, err)
	}

	result := i.db.Model(&models.HostServicePort{}).
		Where("host_id = ? AND sp_id = ?", host.ID, sp.ID).Update("bind_scope", scope)
	if result.Error != nil {
		t.Fatalf("failed to open the assignment to %s: %v", scope, result.Error)
	}

	if result.RowsAffected != 1 {
		t.Fatalf("the assignment of %s to %d is not stored here", hostIP, localPort)
	}
}

// carriedScopes is every assignment stored with what it is opened to, as
// "<Host IP> carries <local port> on <scope>", and an assignment on the
// wildcard by the empty column reads as "on ". The pairs are named the way
// carried names them, for the same reason.
func (i *transferInstall) carriedScopes(t *testing.T) []string {
	t.Helper()

	var rows []struct {
		Address   string
		LocalPort int
		BindScope string
	}

	err := i.db.Model(&models.HostServicePort{}).
		Select("hosts.address AS address, service_ports.local_port AS local_port, " +
			"host_service_ports.bind_scope AS bind_scope").
		Joins("JOIN hosts ON hosts.id = host_service_ports.host_id").
		Joins("JOIN service_ports ON service_ports.id = host_service_ports.sp_id").
		Order("hosts.address, service_ports.local_port").
		Scan(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the assignments: %v", err)
	}

	pairs := make([]string, 0, len(rows))
	for _, row := range rows {
		pairs = append(pairs, row.Address+" carries "+strconv.Itoa(row.LocalPort)+" on "+row.BindScope)
	}

	return pairs
}

// carried is every assignment stored, as "<Host IP> carries <local port>". The
// ids differ between two installations and say nothing to whoever reads a
// failure, so the pairs are named by what means the same on both sides, which
// is what the file carries them as.
func (i *transferInstall) carried(t *testing.T) []string {
	t.Helper()

	var rows []struct {
		Address   string
		LocalPort int
	}

	err := i.db.Model(&models.HostServicePort{}).
		Select("hosts.address AS address, service_ports.local_port AS local_port").
		Joins("JOIN hosts ON hosts.id = host_service_ports.host_id").
		Joins("JOIN service_ports ON service_ports.id = host_service_ports.sp_id").
		Order("hosts.address, service_ports.local_port").
		Scan(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the assignments: %v", err)
	}

	pairs := make([]string, 0, len(rows))
	for _, row := range rows {
		pairs = append(pairs, row.Address+" carries "+strconv.Itoa(row.LocalPort))
	}

	return pairs
}

// rewriteHosts opens a file, hands every Host in it over as the JSON object it
// is, and seals what comes back with the same password. It is how a file no
// version of the export writes is made: one from before the assignments were
// stored, and one naming a service port that is not registered here.
func rewriteHosts(t *testing.T, file string, password string, change func(host map[string]interface{})) string {
	t.Helper()

	plaintext, err := crypto.DecryptWithPassword(file, password)
	if err != nil {
		t.Fatalf("failed to open the file: %v", err)
	}

	var read transferFile

	err = json.Unmarshal([]byte(plaintext), &read)
	if err != nil {
		t.Fatalf("failed to read the file: %v", err)
	}

	var content map[string]interface{}

	err = json.Unmarshal(read.Content, &content)
	if err != nil {
		t.Fatalf("failed to read the content of the file: %v", err)
	}

	hosts, ok := content["hosts"].([]interface{})
	if !ok {
		t.Fatalf("the file carries no list of Hosts")
	}

	for _, entry := range hosts {
		host, ok := entry.(map[string]interface{})
		if !ok {
			t.Fatalf("a Host in the file is not an object")
		}

		change(host)
	}

	body, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("failed to write the content back: %v", err)
	}

	read.Content = body

	rewritten, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("failed to write the file back: %v", err)
	}

	sealed, err := crypto.EncryptWithPassword(string(rewritten), password)
	if err != nil {
		t.Fatalf("failed to seal the file again: %v", err)
	}

	return sealed
}

// threeServicePorts is what the assignment tests hold: three service ports, so
// that a Host carrying some of them is told from a Host carrying all of them.
func threeServicePorts(t *testing.T, install *transferInstall) {
	t.Helper()

	for _, sp := range []servicePortContent{
		{ServiceAddress: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "the first service"},
		{ServiceAddress: "192.0.2.21", ServicePort: 443, LocalPort: 18081, Description: "the second service"},
		{ServiceAddress: "192.0.2.22", ServicePort: 5432, LocalPort: 18082, Description: "the third service"},
	} {
		install.registerServicePort(t, sp)
	}
}

// partlyAssigned is an installation with two Hosts, three service ports and
// four of the six assignments: neither Host carries everything and neither
// carries nothing, so a file that dropped the assignments and one that filled
// them in both fail here.
func partlyAssigned(t *testing.T) *transferInstall {
	t.Helper()

	source := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)
	threeServicePorts(t, source)

	source.assign(t, withKey.Address, 18080)
	source.assign(t, withKey.Address, 18081)
	source.assign(t, withPassword.Address, 18081)
	source.assign(t, withPassword.Address, 18082)

	return source
}

// fourPairs is what partlyAssigned holds, and what an installation that took
// its file in has to hold as well.
var fourPairs = []string{
	"192.0.2.10 carries 18080",
	"192.0.2.10 carries 18081",
	"192.0.2.11 carries 18081",
	"192.0.2.11 carries 18082",
}

// everyPair is all six: both Hosts carrying all three service ports.
var everyPair = []string{
	"192.0.2.10 carries 18080",
	"192.0.2.10 carries 18081",
	"192.0.2.10 carries 18082",
	"192.0.2.11 carries 18080",
	"192.0.2.11 carries 18081",
	"192.0.2.11 carries 18082",
}

func TestTheServicePortsAHostCarriesCrossToAnotherInstallation(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	if !reflect.DeepEqual(source.carried(t), fourPairs) {
		t.Fatalf("the installation that is exported carries %v, want %v", source.carried(t), fourPairs)
	}

	file := source.exportTunnels(t, testExportPassword)

	// The two installations must not share a key, for the reason the test that
	// moves the secrets across says: with one key the trip is not made.
	sealedHere, err := source.cipher.Encrypt("a value")
	if err != nil {
		t.Fatalf("failed to seal a value: %v", err)
	}

	_, err = target.cipher.Decrypt(sealedHere)
	if err == nil {
		t.Fatalf("the two installations were built with the same encryption key")
	}

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.carried(t), fourPairs) {
		t.Fatalf("the installation that took the file in carries %v, want %v",
			target.carried(t), fourPairs)
	}

	// The file names them by the id of the service port, which the import
	// stores the service ports under as well, and no longer by the local port.
	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if !strings.Contains(opened, `"assignments":[{"service_port_id":1,`) {
		t.Errorf("the file does not name the assignments by the id of the service port: %s", opened)
	}

	if strings.Contains(opened, `"assigned_local_ports"`) {
		t.Errorf("the file names the assignments by their local port as well: %s", opened)
	}
}

func TestHowFarEachAssignmentReachesCrossesToAnotherInstallation(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	source.openTo(t, "192.0.2.10", 18080, models.BindScopeLoopback)
	source.openTo(t, "192.0.2.11", 18082, models.BindScopeWildcard)

	want := []string{
		"192.0.2.10 carries 18080 on " + models.BindScopeLoopback,
		"192.0.2.10 carries 18081 on ",
		"192.0.2.11 carries 18081 on ",
		"192.0.2.11 carries 18082 on " + models.BindScopeWildcard,
	}

	if !reflect.DeepEqual(source.carriedScopes(t), want) {
		t.Fatalf("the installation that is exported holds %v, want %v", source.carriedScopes(t), want)
	}

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.carriedScopes(t), want) {
		t.Fatalf("the installation that took the file in holds %v, want %v",
			target.carriedScopes(t), want)
	}

	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if !strings.Contains(opened, `{"service_port_id":1,"bind_scope":"loopback","enabled":true,`) {
		t.Errorf("the file does not name what the assignments are opened to: %s", opened)
	}

	// The file of the release before the scopes held one address for the whole
	// Host. Writing it again would be an answer this version does not keep,
	// read back by nothing.
	if strings.Contains(opened, `"bind_address"`) || strings.Contains(opened, `"assigned_bind_scopes"`) {
		t.Errorf("the file carries the scopes the way an older release wrote them: %s", opened)
	}
}

// switchOff pauses one stored assignment of this installation, the way the
// assignment screen of a Host does.
func (i *transferInstall) switchOff(t *testing.T, hostIP string, localPort int) {
	t.Helper()

	var host models.Host

	err := i.db.Where("address = ?", hostIP).First(&host).Error
	if err != nil {
		t.Fatalf("the Host %s is not registered here: %v", hostIP, err)
	}

	var sp models.ServicePort

	err = i.db.Where("local_port = ?", localPort).First(&sp).Error
	if err != nil {
		t.Fatalf("no service port is on the local port %d here: %v", localPort, err)
	}

	result := i.db.Model(&models.HostServicePort{}).
		Where("host_id = ? AND sp_id = ?", host.ID, sp.ID).Update("enabled", false)
	if result.Error != nil {
		t.Fatalf("failed to switch the assignment off: %v", result.Error)
	}

	if result.RowsAffected != 1 {
		t.Fatalf("the assignment of %s to %d is not stored here", hostIP, localPort)
	}
}

// carriedRunning is every assignment stored with whether it runs, as "<Host
// IP> carries <local port> on" or "off", named the way carried names them.
func (i *transferInstall) carriedRunning(t *testing.T) []string {
	t.Helper()

	var rows []struct {
		Address   string
		LocalPort int
		Enabled   bool
	}

	err := i.db.Model(&models.HostServicePort{}).
		Select("hosts.address AS address, service_ports.local_port AS local_port, " +
			"host_service_ports.enabled AS enabled").
		Joins("JOIN hosts ON hosts.id = host_service_ports.host_id").
		Joins("JOIN service_ports ON service_ports.id = host_service_ports.sp_id").
		Order("hosts.address, service_ports.local_port").
		Scan(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the assignments: %v", err)
	}

	pairs := make([]string, 0, len(rows))
	for _, row := range rows {
		state := "off"
		if row.Enabled {
			state = "on"
		}

		pairs = append(pairs, row.Address+" carries "+strconv.Itoa(row.LocalPort)+" "+state)
	}

	return pairs
}

func TestWhetherEachAssignmentRunsCrossesToAnotherInstallation(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	source.switchOff(t, "192.0.2.10", 18081)

	want := []string{
		"192.0.2.10 carries 18080 on",
		"192.0.2.10 carries 18081 off",
		"192.0.2.11 carries 18081 on",
		"192.0.2.11 carries 18082 on",
	}

	if !reflect.DeepEqual(source.carriedRunning(t), want) {
		t.Fatalf("the installation that is exported holds %v, want %v", source.carriedRunning(t), want)
	}

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.carriedRunning(t), want) {
		t.Fatalf("the installation that took the file in holds %v, want %v", target.carriedRunning(t), want)
	}

	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if !strings.Contains(opened, `{"service_port_id":2,"bind_scope":"","enabled":false,`) {
		t.Errorf("the file does not say that the paused assignment is off: %s", opened)
	}

	if strings.Contains(opened, `"assigned_enabled"`) {
		t.Errorf("the file says whether an assignment runs the way an older release wrote it: %s", opened)
	}
}

func TestAFileFromBeforeAssignmentsCouldBeSwitchedOffRunsEveryOne(t *testing.T) {
	for _, tt := range []struct {
		name   string
		strip  []string
		expect []string
	}{
		{
			name:  "the assignments named without whether they run",
			strip: []string{"assigned_enabled"},
			expect: []string{
				"192.0.2.10 carries 18080 on",
				"192.0.2.10 carries 18081 on",
				"192.0.2.11 carries 18081 on",
				"192.0.2.11 carries 18082 on",
			},
		},
		{
			name:  "the assignments not named at all",
			strip: []string{"assigned_enabled", "assigned_local_ports"},
			expect: []string{
				"192.0.2.10 carries 18080 on",
				"192.0.2.10 carries 18081 on",
				"192.0.2.10 carries 18082 on",
				"192.0.2.11 carries 18080 on",
				"192.0.2.11 carries 18081 on",
				"192.0.2.11 carries 18082 on",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := partlyAssigned(t)
			target := newTransferInstall(t)

			source.switchOff(t, "192.0.2.10", 18081)

			older := rewriteHosts(t, formatOne(t, source.exportTunnels(t, testExportPassword), testExportPassword),
				testExportPassword, func(host map[string]interface{}) {
					for _, field := range tt.strip {
						delete(host, field)
					}
				})

			rec := target.importTunnels(t, older, testExportPassword)
			if rec.Code != http.StatusOK {
				t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
			}

			if !reflect.DeepEqual(target.carriedRunning(t), tt.expect) {
				t.Fatalf("the file left the installation holding %v, want %v",
					target.carriedRunning(t), tt.expect)
			}
		})
	}
}

func TestAFileFromWhenTheHostHeldTheAddressOpensItsAssignmentsThere(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	older := rewriteHosts(t, formatOne(t, source.exportTunnels(t, testExportPassword), testExportPassword),
		testExportPassword, func(host map[string]interface{}) {
			delete(host, "assigned_bind_scopes")

			if host["address"] == "192.0.2.10" {
				host["bind_address"] = "127.0.0.1"

				return
			}

			host["bind_address"] = "192.0.2.11"
		})

	rec := target.importTunnels(t, older, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := []string{
		"192.0.2.10 carries 18080 on " + models.BindScopeLoopback,
		"192.0.2.10 carries 18081 on " + models.BindScopeLoopback,
		"192.0.2.11 carries 18081 on ",
		"192.0.2.11 carries 18082 on ",
	}

	if !reflect.DeepEqual(target.carriedScopes(t), want) {
		t.Fatalf("a file that held the address on the Host left the installation holding %v, want %v",
			target.carriedScopes(t), want)
	}
}

func TestAFileNamingAScopeThatIsNeitherIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name  string
		write func(t *testing.T, file string) string
	}{
		{"by the id of the service port", func(t *testing.T, file string) string {
			return rewriteHosts(t, file, testExportPassword, func(host map[string]interface{}) {
				if host["address"] != "192.0.2.10" {
					return
				}

				assignments, ok := host["assignments"].([]interface{})
				if !ok || len(assignments) == 0 {
					t.Fatalf("the exported file does not name the assignments of a Host")
				}

				assignments[0].(map[string]interface{})["bind_scope"] = "everywhere"
			})
		}},
		{"by the local port", func(t *testing.T, file string) string {
			return rewriteHosts(t, formatOne(t, file, testExportPassword), testExportPassword,
				func(host map[string]interface{}) {
					if host["address"] != "192.0.2.10" {
						return
					}

					host["assigned_bind_scopes"] = map[string]interface{}{"18080": "everywhere"}
				})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := partlyAssigned(t)
			target := newTransferInstall(t)

			byHand := tt.write(t, source.exportTunnels(t, testExportPassword))

			rec := target.importTunnels(t, byHand, testExportPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest,
					rec.Body.String())
			}

			if errorCodeOf(t, rec) != errImportHostRefused {
				t.Errorf("the import was refused under %s, want %s", errorCodeOf(t, rec), errImportHostRefused)
			}

			if target.count(t, &models.Host{}) != 0 || target.count(t, &models.ServicePort{}) != 0 {
				t.Fatalf("the refused file left %d Hosts and %d service ports, want none of either",
					target.count(t, &models.Host{}), target.count(t, &models.ServicePort{}))
			}
		})
	}
}

func TestAFileFromBeforeTheAssignmentsWereStoredCarriesEverything(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	older := rewriteHosts(t, formatOne(t, source.exportTunnels(t, testExportPassword), testExportPassword),
		testExportPassword, func(host map[string]interface{}) {
			delete(host, "assigned_local_ports")
		})

	rec := target.importTunnels(t, older, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.carried(t), everyPair) {
		t.Fatalf("a file that does not name the assignments left the installation carrying %v, want %v",
			target.carried(t), everyPair)
	}
}

func TestAHostThatIsCarriedAsCarryingNothingCarriesNothing(t *testing.T) {
	for _, tt := range []struct {
		name  string
		write func(t *testing.T, file string) string
	}{
		{"a file with ids and no assignments", func(t *testing.T, file string) string {
			return rewriteHosts(t, file, testExportPassword, func(host map[string]interface{}) {
				delete(host, "assignments")
			})
		}},
		{"a file without ids and an empty list", func(t *testing.T, file string) string {
			return rewriteHosts(t, formatOne(t, file, testExportPassword), testExportPassword,
				func(host map[string]interface{}) {
					host["assigned_local_ports"] = []interface{}{}
				})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := partlyAssigned(t)
			target := newTransferInstall(t)

			emptied := tt.write(t, source.exportTunnels(t, testExportPassword))

			rec := target.importTunnels(t, emptied, testExportPassword)
			if rec.Code != http.StatusOK {
				t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
			}

			if len(target.carried(t)) != 0 {
				t.Fatalf("a file whose Hosts carry nothing left the installation carrying %v",
					target.carried(t))
			}

			// The Hosts and the service ports themselves came across all the
			// same. An empty list is about what a Host carries and about
			// nothing else.
			if target.count(t, &models.Host{}) != 2 || target.count(t, &models.ServicePort{}) != 3 {
				t.Fatalf("the file left %d Hosts and %d service ports, want 2 and 3",
					target.count(t, &models.Host{}), target.count(t, &models.ServicePort{}))
			}
		})
	}
}

// TestAnAssignmentToAServicePortTheFileDoesNotHoldIsRefused is a Host of the
// file carrying a service port the file does not hold. Every service port
// stored after the import is one of the file, so there is nothing to point
// the assignment at, and the file is refused with the Host named.
func TestAnAssignmentToAServicePortTheFileDoesNotHoldIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name  string
		write func(t *testing.T, file string) string
		code  errorCode
		arg   string
		value string
	}{
		{"by the id of the service port", func(t *testing.T, file string) string {
			return rewriteHosts(t, file, testExportPassword, func(host map[string]interface{}) {
				assignments, _ := host["assignments"].([]interface{})
				host["assignments"] = append(assignments,
					map[string]interface{}{"service_port_id": 99, "bind_scope": "", "enabled": true})
			})
		}, errImportAssignmentUnknownServicePort, "service_port_id", "99"},
		{"by the local port", func(t *testing.T, file string) string {
			return rewriteHosts(t, formatOne(t, file, testExportPassword), testExportPassword,
				func(host map[string]interface{}) {
					ports, ok := host["assigned_local_ports"].([]interface{})
					if !ok {
						t.Fatalf("the older file does not name the assignments of a Host")
					}

					host["assigned_local_ports"] = append(ports, float64(19999))
				})
		}, errImportAssignmentUnknownLocalPort, "local_port", "19999"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := partlyAssigned(t)
			target := newTransferInstall(t)

			target.registerHost(t, passwordHost("192.0.2.99"))

			before := target.configuration(t)

			rec := target.importTunnels(t, tt.write(t, source.exportTunnels(t, testExportPassword)), testExportPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			var refused struct {
				Code errorCode         `json:"error_code"`
				Args map[string]string `json:"error_args"`
			}

			if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
				t.Fatalf("the refusal is not JSON: %v", err)
			}

			if refused.Code != tt.code || refused.Args["host"] != "192.0.2.10" || refused.Args[tt.arg] != tt.value {
				t.Errorf("the import was refused under %s with %v, want %s naming 192.0.2.10 and %s %s",
					refused.Code, refused.Args, tt.code, tt.arg, tt.value)
			}

			if !reflect.DeepEqual(target.configuration(t), before) {
				t.Fatalf("the refused import changed what is stored")
			}
		})
	}
}

// TestTheLocalForwardContentCarriesEveryFieldOfALocalForward does for a local
// forward what the tests above do for a Host and a service port. The Host is
// left out: it is the one the forward is written under in the file.
func TestTheLocalForwardContentCarriesEveryFieldOfALocalForward(t *testing.T) {
	left := map[string]bool{"HostID": true}

	stored := reflect.TypeOf(models.LocalForward{})
	carried := reflect.TypeOf(localForwardContent{})

	for i := 0; i < stored.NumField(); i++ {
		name := stored.Field(i).Name
		if left[name] {
			continue
		}

		_, found := carried.FieldByName(name)
		if !found {
			t.Errorf("models.LocalForward has %s and localForwardContent does not, "+
				"so it is not carried by an export", name)
		}
	}
}

// passwordHost is a Host logged in to with a password, which is all the local
// forward tests need of one.
func passwordHost(ip string) hostContent {
	return hostContent{
		Address:     ip,
		Port:        22,
		User:        "operator",
		Password:    "the password of the Host",
		Description: "the Host " + ip,
		Enabled:     true,
	}
}

// forward stores one local forward on a registered Host.
func (i *transferInstall) forward(t *testing.T, hostIP string, lf localForwardContent) {
	t.Helper()

	var host models.Host

	err := i.db.Where("address = ?", hostIP).First(&host).Error
	if err != nil {
		t.Fatalf("the Host %s is not registered here: %v", hostIP, err)
	}

	number, err := nextLocalForwardNumber(i.db, host.ID)
	if err != nil {
		t.Fatalf("failed to read the numbers of the local forwards of %s: %v", hostIP, err)
	}

	err = i.db.Create(&models.LocalForward{
		HostID:         host.ID,
		Number:         number,
		BindScope:      lf.BindScope,
		LocalPort:      lf.LocalPort,
		TargetAddress:  lf.TargetAddress,
		TargetPort:     lf.TargetPort,
		Description:    lf.Description,
		AllowedSources: lf.AllowedSources,
		Enabled:        lf.enabled(),
	}).Error
	if err != nil {
		t.Fatalf("failed to store the local forward: %v", err)
	}
}

// forwardsEnabled is whether each stored local forward is switched on, keyed
// by its local port.
func (i *transferInstall) forwardsEnabled(t *testing.T) map[int]bool {
	t.Helper()

	var rows []models.LocalForward

	err := i.db.Find(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the local forwards: %v", err)
	}

	enabled := make(map[int]bool, len(rows))
	for _, row := range rows {
		enabled[row.LocalPort] = row.Enabled
	}

	return enabled
}

// forwards is every local forward stored, named by what means the same on both
// installations: "<Host IP> <scope> <local port> -> <target> (<description>)".
func (i *transferInstall) forwards(t *testing.T) []string {
	t.Helper()

	var rows []struct {
		Address       string
		BindScope     string
		LocalPort     int
		TargetAddress string
		TargetPort    int
		Description   string
	}

	err := i.db.Model(&models.LocalForward{}).
		Select("hosts.address AS address, local_forwards.bind_scope AS bind_scope, " +
			"local_forwards.local_port AS local_port, local_forwards.target_address AS target_address, " +
			"local_forwards.target_port AS target_port, local_forwards.description AS description").
		Joins("JOIN hosts ON hosts.id = local_forwards.host_id").
		Order("local_forwards.local_port").
		Scan(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the local forwards: %v", err)
	}

	named := make([]string, 0, len(rows))
	for _, row := range rows {
		named = append(named, row.Address+" "+row.BindScope+" "+strconv.Itoa(row.LocalPort)+" -> "+
			row.TargetAddress+":"+strconv.Itoa(row.TargetPort)+" ("+row.Description+")")
	}

	return named
}

// errorCodeOf reads the code a refusal was answered under.
func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) errorCode {
	t.Helper()

	var body errorBody

	err := json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the refusal: %v, body: %s", err, rec.Body.String())
	}

	return body.Code
}

// TestTheLocalForwardsOfAHostCrossToAnotherInstallation is the round trip: what
// an installation forwards is what the one that took the file in forwards, on
// the same Host under the same number, and an empty scope is stored as it was.
func TestTheLocalForwardsOfAHostCrossToAnotherInstallation(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	source.registerHost(t, passwordHost("192.0.2.10"))
	source.registerHost(t, passwordHost("192.0.2.11"))
	source.forward(t, "192.0.2.10", localForwardContent{BindScope: models.BindScopeLoopback,
		LocalPort: 15433, TargetAddress: "127.0.0.1", TargetPort: 5432, Description: "the database"})
	source.forward(t, "192.0.2.10", localForwardContent{BindScope: models.BindScopeWildcard,
		LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 5432})
	source.forward(t, "192.0.2.11", localForwardContent{
		LocalPort: 18443, TargetAddress: "192.0.2.31", TargetPort: 443, Description: "no scope"})

	rec := source.call(t, source.handler.ExportTunnels, exportBody(t, testExportPassword))
	if rec.Code != http.StatusOK {
		t.Fatalf("the export answered %d: %s", rec.Code, rec.Body.String())
	}

	var exported exportedTunnels

	decodeTransfer(t, rec).into(t, &exported)

	if exported.LocalForwards != 3 {
		t.Errorf("the export counts %d local forwards, want 3", exported.LocalForwards)
	}

	opened, err := crypto.DecryptWithPassword(exported.File, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	// In the order of the number, which is the order they were added in.
	if !strings.Contains(opened, `"local_forwards":[{"number":1,"bind_scope":"loopback","local_port":15433,`) {
		t.Errorf("the file does not carry the local forwards of the Host in the order of their number: %s", opened)
	}

	rec = target.importTunnels(t, exported.File, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	if imported.File.Hosts != 2 || imported.File.LocalForwards != 3 {
		t.Errorf("the import counts %+v, want the 2 Hosts and the 3 local forwards", imported.File)
	}

	want := []string{
		"192.0.2.10 wildcard 15432 -> 192.0.2.30:5432 ()",
		"192.0.2.10 loopback 15433 -> 127.0.0.1:5432 (the database)",
		"192.0.2.11  18443 -> 192.0.2.31:443 (no scope)",
	}

	if !reflect.DeepEqual(target.forwards(t), want) {
		t.Fatalf("the installation that took the file in forwards %v, want %v", target.forwards(t), want)
	}

	if !reflect.DeepEqual(target.configuration(t), source.configuration(t)) {
		t.Fatalf("the local forwards came across as\n%v\nwant\n%v", target.configuration(t), source.configuration(t))
	}

	if target.manager.count() != 1 {
		t.Errorf("the import asked for %d reconcile passes, want 1", target.manager.count())
	}

	// A file without the numbers, which is every file of the release before
	// them, is numbered from 1 in its order, and an empty scope in it is
	// stored as the wildcard it stands for, as it always was.
	fromOlder := newTransferInstall(t)

	rec = fromOlder.importTunnels(t, formatOne(t, exported.File, testExportPassword), testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import of the older file answered %d: %s", rec.Code, rec.Body.String())
	}

	want[2] = "192.0.2.11 wildcard 18443 -> 192.0.2.31:443 (no scope)"

	if !reflect.DeepEqual(fromOlder.forwards(t), want) {
		t.Fatalf("the installation that took the older file in forwards %v, want %v", fromOlder.forwards(t), want)
	}
}

// TestWhetherALocalForwardIsOnCrossesWithIt is the enabled flag on the round
// trip: a forward that is off arrives off and one that is on arrives on, and a
// file from before the flag, which carries none, arrives with every forward
// on, since every forward of such a file was running.
func TestWhetherALocalForwardIsOnCrossesWithIt(t *testing.T) {
	source := newTransferInstall(t)

	off := false

	source.registerHost(t, passwordHost("192.0.2.10"))
	source.forward(t, "192.0.2.10", localForwardContent{LocalPort: 15432, TargetAddress: "127.0.0.1", TargetPort: 5432,
		Enabled: &off})
	source.forward(t, "192.0.2.10", localForwardContent{LocalPort: 15433, TargetAddress: "127.0.0.1", TargetPort: 5433})

	file := source.exportTunnels(t, testExportPassword)

	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if !strings.Contains(opened, `"local_port":15432,"target_address":"127.0.0.1","target_port":5432,"description":"","enabled":false`) {
		t.Errorf("the file does not say the forward on 15432 is off: %s", opened)
	}

	target := newTransferInstall(t)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := map[int]bool{15432: false, 15433: true}
	if got := target.forwardsEnabled(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("the installation that took the file in holds %v, want %v", got, want)
	}

	older := rewriteHosts(t, file, testExportPassword, func(host map[string]interface{}) {
		forwards, ok := host["local_forwards"].([]interface{})
		if !ok {
			t.Fatalf("the Host carries no list of local forwards")
		}

		for _, entry := range forwards {
			forward, ok := entry.(map[string]interface{})
			if !ok {
				t.Fatalf("a local forward in the file is not an object")
			}

			delete(forward, "enabled")
		}
	})

	opened, err = crypto.DecryptWithPassword(older, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if strings.Contains(opened, `"enabled":false`) {
		t.Fatalf("the file still says a forward is off: %s", opened)
	}

	fromOlder := newTransferInstall(t)

	rec = fromOlder.importTunnels(t, older, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import of the older file answered %d: %s", rec.Code, rec.Body.String())
	}

	want = map[int]bool{15432: true, 15433: true}
	if got := fromOlder.forwardsEnabled(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("the installation that took the older file in holds %v, want %v", got, want)
	}
}

// TestAFileFromBeforeTheLocalForwardsWereStoredIsImported is a file with no
// local_forwards field at all, which is every file an earlier release wrote.
func TestAFileFromBeforeTheLocalForwardsWereStoredIsImported(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	source.registerHost(t, passwordHost("192.0.2.10"))
	source.forward(t, "192.0.2.10", localForwardContent{LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 5432})

	file := rewriteHosts(t, source.exportTunnels(t, testExportPassword), testExportPassword,
		func(host map[string]interface{}) {
			delete(host, "local_forwards")
		})

	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if strings.Contains(opened, "local_forwards") {
		t.Fatalf("the file still carries the local forwards: %s", opened)
	}

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if target.count(t, &models.Host{}) != 1 {
		t.Fatalf("the Host of the file was not imported")
	}

	if target.count(t, &models.LocalForward{}) != 0 {
		t.Fatalf("a local forward was stored from a file that carries none: %v", target.forwards(t))
	}
}

func TestAFileFromBeforeTheAddressesWereRenamedIsImported(t *testing.T) {
	target := newTransferInstall(t)

	content := json.RawMessage(`{
		"hosts": [
			{"ip": "192.0.2.10", "port": 22, "user": "operator", "password": "the password of the Host",
			 "enabled": true, "assigned_local_ports": [18080],
			 "local_forwards": [{"bind_scope": "loopback", "local_port": 15432,
			  "target_ip": "192.0.2.30", "target_port": 5432, "description": "", "enabled": true}]},
			{"ip": "192.0.2.99", "address": "db.example.com", "port": 22, "user": "operator",
			 "password": "the password of the Host", "enabled": true, "assigned_local_ports": []}
		],
		"service_ports": [{"service_ip": "198.51.100.20", "service_port": 80, "local_port": 18080}]
	}`)

	file := sealedAtFormat(t, 1, content, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var hosts []models.Host
	err := target.db.Order("id").Find(&hosts).Error
	if err != nil {
		t.Fatalf("failed to read the Hosts: %v", err)
	}
	if len(hosts) != 2 || hosts[0].Address != "192.0.2.10" || hosts[1].Address != "db.example.com" {
		t.Fatalf("the Hosts were stored as %+v", hosts)
	}

	var sp models.ServicePort
	err = target.db.First(&sp).Error
	if err != nil {
		t.Fatalf("failed to read the service port: %v", err)
	}
	if sp.ServiceAddress != "198.51.100.20" {
		t.Fatalf("the service port was stored at %q", sp.ServiceAddress)
	}

	want := []string{"192.0.2.10 loopback 15432 -> 192.0.2.30:5432 ()"}
	if !reflect.DeepEqual(target.forwards(t), want) {
		t.Fatalf("the installation that took the file in forwards %v, want %v", target.forwards(t), want)
	}

	if !reflect.DeepEqual(target.carried(t), []string{"192.0.2.10 carries 18080"}) {
		t.Fatalf("the assignments are %v", target.carried(t))
	}
}

// TestAnExportWritesOnlyTheNewNames holds the export to the names the import
// reads first, so that a file written now never carries the old ones.
func TestAnExportWritesOnlyTheNewNames(t *testing.T) {
	source := newTransferInstall(t)

	source.registerHost(t, passwordHost("192.0.2.10"))
	source.registerServicePort(t, servicePortContent{ServiceAddress: "198.51.100.20", ServicePort: 80, LocalPort: 18080})
	source.forward(t, "192.0.2.10", localForwardContent{LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 5432})

	opened, err := crypto.DecryptWithPassword(source.exportTunnels(t, testExportPassword), testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	for _, name := range []string{`"ip"`, `"service_ip"`, `"target_ip"`} {
		if strings.Contains(opened, name) {
			t.Errorf("the file carries %s: %s", name, opened)
		}
	}

	for _, name := range []string{`"address":"192.0.2.10"`, `"service_address":"198.51.100.20"`,
		`"target_address":"192.0.2.30"`} {
		if !strings.Contains(opened, name) {
			t.Errorf("the file does not carry %s: %s", name, opened)
		}
	}
}

// TestALocalForwardTheFileCannotCarryIsRefused holds every local forward of the
// file to what the screens refuse, and to the one local port per forward that
// the table is held to, and the refusal leaves nothing behind.
func TestALocalForwardTheFileCannotCarryIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		second localForwardContent
		code   errorCode
	}{
		{
			name:   "the local port of the other Host",
			second: localForwardContent{LocalPort: 15432, TargetAddress: "192.0.2.31", TargetPort: 22},
			code:   errImportLocalForwardDuplicate,
		},
		{
			name:   "a scope that is neither",
			second: localForwardContent{BindScope: "public", LocalPort: 15433, TargetAddress: "192.0.2.31", TargetPort: 22},
			code:   errImportLocalForwardRefused,
		},
		{
			name:   "a target that is not an address",
			second: localForwardContent{LocalPort: 15433, TargetAddress: "the database", TargetPort: 22},
			code:   errImportLocalForwardRefused,
		},
		{
			name:   "a port out of range",
			second: localForwardContent{LocalPort: 70000, TargetAddress: "192.0.2.31", TargetPort: 22},
			code:   errImportLocalForwardRefused,
		},
		{
			name: "allowed sources that are not addresses",
			second: localForwardContent{LocalPort: 15433, TargetAddress: "192.0.2.31", TargetPort: 22,
				AllowedSources: "192.0.2.0/24, not-an-address"},
			code: errImportLocalForwardRefused,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := newTransferInstall(t)
			target := newTransferInstall(t)

			first := passwordHost("192.0.2.10")
			first.LocalForwards = []localForwardContent{{LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 22}}

			second := passwordHost("192.0.2.11")
			second.LocalForwards = []localForwardContent{tc.second}

			file := sealedTunnelsFile(t, source, tunnelsContent{
				Hosts: []hostContent{first, second},
			}, testExportPassword)

			rec := target.importTunnels(t, file, testExportPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			if errorCodeOf(t, rec) != tc.code {
				t.Errorf("the import was refused under %s, want %s", errorCodeOf(t, rec), tc.code)
			}

			if target.count(t, &models.Host{}) != 0 || target.count(t, &models.LocalForward{}) != 0 {
				t.Fatalf("the refused import left rows behind")
			}

			if target.manager.count() != 0 {
				t.Errorf("a reconcile pass was asked for although nothing was imported")
			}
		})
	}
}

// TestALocalForwardOnThePortOfThisServerIsRefused is the port the stored
// settings listen on, which a local forward opened on this machine would take.
func TestALocalForwardOnThePortOfThisServerIsRefused(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	stored := settings.Defaults()
	stored.APIPort = 19443

	err := settings.Save(target.db, &stored, target.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	host := passwordHost("192.0.2.10")
	host.LocalForwards = []localForwardContent{{LocalPort: 19443, TargetAddress: "192.0.2.30", TargetPort: 443}}

	file := sealedTunnelsFile(t, source, tunnelsContent{Hosts: []hostContent{host}}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusConflict {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	if errorCodeOf(t, rec) != errImportLocalForwardAPIPort {
		t.Errorf("the import was refused under %s, want %s", errorCodeOf(t, rec), errImportLocalForwardAPIPort)
	}

	if target.count(t, &models.Host{}) != 0 || target.count(t, &models.LocalForward{}) != 0 {
		t.Fatalf("the refused import left rows behind")
	}
}

// TestALocalForwardOnTheRunningPortOfThisServerIsRefused is the port this
// process listens on when it is not the stored one: a local forward of the file
// on it is refused as one on the stored port is, and with no running port told
// the same file is imported.
func TestALocalForwardOnTheRunningPortOfThisServerIsRefused(t *testing.T) {
	for _, running := range []int{19500, 0} {
		t.Run(strconv.Itoa(running), func(t *testing.T) {
			source := newTransferInstall(t)
			target := newTransferInstall(t)
			target.handler.hosts.SetRunningAPIPort(running)

			stored := settings.Defaults()
			stored.APIPort = 19443

			err := settings.Save(target.db, &stored, target.cipher)
			if err != nil {
				t.Fatalf("failed to store the settings: %v", err)
			}

			host := passwordHost("192.0.2.10")
			host.LocalForwards = []localForwardContent{{LocalPort: 19500, TargetAddress: "192.0.2.30", TargetPort: 443}}

			file := sealedTunnelsFile(t, source, tunnelsContent{Hosts: []hostContent{host}}, testExportPassword)

			rec := target.importTunnels(t, file, testExportPassword)

			if running == 0 {
				if rec.Code != http.StatusOK || target.count(t, &models.LocalForward{}) != 1 {
					t.Fatalf("the import answered %d and stored %d local forwards, want %d and 1: %s",
						rec.Code, target.count(t, &models.LocalForward{}), http.StatusOK, rec.Body.String())
				}
				return
			}

			if rec.Code != http.StatusConflict {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
			}

			if errorCodeOf(t, rec) != errImportLocalForwardAPIPort {
				t.Errorf("the import was refused under %s, want %s", errorCodeOf(t, rec), errImportLocalForwardAPIPort)
			}

			var answer struct {
				Args errorArgs `json:"error_args"`
			}

			err = json.Unmarshal(rec.Body.Bytes(), &answer)
			if err != nil {
				t.Fatalf("failed to read the answer: %v", err)
			}
			if answer.Args["local_port"] != "19500" || answer.Args["host"] != "192.0.2.10" {
				t.Errorf("error_args = %v, want local_port 19500 and host 192.0.2.10", answer.Args)
			}

			if target.count(t, &models.Host{}) != 0 || target.count(t, &models.LocalForward{}) != 0 {
				t.Fatalf("the refused import left rows behind")
			}
		})
	}
}

// TestImportedSettingsSuggestAPortClearOfTheRunningAPIPort is a settings file
// whose api_port a local forward opens, while this process listens on a port
// other than the stored one: the port suggested steps over the running port.
func TestImportedSettingsSuggestAPortClearOfTheRunningAPIPort(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)
	target.handler.hosts.SetRunningAPIPort(15433)

	host := models.Host{Address: "192.0.2.10", Port: 22, User: "root"}

	err := target.db.Create(&host).Error
	if err != nil {
		t.Fatalf("failed to store a Host: %v", err)
	}

	forward := models.LocalForward{HostID: host.ID, BindScope: models.BindScopeLoopback, LocalPort: 15432,
		TargetAddress: "127.0.0.1", TargetPort: 5432}

	err = target.db.Create(&forward).Error
	if err != nil {
		t.Fatalf("failed to store a local forward: %v", err)
	}

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored.APIPort = 15432

	err = settings.Save(source.db, stored, source.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	file := source.exportSettings(t, testExportPassword)

	request, err := json.Marshal(importRequest{Password: testExportPassword, File: file})
	if err != nil {
		t.Fatalf("failed to write the import request: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings, string(request))
	if rec.Code != http.StatusConflict {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	answer := readAPIPortTaken(t, rec)
	if answer.Code != string(errImportSettingsAPIPortForward) {
		t.Errorf("error_code = %q, want %q", answer.Code, errImportSettingsAPIPortForward)
	}

	if answer.Data.SuggestedPort != 15434 {
		t.Errorf("suggested_port = %d, want 15434, past the running port 15433", answer.Data.SuggestedPort)
	}
}

// TestALocalPortAHostHereHoldsIsFreedByTheImport is a local forward of the file
// on a port a Host the file does not name opens here. The import deletes that
// Host with the rest of what is stored, so the port is free for the file.
func TestALocalPortAHostHereHoldsIsFreedByTheImport(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("192.0.2.12"))
	target.forward(t, "192.0.2.12", localForwardContent{BindScope: models.BindScopeWildcard,
		LocalPort: 15432, TargetAddress: "192.0.2.40", TargetPort: 5432})

	host := passwordHost("192.0.2.10")
	host.LocalForwards = []localForwardContent{{LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 5432}}

	file := sealedTunnelsFile(t, source, tunnelsContent{Hosts: []hostContent{host}}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := []string{"192.0.2.10 wildcard 15432 -> 192.0.2.30:5432 ()"}
	if !reflect.DeepEqual(target.forwards(t), want) {
		t.Fatalf("after the import the installation forwards %v, want %v", target.forwards(t), want)
	}
}

// TestALocalPortTheFileMovesBetweenHostsMoves is two Hosts here trading their
// local ports in the file: whichever of them the file names first, the port
// is free by the time it is written.
func TestALocalPortTheFileMovesBetweenHostsMoves(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("192.0.2.10"))
	target.registerHost(t, passwordHost("192.0.2.11"))
	target.forward(t, "192.0.2.10", localForwardContent{BindScope: models.BindScopeWildcard,
		LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 5432})
	target.forward(t, "192.0.2.10", localForwardContent{BindScope: models.BindScopeWildcard,
		LocalPort: 15499, TargetAddress: "192.0.2.30", TargetPort: 99})
	target.forward(t, "192.0.2.11", localForwardContent{BindScope: models.BindScopeWildcard,
		LocalPort: 15433, TargetAddress: "192.0.2.31", TargetPort: 5432})

	// The two ports trade places, and the first Host drops the third.
	first := passwordHost("192.0.2.10")
	first.LocalForwards = []localForwardContent{{BindScope: models.BindScopeLoopback,
		LocalPort: 15433, TargetAddress: "192.0.2.30", TargetPort: 5432}}

	second := passwordHost("192.0.2.11")
	second.LocalForwards = []localForwardContent{{BindScope: models.BindScopeWildcard,
		LocalPort: 15432, TargetAddress: "192.0.2.31", TargetPort: 5432}}

	file := sealedTunnelsFile(t, source, tunnelsContent{Hosts: []hostContent{first, second}}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := []string{
		"192.0.2.11 wildcard 15432 -> 192.0.2.31:5432 ()",
		"192.0.2.10 loopback 15433 -> 192.0.2.30:5432 ()",
	}

	if !reflect.DeepEqual(target.forwards(t), want) {
		t.Fatalf("after the import the installation forwards %v, want %v", target.forwards(t), want)
	}
}

// TestAHostCarriedAsForwardingNothingForwardsNothing is a Host the file names
// with an empty list: it forwards nothing after the import, whatever it
// forwarded here before.
func TestAHostCarriedAsForwardingNothingForwardsNothing(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	source.registerHost(t, passwordHost("192.0.2.10"))

	target.registerHost(t, passwordHost("192.0.2.10"))
	target.forward(t, "192.0.2.10", localForwardContent{BindScope: models.BindScopeWildcard,
		LocalPort: 15432, TargetAddress: "192.0.2.30", TargetPort: 5432})

	file := source.exportTunnels(t, testExportPassword)

	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if !strings.Contains(opened, `"local_forwards":[]`) {
		t.Fatalf("a Host with no local forward is not written with an empty list: %s", opened)
	}

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if target.count(t, &models.LocalForward{}) != 0 {
		t.Fatalf("after the import the Host still forwards %v", target.forwards(t))
	}
}

// TestImportedSettingsWithAnAPIPortALocalForwardOpensAreNotStored holds an
// import to the rule a save is held to: a port a local forward opens here is
// refused with the forward and a free port, and nothing of the file is
// stored. The pool holds one connection, as the server's does.
func TestImportedSettingsWithAnAPIPortALocalForwardOpensAreNotStored(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	sqlDB, err := target.db.DB()
	if err != nil {
		t.Fatalf("failed to reach the connection pool: %v", err)
	}

	sqlDB.SetMaxOpenConns(1)

	host := models.Host{Address: "192.0.2.10", Port: 22, User: "root"}

	err = target.db.Create(&host).Error
	if err != nil {
		t.Fatalf("failed to store a Host: %v", err)
	}

	forward := models.LocalForward{HostID: host.ID, BindScope: models.BindScopeLoopback, LocalPort: 15432,
		TargetAddress: "127.0.0.1", TargetPort: 5432}

	err = target.db.Create(&forward).Error
	if err != nil {
		t.Fatalf("failed to store a local forward: %v", err)
	}

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	stored.APIPort = 15432
	stored.MonitoringIntervalSec = 47

	err = settings.Save(source.db, stored, source.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	file := source.exportSettings(t, testExportPassword)

	request, err := json.Marshal(importRequest{Password: testExportPassword, File: file})
	if err != nil {
		t.Fatalf("failed to write the import request: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings, string(request))
	if rec.Code != http.StatusConflict {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	answer := readAPIPortTaken(t, rec)
	if answer.Code != string(errImportSettingsAPIPortForward) {
		t.Errorf("error_code = %q, want %q", answer.Code, errImportSettingsAPIPortForward)
	}

	want := apiPortHolder{Number: forward.Number, HostID: host.ID, HostAddress: "192.0.2.10", LocalPort: 15432,
		TargetAddress: "127.0.0.1", TargetPort: 5432}
	if answer.Data.LocalForward != want {
		t.Errorf("local_forward = %+v, want %+v", answer.Data.LocalForward, want)
	}

	if answer.Data.SuggestedPort != 15433 {
		t.Errorf("suggested_port = %d, want 15433", answer.Data.SuggestedPort)
	}

	after, err := settings.Load(target.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if after.APIPort != 8888 || after.MonitoringIntervalSec == 47 {
		t.Errorf("the refused file was stored: api_port %d, monitoring.interval_sec %d",
			after.APIPort, after.MonitoringIntervalSec)
	}

	var rows []models.LocalForward

	err = target.db.Find(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the local forwards: %v", err)
	}

	if len(rows) != 1 || rows[0].LocalPort != 15432 {
		t.Errorf("the local forwards changed: %+v", rows)
	}
}

// namedItem is what an item of an import is expected to say, by code and by
// the English written from it.
type namedItem struct {
	kind         string
	name         string
	nameCode     textCode
	nameValues   textArgs
	action       string
	reason       string
	reasonCode   textCode
	reasonValues textArgs
}

// checkNamedItems holds the items of an answer to what they are expected to
// say. The English is compared as a literal, so that a sentence moved into
// transferTexts reads as it did before, and as the sentence of its code filled
// in with the values the item carries, so that the two cannot drift apart. The
// field names are read from the raw answer, since they are what a screen and a
// script look for.
func checkNamedItems(t *testing.T, raw json.RawMessage, want []namedItem) {
	t.Helper()

	var answer struct {
		Items []transferItem `json:"items"`
	}

	err := json.Unmarshal(raw, &answer)
	if err != nil {
		t.Fatalf("failed to read the items: %v", err)
	}

	var fields struct {
		Items []map[string]json.RawMessage `json:"items"`
	}

	err = json.Unmarshal(raw, &fields)
	if err != nil {
		t.Fatalf("failed to read the items: %v", err)
	}

	got := map[string]transferItem{}
	gotFields := map[string]map[string]json.RawMessage{}

	for i, item := range answer.Items {
		got[item.Kind+" "+item.Name] = item
		gotFields[item.Kind+" "+item.Name] = fields.Items[i]
	}

	for _, w := range want {
		item, found := got[w.kind+" "+w.name]
		if !found {
			t.Errorf("the answer has no %s named %q: %+v", w.kind, w.name, answer.Items)
			continue
		}

		if item.Action != w.action {
			t.Errorf("%s %q was %q, want %q", w.kind, w.name, item.Action, w.action)
		}

		if item.NameCode != w.nameCode || !reflect.DeepEqual(item.NameValues, w.nameValues) {
			t.Errorf("%s %q is named by %q %v, want %q %v", w.kind, w.name,
				item.NameCode, item.NameValues, w.nameCode, w.nameValues)
		}

		if item.Reason != w.reason {
			t.Errorf("%s %q carries the reason %q, want %q", w.kind, w.name, item.Reason, w.reason)
		}

		if item.ReasonCode != w.reasonCode || !reflect.DeepEqual(item.ReasonValues, w.reasonValues) {
			t.Errorf("%s %q gives its reason as %q %v, want %q %v", w.kind, w.name,
				item.ReasonCode, item.ReasonValues, w.reasonCode, w.reasonValues)
		}

		for _, text := range []struct {
			english string
			code    textCode
			values  textArgs
			fields  []string
		}{
			{item.Name, item.NameCode, item.NameValues, []string{"name_code", "name_values"}},
			{item.Reason, item.ReasonCode, item.ReasonValues, []string{"reason_code", "reason_values"}},
		} {
			if text.code == "" {
				for _, field := range text.fields {
					if _, carried := gotFields[w.kind+" "+w.name][field]; carried {
						t.Errorf("%s %q carries %s with nothing to name", w.kind, w.name, field)
					}
				}

				continue
			}

			if _, carried := gotFields[w.kind+" "+w.name][text.fields[0]]; !carried {
				t.Errorf("%s %q has no %s in the answer", w.kind, w.name, text.fields[0])
			}

			template, known := transferTexts[text.code]
			if !known {
				t.Errorf("%s %q is named by %q, which has no sentence", w.kind, w.name, text.code)
				continue
			}

			written, filled := renderErrorMessage(template, errorArgs(text.values))
			if !filled {
				t.Errorf("the sentence of %q is not filled by %v", text.code, text.values)
			}

			if written != text.english {
				t.Errorf("%s %q carries %q, and its code %q writes %q", w.kind, w.name,
					text.english, text.code, written)
			}
		}
	}
}

// TestADroppedPathNamesItsReason is the same for the settings: a path outside
// the installation and an empty one are each skipped under a code of their own,
// the empty one because "an empty path" is not a value a translator can place.
func TestADroppedPathNamesItsReason(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	stored, err := settings.Load(source.db)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	outside := platformAbsolutePath("/var/lib/tunnel-manager/tunnel-manager.key")

	content := settingsOf(stored)
	content.SecurityKeyFile = outside
	content.LoggingFilePath = ""

	file, err := source.handler.seal(transferKindSettings, content, testExportPassword, time.Now())
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	request, err := json.Marshal(importRequest{Password: testExportPassword, File: file})
	if err != nil {
		t.Fatalf("failed to write the import request: %v", err)
	}

	rec := target.call(t, target.handler.ImportSettings, string(request))
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	defaults := settings.Defaults()

	checkNamedItems(t, decodeTransfer(t, rec).Data, []namedItem{
		{
			kind: "setting", name: "security.key_file", action: transferSkipped,
			reason: "the file carries " + outside + ", which does not name " +
				"a file under the directory the database file is in, so " + defaults.SecurityKeyFile +
				" was stored instead",
			reasonCode: textImportReasonPathOutside,
			reasonValues: textArgs{"carried": outside,
				"stored": defaults.SecurityKeyFile},
		},
		{
			kind: "setting", name: "logging.file.path", action: transferSkipped,
			reason: "the file carries an empty path, which does not name a file under the directory " +
				"the database file is in, so " + defaults.LoggingFilePath + " was stored instead",
			reasonCode:   textImportReasonEmptyPathOutside,
			reasonValues: textArgs{"stored": defaults.LoggingFilePath},
		},
	})
}

// TestTheAllowedSourcesOfALocalForwardCrossWithIt is the list on the round
// trip: the export writes it, the import stores it, and a file from before the
// list, which carries no field, stores every forward with an empty list, which
// lets every address in as those forwards always did.
func TestTheAllowedSourcesOfALocalForwardCrossWithIt(t *testing.T) {
	source := newTransferInstall(t)

	source.registerHost(t, passwordHost("192.0.2.10"))
	source.forward(t, "192.0.2.10", localForwardContent{BindScope: models.BindScopeWildcard, LocalPort: 15432,
		TargetAddress: "127.0.0.1", TargetPort: 5432, AllowedSources: "192.0.2.0/24, 198.51.100.7"})

	file := source.exportTunnels(t, testExportPassword)

	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if !strings.Contains(opened, `"allowed_sources":"192.0.2.0/24, 198.51.100.7"`) {
		t.Fatalf("the file does not carry the allowed sources: %s", opened)
	}

	sources := func(install *transferInstall) []string {
		var rows []models.LocalForward

		err := install.db.Order("local_port").Find(&rows).Error
		if err != nil {
			t.Fatalf("failed to read the local forwards: %v", err)
		}

		lists := make([]string, 0, len(rows))
		for _, row := range rows {
			lists = append(lists, row.AllowedSources)
		}

		return lists
	}

	target := newTransferInstall(t)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if got := sources(target); !reflect.DeepEqual(got, []string{"192.0.2.0/24, 198.51.100.7"}) {
		t.Fatalf("the installation that took the file in holds %q", got)
	}

	older := rewriteHosts(t, file, testExportPassword, func(host map[string]interface{}) {
		forwards, ok := host["local_forwards"].([]interface{})
		if !ok {
			t.Fatalf("the Host carries no list of local forwards")
		}

		for _, entry := range forwards {
			forward, ok := entry.(map[string]interface{})
			if !ok {
				t.Fatalf("a local forward in the file is not an object")
			}

			delete(forward, "allowed_sources")
		}
	})

	opened, err = crypto.DecryptWithPassword(older, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if strings.Contains(opened, `"allowed_sources"`) {
		t.Fatalf("the file still carries the allowed sources: %s", opened)
	}

	fromOlder := newTransferInstall(t)

	rec = fromOlder.importTunnels(t, older, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import of the older file answered %d: %s", rec.Code, rec.Body.String())
	}

	if got := sources(fromOlder); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("the installation that took the older file in holds %q, want one empty list", got)
	}
}

// accountPasswordCase is one of the three calls that ask for the password of
// the account, with what it answers under when that password is missing and
// when it is wrong.
type accountPasswordCase struct {
	name     string
	path     string
	handler  func(*transferInstall) func(echo.Context) error
	body     func(t *testing.T, i *transferInstall, accountPassword *string) string
	required string
	wrong    string
	logID    string
}

// transferBody writes the body of an export or of an import of the tunnels,
// with account_password left out where accountPassword is nil. file is empty
// for an export.
func transferBody(t *testing.T, file string, accountPassword *string) string {
	t.Helper()

	body := map[string]interface{}{"password": testExportPassword}

	if file != "" {
		body["file"] = file
	}

	if accountPassword != nil {
		body["account_password"] = *accountPassword
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to write the body: %v", err)
	}

	return string(encoded)
}

func accountPasswordCases() []accountPasswordCase {
	return []accountPasswordCase{
		{
			name: "the export of the tunnels",
			path: "/api/export/tunnels",
			handler: func(i *transferInstall) func(echo.Context) error {
				return i.handler.ExportTunnels
			},
			body: func(t *testing.T, _ *transferInstall, accountPassword *string) string {
				return transferBody(t, "", accountPassword)
			},
			required: "export.account_password.required",
			wrong:    "export.account_password.wrong",
			logID:    "transfer.export_account_password_wrong",
		},
		{
			name: "the export of the settings",
			path: "/api/export/settings",
			handler: func(i *transferInstall) func(echo.Context) error {
				return i.handler.ExportSettings
			},
			body: func(t *testing.T, _ *transferInstall, accountPassword *string) string {
				return transferBody(t, "", accountPassword)
			},
			required: "export.account_password.required",
			wrong:    "export.account_password.wrong",
			logID:    "transfer.export_account_password_wrong",
		},
		{
			name: "the import of the tunnels",
			path: "/api/import/tunnels",
			handler: func(i *transferInstall) func(echo.Context) error {
				return i.handler.ImportTunnels
			},
			body: func(t *testing.T, i *transferInstall, accountPassword *string) string {
				return transferBody(t, i.exportTunnels(t, testExportPassword), accountPassword)
			},
			required: "import.account_password.required",
			wrong:    "import.account_password.wrong",
			logID:    "transfer.import_account_password_wrong",
		},
	}
}

// TestTheTransfersOfSecretsAskForTheAccountPassword holds the two exports and
// the import of the tunnels to the password of the account. A file of either
// export carries the credentials of every Host or the secrets of the settings
// in the clear under a password the caller picks, and the import of the tunnels
// replaces the host keys the approval on the Status screen asks the password
// for. A session left open is not to be enough for any of the three.
func TestTheTransfersOfSecretsAskForTheAccountPassword(t *testing.T) {
	wrong := "not the password of the account"
	right := testPassword

	for _, tc := range accountPasswordCases() {
		t.Run(tc.name, func(t *testing.T) {
			install := newTransferInstall(t)
			withKey, withPassword := twoHosts(t)
			install.registerHost(t, withKey)
			install.registerHost(t, withPassword)

			rec := install.call(t, tc.handler(install), tc.body(t, install, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("with no account_password it answered %d, want 400: %s", rec.Code, rec.Body.String())
			}

			if code := refusalCode(t, rec); code != tc.required {
				t.Errorf("with no account_password error_code = %q, want %q", code, tc.required)
			}

			rec = install.call(t, tc.handler(install), tc.body(t, install, &wrong))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("with the wrong account_password it answered %d, want 401: %s",
					rec.Code, rec.Body.String())
			}

			// The screen reads this name to keep the operator on the form
			// rather than sending them to the login.
			if code := refusalCode(t, rec); code != tc.wrong {
				t.Errorf("with the wrong account_password error_code = %q, want %q", code, tc.wrong)
			}

			written := install.logs.FilterField(zap.String(logid.FieldKey, tc.logID)).Len()
			if written != 1 {
				t.Errorf("the wrong password was written down %d times, want 1", written)
			}

			for _, line := range install.logs.All() {
				if strings.Contains(line.Message, wrong) {
					t.Errorf("a log line carries what was typed: %s", line.Message)
				}

				for _, field := range line.Context {
					if strings.Contains(field.String, wrong) {
						t.Errorf("a log field carries what was typed: %s=%s", field.Key, field.String)
					}
				}
			}

			rec = install.call(t, tc.handler(install), tc.body(t, install, &right))
			if rec.Code != http.StatusOK {
				t.Fatalf("with the account_password it answered %d, want 200: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTheImportOfTheSettingsDoesNotAskForTheAccountPassword is the one of the
// four that is left as it was: the settings file carries no credential of a
// Host and no host key, and what it stores is what the Settings screen stores
// with the same session and no password.
func TestTheImportOfTheSettingsDoesNotAskForTheAccountPassword(t *testing.T) {
	install := newTransferInstall(t)
	file := install.exportSettings(t, testExportPassword)

	rec := install.call(t, install.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import of the settings with no account_password answered %d, want 200: %s",
			rec.Code, rec.Body.String())
	}
}

// transferServer is the installation served the way main serves it, behind the
// session middleware, with a token that carries the transfer scope. It is what
// holds a token to the same answers a session gets: the middleware leaves the
// account and the limiter on the context for either, and the password is
// asked for all the same.
func (i *transferInstall) transferServer(t *testing.T) (*echo.Echo, string) {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	authHandler := NewAuthHandler(i.db, zap.NewNop(), "")

	g := e.Group("/api")
	g.Use(authHandler.RequireSession())
	g.POST("/login", authHandler.Login)
	g.POST("/token", authHandler.CreateToken)
	g.POST("/export/tunnels", i.handler.ExportTunnels)
	g.POST("/import/tunnels", i.handler.ImportTunnels)
	g.POST("/export/settings", i.handler.ExportSettings)
	g.POST("/import/settings", i.handler.ImportSettings)

	cookies := csrfLoginCookies(t, e, loginBody(t, testUsername, testPassword))

	rec := do(e, http.MethodPost, "/api/token",
		`{"name":"backup","scopes":["`+TokenScopeTransfer+`"],"account_password":"`+testPassword+`"}`, cookies...) // hook:allow
	if rec.Code != http.StatusCreated {
		t.Fatalf("the creation of the token answered %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var created tokenCreated

	err := json.Unmarshal(decodeTokenAnswer(t, rec).Data, &created)
	if err != nil {
		t.Fatalf("failed to read the created token: %v", err)
	}

	return e, created.Token
}

// TestATokenIsAskedForTheAccountPasswordToo holds a request made with a token
// to the answers a session gets. A token with the transfer scope is otherwise
// a way to take every credential out of the installation that no password
// stands in front of.
func TestATokenIsAskedForTheAccountPasswordToo(t *testing.T) {
	wrong := "not the password of the account"
	right := testPassword

	for _, tc := range accountPasswordCases() {
		t.Run(tc.name, func(t *testing.T) {
			install := newTransferInstall(t)
			withKey, withPassword := twoHosts(t)
			install.registerHost(t, withKey)
			install.registerHost(t, withPassword)

			e, token := install.transferServer(t)

			send := func(accountPassword *string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, tc.path,
					strings.NewReader(tc.body(t, install, accountPassword)))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)

				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)

				return rec
			}

			rec := send(nil)
			if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != tc.required {
				t.Fatalf("with no account_password it answered %d, want 400 %s: %s",
					rec.Code, tc.required, rec.Body.String())
			}

			rec = send(&wrong)
			if rec.Code != http.StatusUnauthorized || refusalCode(t, rec) != tc.wrong {
				t.Fatalf("with the wrong account_password it answered %d, want 401 %s: %s",
					rec.Code, tc.wrong, rec.Body.String())
			}

			rec = send(&right)
			if rec.Code != http.StatusOK {
				t.Fatalf("with the account_password it answered %d, want 200: %s", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("the import of the settings", func(t *testing.T) {
		install := newTransferInstall(t)
		file := install.exportSettings(t, testExportPassword)
		e, token := install.transferServer(t)

		req := httptest.NewRequest(http.MethodPost, "/api/import/settings",
			strings.NewReader(`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`)) // hook:allow
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)

		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("with no account_password it answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}

// jump gives a stored Host a jump route through the stored Hosts named, in
// order, the way the Host screen stores one.
func (i *transferInstall) jump(t *testing.T, hostIP string, through ...string) {
	t.Helper()

	idOf := func(address string) uint {
		var host models.Host

		err := i.db.Where("address = ?", address).First(&host).Error
		if err != nil {
			t.Fatalf("the Host %s is not registered here: %v", address, err)
		}

		return host.ID
	}

	hostID := idOf(hostIP)

	for n, address := range through {
		err := i.db.Create(&models.HostJump{HostID: hostID, Seq: uint(n + 1), JumpHostID: idOf(address)}).Error
		if err != nil {
			t.Fatalf("failed to store the jump route of %s: %v", hostIP, err)
		}
	}
}

// configuration is every row of the tunnel configuration stored, written out
// with its id, its number and its times, and with the secrets of the Hosts
// opened: they are sealed with a key of each installation, so what is the same
// on both sides is what they open to.
func (i *transferInstall) configuration(t *testing.T) []string {
	t.Helper()

	var written []string

	add := func(what string, row interface{}) {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("failed to write a row: %v", err)
		}

		written = append(written, what+" "+string(encoded))
	}

	var hosts []models.Host

	err := i.db.Order("id").Find(&hosts).Error
	if err != nil {
		t.Fatalf("failed to read the Hosts: %v", err)
	}

	for _, host := range hosts {
		opened, err := i.handler.unsealHost(host)
		if err != nil {
			t.Fatalf("the secrets of the Host %s do not open: %v", host.Address, err)
		}

		host.CreatedAt = host.CreatedAt.UTC()
		host.UpdatedAt = host.UpdatedAt.UTC()

		add("host", struct {
			models.Host
			Password      string
			PrivateKey    string
			KeyPassphrase string
		}{host, opened.Password, opened.PrivateKey, opened.KeyPassphrase})
	}

	var sps []models.ServicePort

	err = i.db.Order("id").Find(&sps).Error
	if err != nil {
		t.Fatalf("failed to read the service ports: %v", err)
	}

	for _, sp := range sps {
		sp.CreatedAt = sp.CreatedAt.UTC()
		sp.UpdatedAt = sp.UpdatedAt.UTC()
		add("service_port", sp)
	}

	var assignments []models.HostServicePort

	err = i.db.Order("host_id, sp_id").Find(&assignments).Error
	if err != nil {
		t.Fatalf("failed to read the assignments: %v", err)
	}

	for _, assignment := range assignments {
		assignment.CreatedAt = assignment.CreatedAt.UTC()
		add("assignment", assignment)
	}

	var forwards []models.LocalForward

	err = i.db.Order("host_id, number").Find(&forwards).Error
	if err != nil {
		t.Fatalf("failed to read the local forwards: %v", err)
	}

	for _, lf := range forwards {
		lf.CreatedAt = lf.CreatedAt.UTC()
		lf.UpdatedAt = lf.UpdatedAt.UTC()
		add("local_forward", lf)
	}

	var jumps []models.HostJump

	err = i.db.Order("host_id, seq").Find(&jumps).Error
	if err != nil {
		t.Fatalf("failed to read the jump routes: %v", err)
	}

	for _, jump := range jumps {
		add("jump", jump)
	}

	return written
}

// formatOne writes a file of this version again as the release before the ids
// wrote it: no id, number, time or pending key on any row, no jump route, and
// the assignments named by the local port of the service port.
func formatOne(t *testing.T, file string, password string) string {
	t.Helper()

	plaintext, err := crypto.DecryptWithPassword(file, password)
	if err != nil {
		t.Fatalf("failed to open the file: %v", err)
	}

	var read transferFile

	err = json.Unmarshal([]byte(plaintext), &read)
	if err != nil {
		t.Fatalf("failed to read the file: %v", err)
	}

	var content tunnelsContent

	err = json.Unmarshal(read.Content, &content)
	if err != nil {
		t.Fatalf("failed to read the content of the file: %v", err)
	}

	localPortOf := make(map[uint]int, len(content.ServicePorts))

	for n := range content.ServicePorts {
		sp := &content.ServicePorts[n]
		localPortOf[sp.ID] = sp.LocalPort
		sp.ID, sp.CreatedAt, sp.UpdatedAt = 0, nil, nil
	}

	for n := range content.Hosts {
		host := &content.Hosts[n]

		host.AssignedLocalPorts = []int{}

		for _, assignment := range host.Assignments {
			localPort := strconv.Itoa(localPortOf[assignment.ServicePortID])
			host.AssignedLocalPorts = append(host.AssignedLocalPorts, localPortOf[assignment.ServicePortID])

			if assignment.BindScope != "" {
				if host.AssignedBindScopes == nil {
					host.AssignedBindScopes = map[string]string{}
				}

				host.AssignedBindScopes[localPort] = assignment.BindScope
			}

			if !assignment.Enabled {
				if host.AssignedEnabled == nil {
					host.AssignedEnabled = map[string]bool{}
				}

				host.AssignedEnabled[localPort] = false
			}
		}

		host.ID, host.CreatedAt, host.UpdatedAt = 0, nil, nil
		host.PendingHostKey, host.JumpHostIDs, host.Assignments = "", nil, nil

		for m := range host.LocalForwards {
			lf := &host.LocalForwards[m]
			lf.Number, lf.CreatedAt, lf.UpdatedAt = 0, nil, nil
		}
	}

	// assigned_local_ports is written even where it is empty, since an empty
	// list and no field say different things in a file of this version.
	body, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("failed to write the content back: %v", err)
	}

	var fields map[string]interface{}

	err = json.Unmarshal(body, &fields)
	if err != nil {
		t.Fatalf("failed to read the content back: %v", err)
	}

	for _, entry := range fields["hosts"].([]interface{}) {
		host := entry.(map[string]interface{})
		if _, carried := host["assigned_local_ports"]; !carried {
			host["assigned_local_ports"] = []interface{}{}
		}
	}

	return sealedAtFormat(t, 1, fields, password)
}

// testRoute is an installation with something in every table of the tunnel
// configuration, with gaps in the ids and the numbers and with times of its
// own, so that an import that numbered anything anew or stamped anything with
// the time of the write is told from one that put the rows back.
func testRoute(t *testing.T) *transferInstall {
	t.Helper()

	source := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	withKey.HostKey = testTrustedHostKey

	source.registerHost(t, withKey)
	source.registerHost(t, passwordHost("192.0.2.12"))
	source.registerHost(t, withPassword)
	source.registerHost(t, passwordHost("203.0.113.5"))
	source.registerHost(t, passwordHost("203.0.113.6"))

	// The second Host goes, so the ids are 1, 3, 4 and 5.
	err := source.db.Delete(&models.Host{}, 2).Error
	if err != nil {
		t.Fatalf("failed to delete a Host: %v", err)
	}

	source.presentedAKeyNobodyHasApproved(t, withKey.Address, testPresentedHostKey)
	source.setSocks(t, withPassword.Address, 1080, models.BindScopeLoopback, "192.0.2.0/24")

	err = source.db.Model(&models.Host{}).Where("address = ?", "203.0.113.6").
		Updates(map[string]interface{}{"enabled": false, "socks_bind_scope": ""}).Error
	if err != nil {
		t.Fatalf("failed to change a Host: %v", err)
	}

	source.jump(t, "203.0.113.5", withKey.Address)
	source.jump(t, "203.0.113.6", withKey.Address, withPassword.Address, "203.0.113.5")

	threeServicePorts(t, source)

	// The first service port goes, so the ids are 2 and 3.
	err = source.db.Delete(&models.ServicePort{}, 1).Error
	if err != nil {
		t.Fatalf("failed to delete a service port: %v", err)
	}

	source.assign(t, withKey.Address, 18081)
	source.assign(t, "203.0.113.5", 18081)
	source.assign(t, "203.0.113.5", 18082)
	source.openTo(t, "203.0.113.5", 18082, models.BindScopeLoopback)
	source.switchOff(t, withKey.Address, 18081)

	off := false

	source.forward(t, "203.0.113.5", localForwardContent{BindScope: models.BindScopeLoopback,
		LocalPort: 15432, TargetAddress: "127.0.0.1", TargetPort: 5432, Description: "goes"})
	source.forward(t, "203.0.113.5", localForwardContent{BindScope: models.BindScopeWildcard,
		LocalPort: 15433, TargetAddress: "203.0.113.7", TargetPort: 5432, AllowedSources: "192.0.2.0/24"})
	source.forward(t, "203.0.113.5", localForwardContent{LocalPort: 15434, TargetAddress: "db.example.com",
		TargetPort: 5432, Enabled: &off})

	// The first forward goes, so the numbers are 2 and 3.
	err = source.db.Where("local_port = ?", 15432).Delete(&models.LocalForward{}).Error
	if err != nil {
		t.Fatalf("failed to delete a local forward: %v", err)
	}

	// Every row is given times of its own, a day apart for each table, so that
	// the times of the write are told from the ones put back.
	at := time.Date(2025, 3, 14, 15, 9, 26, 535897932, time.UTC)

	for n, table := range []string{"hosts", "service_ports", "local_forwards"} {
		err = source.db.Exec("UPDATE "+table+" SET created_at = ?, updated_at = ?",
			at.Add(time.Duration(n)*24*time.Hour), at.Add(time.Duration(n)*24*time.Hour+time.Minute)).Error
		if err != nil {
			t.Fatalf("failed to set the times of %s: %v", table, err)
		}
	}

	err = source.db.Exec("UPDATE host_service_ports SET created_at = ?", at.Add(72*time.Hour)).Error
	if err != nil {
		t.Fatalf("failed to set the times of the assignments: %v", err)
	}

	return source
}

// TestAnExportAndAnImportPutTheConfigurationBackAsItWas is what the file is for
// now: taken in by an installation that holds something else entirely, it
// leaves every table of the tunnel configuration as the one it came from held
// it, ids, numbers, jump routes and times included.
func TestAnExportAndAnImportPutTheConfigurationBackAsItWas(t *testing.T) {
	source := testRoute(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("198.51.100.1"))
	target.registerHost(t, passwordHost("198.51.100.2"))
	target.registerServicePort(t, servicePortContent{ServiceAddress: "198.51.100.30", ServicePort: 80, LocalPort: 18080})
	target.assign(t, "198.51.100.1", 18080)
	target.forward(t, "198.51.100.1", localForwardContent{LocalPort: 15433, TargetAddress: "198.51.100.40", TargetPort: 22})
	target.jump(t, "198.51.100.2", "198.51.100.1")

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	want := source.configuration(t)
	got := target.configuration(t)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the installation that took the file in holds\n%s\nwant\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	wantAnswer := importedTunnels{
		Current: transferCounts{Hosts: 2, ServicePorts: 1, Assignments: 1, LocalForwards: 1, JumpHosts: 1},
		File:    transferCounts{Hosts: 4, ServicePorts: 2, Assignments: 3, LocalForwards: 2, JumpHosts: 2},
	}
	if imported != wantAnswer {
		t.Errorf("the import answered %+v, want %+v", imported, wantAnswer)
	}
}

// TestTheNextServicePortIsNumberedPastTheImportedOnes is the sequence sqlite
// hands the next id out of. The import writes the ids of the file itself, and
// a service port added afterwards has to be numbered past every one of them,
// whatever the installation had numbered up to before.
func TestTheNextServicePortIsNumberedPastTheImportedOnes(t *testing.T) {
	for _, before := range []int{0, 5} {
		t.Run(strconv.Itoa(before), func(t *testing.T) {
			source := testRoute(t)
			target := newTransferInstall(t)

			for n := 0; n < before; n++ {
				target.registerServicePort(t, servicePortContent{ServiceAddress: "198.51.100.30",
					ServicePort: 1000 + n, LocalPort: 19000 + n})
			}

			rec := target.importTunnels(t, source.exportTunnels(t, testExportPassword), testExportPassword)
			if rec.Code != http.StatusOK {
				t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
			}

			var sequence int64

			err := target.db.Raw("SELECT seq FROM sqlite_sequence WHERE name = ?", "service_ports").
				Scan(&sequence).Error
			if err != nil {
				t.Fatalf("failed to read the sequence: %v", err)
			}

			added := models.ServicePort{ServiceAddress: "198.51.100.31", ServicePort: 80, LocalPort: 19999}

			err = target.db.Create(&added).Error
			if err != nil {
				t.Fatalf("failed to add a service port: %v", err)
			}

			t.Logf("the sequence stands at %d after the import, and the next service port is %d",
				sequence, added.ID)

			if added.ID <= 3 {
				t.Fatalf("the service port added after the import is %d, which is not past the imported 3", added.ID)
			}
		})
	}
}

// TestAFileWithoutIDsIsNumberedInItsOrder is the import of a file of the
// release before the ids: whatever the installation held, the rows of the file
// are numbered from 1 in the order the file lists them, and no Host has a jump
// route.
func TestAFileWithoutIDsIsNumberedInItsOrder(t *testing.T) {
	source := testRoute(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("198.51.100.1"))
	target.registerHost(t, passwordHost("198.51.100.2"))
	target.jump(t, "198.51.100.2", "198.51.100.1")

	rec := target.importTunnels(t, formatOne(t, source.exportTunnels(t, testExportPassword), testExportPassword),
		testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var hosts []models.Host

	err := target.db.Order("id").Find(&hosts).Error
	if err != nil {
		t.Fatalf("failed to read the Hosts: %v", err)
	}

	got := make([]string, 0, len(hosts))
	for _, host := range hosts {
		got = append(got, strconv.FormatUint(uint64(host.ID), 10)+" "+host.Address+" "+host.PendingHostKey)
	}

	want := []string{"1 192.0.2.10 ", "2 192.0.2.11 ", "3 203.0.113.5 ", "4 203.0.113.6 "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the Hosts are %v, want %v", got, want)
	}

	var sps []models.ServicePort

	err = target.db.Order("id").Find(&sps).Error
	if err != nil {
		t.Fatalf("failed to read the service ports: %v", err)
	}

	if len(sps) != 2 || sps[0].ID != 1 || sps[0].LocalPort != 18081 || sps[1].ID != 2 || sps[1].LocalPort != 18082 {
		t.Fatalf("the service ports are %+v, want 18081 as 1 and 18082 as 2", sps)
	}

	var numbers []uint

	err = target.db.Model(&models.LocalForward{}).Order("number").Pluck("number", &numbers).Error
	if err != nil {
		t.Fatalf("failed to read the local forwards: %v", err)
	}

	if !reflect.DeepEqual(numbers, []uint{1, 2}) {
		t.Fatalf("the local forwards are numbered %v, want 1 and 2", numbers)
	}

	if target.count(t, &models.HostJump{}) != 0 {
		t.Fatalf("a file without ids left %d jump route steps, want none", target.count(t, &models.HostJump{}))
	}

	wantCarried := []string{"192.0.2.10 carries 18081", "203.0.113.5 carries 18081", "203.0.113.5 carries 18082"}
	if !reflect.DeepEqual(target.carried(t), wantCarried) {
		t.Fatalf("the assignments are %v, want %v", target.carried(t), wantCarried)
	}
}

// TestABodyThatCarriesOverwriteIsRefused is the field of the import that added
// and skipped rows. A screen or a script written for that import sends it, and
// what it would get now is its whole configuration replaced, so it is told
// instead, whatever the field says, null included, and nothing is read or
// written. A body without the key is imported.
func TestABodyThatCarriesOverwriteIsRefused(t *testing.T) {
	source := testRoute(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("198.51.100.1"))

	before := target.configuration(t)
	file := source.exportTunnels(t, testExportPassword)

	for _, dryRun := range []bool{false, true} {
		for _, overwrite := range []interface{}{nil, false, true} {
			rec := target.importTunnelsWith(t, map[string]interface{}{
				"password":         testExportPassword,
				"account_password": testPassword,
				"file":             file,
				"dry_run":          dryRun,
				"overwrite":        overwrite,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("dry_run %v, overwrite %v: the import answered %d, want %d: %s", dryRun, overwrite,
					rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			if errorCodeOf(t, rec) != errImportOverwriteRemoved {
				t.Errorf("dry_run %v, overwrite %v: the import was refused under %s, want %s", dryRun, overwrite,
					errorCodeOf(t, rec), errImportOverwriteRemoved)
			}
		}
	}

	if !reflect.DeepEqual(target.configuration(t), before) || target.manager.count() != 0 {
		t.Fatalf("a refused import changed what is stored or woke the loop")
	}

	rec := target.importTunnelsWith(t, map[string]interface{}{
		"password":         testExportPassword,
		"account_password": testPassword,
		"file":             file,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("a body without overwrite answered %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if reflect.DeepEqual(target.configuration(t), before) {
		t.Fatalf("a body without overwrite left the configuration as it was")
	}
}

// TestAJumpRouteTheFileCannotCarryIsRefused holds every jump route of the file
// to the rules the Host screen is held to, and refuses the whole file for one
// that breaks them.
func TestAJumpRouteTheFileCannotCarryIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name  string
		route []interface{}
		code  errorCode
		args  map[string]string
	}{
		{"a Host the file does not hold", []interface{}{1, 42}, errImportJumpUnknownHost,
			map[string]string{"host": "203.0.113.6", "jump_host_id": "42"}},
		{"the Host itself", []interface{}{1, 5}, errImportJumpSelf,
			map[string]string{"host": "203.0.113.6"}},
		{"one Host twice", []interface{}{1, 3, 1}, errImportJumpDuplicate,
			map[string]string{"host": "203.0.113.6", "jump_host_id": "1"}},
		{"more than eight Hosts", []interface{}{1, 3, 4, 1, 3, 4, 1, 3, 4}, errImportJumpTooMany,
			map[string]string{"host": "203.0.113.6", "max": "8"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := testRoute(t)
			target := newTransferInstall(t)

			target.registerHost(t, passwordHost("198.51.100.1"))

			before := target.configuration(t)

			file := rewriteHosts(t, source.exportTunnels(t, testExportPassword), testExportPassword,
				func(host map[string]interface{}) {
					if host["address"] == "203.0.113.6" {
						host["jump_host_ids"] = tt.route
					}
				})

			rec := target.importTunnels(t, file, testExportPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			var refused struct {
				Code errorCode         `json:"error_code"`
				Args map[string]string `json:"error_args"`
			}

			if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil {
				t.Fatalf("the refusal is not JSON: %v", err)
			}

			if refused.Code != tt.code || !reflect.DeepEqual(refused.Args, tt.args) {
				t.Errorf("the import was refused under %s with %v, want %s with %v",
					refused.Code, refused.Args, tt.code, tt.args)
			}

			if !reflect.DeepEqual(target.configuration(t), before) || target.manager.count() != 0 {
				t.Fatalf("a refused import changed what is stored or woke the loop")
			}
		})
	}
}

// TestAnIDTheFileCarriesTwiceIsRefused is a file with ids whose ids do not
// name one row each: an id that is missing or taken by another Host, another
// service port or, for a local forward, another forward of the same Host.
func TestAnIDTheFileCarriesTwiceIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(content map[string]interface{})
		code   errorCode
	}{
		{"a Host with the id of another", func(content map[string]interface{}) {
			content["hosts"].([]interface{})[1].(map[string]interface{})["id"] = 1
		}, errImportHostIDInvalid},
		{"a Host without an id", func(content map[string]interface{}) {
			delete(content["hosts"].([]interface{})[0].(map[string]interface{}), "id")
		}, errImportHostIDInvalid},
		{"a service port with the id of another", func(content map[string]interface{}) {
			content["service_ports"].([]interface{})[1].(map[string]interface{})["id"] = 2
		}, errImportServicePortIDInvalid},
		{"a local forward with the number of another", func(content map[string]interface{}) {
			forwards := content["hosts"].([]interface{})[2].(map[string]interface{})["local_forwards"].([]interface{})
			forwards[1].(map[string]interface{})["number"] = 2
		}, errImportLocalForwardNumberInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := testRoute(t)
			target := newTransferInstall(t)

			target.registerHost(t, passwordHost("198.51.100.1"))

			before := target.configuration(t)

			file := rewriteContent(t, source.exportTunnels(t, testExportPassword), testExportPassword, tt.change)

			rec := target.importTunnels(t, file, testExportPassword)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			if errorCodeOf(t, rec) != tt.code {
				t.Errorf("the import was refused under %s, want %s", errorCodeOf(t, rec), tt.code)
			}

			if !reflect.DeepEqual(target.configuration(t), before) {
				t.Fatalf("a refused import changed what is stored")
			}
		})
	}
}

// rewriteContent opens a file, hands its content over as the JSON object it
// is, and seals what comes back with the same password and format version.
func rewriteContent(t *testing.T, file string, password string, change func(content map[string]interface{})) string {
	t.Helper()

	plaintext, err := crypto.DecryptWithPassword(file, password)
	if err != nil {
		t.Fatalf("failed to open the file: %v", err)
	}

	var read transferFile

	err = json.Unmarshal([]byte(plaintext), &read)
	if err != nil {
		t.Fatalf("failed to read the file: %v", err)
	}

	var content map[string]interface{}

	err = json.Unmarshal(read.Content, &content)
	if err != nil {
		t.Fatalf("failed to read the content of the file: %v", err)
	}

	change(content)

	return sealedAtFormat(t, read.FormatVersion, content, password)
}

// TestADryRunWritesNothing is the question a screen asks before it replaces
// anything: the file is opened and checked, the answer says what is stored and
// what the file holds, and nothing is written, the loop is not woken and no
// import is logged. The password of the account is asked for all the same, and
// a file that would be refused is refused.
func TestADryRunWritesNothing(t *testing.T) {
	source := testRoute(t)
	target := newTransferInstall(t)

	target.registerHost(t, passwordHost("198.51.100.1"))
	target.registerHost(t, passwordHost("198.51.100.2"))
	target.jump(t, "198.51.100.2", "198.51.100.1")

	before := target.configuration(t)
	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnelsWith(t, map[string]interface{}{
		"password":         testExportPassword,
		"account_password": testPassword,
		"file":             file,
		"dry_run":          true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("the dry run answered %d: %s", rec.Code, rec.Body.String())
	}

	var answered importedTunnels

	decodeTransfer(t, rec).into(t, &answered)

	want := importedTunnels{
		DryRun:  true,
		Current: transferCounts{Hosts: 2, JumpHosts: 1},
		File:    transferCounts{Hosts: 4, ServicePorts: 2, Assignments: 3, LocalForwards: 2, JumpHosts: 2},
	}
	if answered != want {
		t.Errorf("the dry run answered %+v, want %+v", answered, want)
	}

	if !reflect.DeepEqual(target.configuration(t), before) {
		t.Fatalf("the dry run changed what is stored")
	}

	if target.manager.count() != 0 || target.logs.FilterMessage("imported a tunnel configuration").Len() != 0 {
		t.Fatalf("the dry run woke the loop or was logged as an import")
	}

	rec = target.importTunnelsWith(t, map[string]interface{}{
		"password":         testExportPassword,
		"account_password": "not the password of the account",
		"file":             file,
		"dry_run":          true,
	})
	if rec.Code != http.StatusUnauthorized || errorCodeOf(t, rec) != errImportAccountPasswordWrong {
		t.Fatalf("a dry run with the wrong account password answered %d: %s", rec.Code, rec.Body.String())
	}

	broken := rewriteHosts(t, file, testExportPassword, func(host map[string]interface{}) {
		host["jump_host_ids"] = []interface{}{42}
	})

	rec = target.importTunnelsWith(t, map[string]interface{}{
		"password":         testExportPassword,
		"account_password": testPassword,
		"file":             broken,
		"dry_run":          true,
	})
	if rec.Code != http.StatusBadRequest || errorCodeOf(t, rec) != errImportJumpUnknownHost {
		t.Fatalf("a dry run of a file that is refused answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.configuration(t), before) {
		t.Fatalf("a refused dry run changed what is stored")
	}
}

// TestOnlyTheTunnelRowsOfAssignmentsTheFileCarriesAreKept is the state of the
// connections. A row whose assignment the file carries is left to the loop,
// which goes on writing the state of that tunnel into it or restarts it; one
// whose assignment is gone is deleted with it.
func TestOnlyTheTunnelRowsOfAssignmentsTheFileCarriesAreKept(t *testing.T) {
	source := testRoute(t)
	target := newTransferInstall(t)

	for _, row := range []models.Tunnel{
		{HostID: 1, SPID: 2, Status: "connected", Server: "a", Local: "b", Remote: "c"},
		{HostID: 3, SPID: 2, Status: "connected", Server: "a", Local: "b", Remote: "c"},
		{HostID: 7, SPID: 9, Status: "error", Server: "a", Local: "b", Remote: "c"},
	} {
		err := target.db.Create(&row).Error
		if err != nil {
			t.Fatalf("failed to store a tunnel row: %v", err)
		}
	}

	rec := target.importTunnels(t, source.exportTunnels(t, testExportPassword), testExportPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var kept []models.Tunnel

	err := target.db.Order("host_id, sp_id").Find(&kept).Error
	if err != nil {
		t.Fatalf("failed to read the tunnel rows: %v", err)
	}

	if len(kept) != 1 || kept[0].HostID != 1 || kept[0].SPID != 2 || kept[0].Status != "connected" {
		t.Fatalf("the tunnel rows after the import are %+v, want only the one of Host 1 and service port 2", kept)
	}
}
