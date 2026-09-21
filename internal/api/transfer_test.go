package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

func (m *transferWakes) GetAllTunnels() (*[]models.Tunnel, error) {
	return &[]models.Tunnel{}, nil
}

func (m *transferWakes) GetHostTunnels(hostID uint) (*[]models.Tunnel, error) {
	return &[]models.Tunnel{}, nil
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
}

func newTransferInstall(t *testing.T) *transferInstall {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&models.Host{}, &models.ServicePort{}, &models.HostServicePort{},
		&settings.Settings{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	manager := &transferWakes{}
	cipher := newTestCipher(t)

	return &transferInstall{
		db:      db,
		cipher:  cipher,
		handler: NewTransferHandler(NewHandler(db, manager, logger, cipher), "0.0.0-test"),
		manager: manager,
		logs:    logs,
	}
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
// wrote.
func (i *transferInstall) call(t *testing.T, handler func(echo.Context) error, body string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()
	e.Validator = &testValidator{validator: validator.New()}

	req := httptest.NewRequest(http.MethodPost, "/api/transfer", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	err := handler(e.NewContext(req, rec))
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
		IP:            host.IP,
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
		ServiceIP:   sp.ServiceIP,
		ServicePort: sp.ServicePort,
		LocalPort:   sp.LocalPort,
		Description: sp.Description,
	}

	err := i.db.Create(&stored).Error
	if err != nil {
		t.Fatalf("failed to store the service port: %v", err)
	}
}

// exportTunnels runs the export and hands back the sealed file.
func (i *transferInstall) exportTunnels(t *testing.T, password string) string {
	t.Helper()

	rec := i.call(t, i.handler.ExportTunnels, `{"password":`+jsonString(t, password)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the export answered %d: %s", rec.Code, rec.Body.String())
	}

	var exported exportedTunnels

	decodeTransfer(t, rec).into(t, &exported)

	return exported.File
}

// importTunnels runs the import of a file and hands back what it answered.
func (i *transferInstall) importTunnels(t *testing.T, file string, password string,
	overwrite bool) *httptest.ResponseRecorder {
	t.Helper()

	body := `{"password":` + jsonString(t, password) + `,"file":` + jsonString(t, file) +
		`,"overwrite":` + map[bool]string{true: "true", false: "false"}[overwrite] + `}`

	return i.call(t, i.handler.ImportTunnels, body)
}

// twoHosts is the pair every test that moves a configuration starts from: one
// Host that is logged in to with a key, one with a password. The two ways in
// are sealed differently and have to survive the trip in the same file.
func twoHosts(t *testing.T) (withKey hostContent, withPassword hostContent) {
	t.Helper()

	withKey = hostContent{
		IP:            "192.0.2.10",
		Port:          22,
		User:          "operator",
		PrivateKey:    testPrivateKeyPEM(t, "the passphrase of the key"),
		KeyPassphrase: "the passphrase of the key",
		Description:   "the Host with a key",
		Enabled:       true,
	}

	withPassword = hostContent{
		IP:          "192.0.2.11",
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
// The row id and the time the row was written are the two that are left out on
// purpose: both describe the row an export was read from.
func TestTheSettingsContentCarriesEverySetting(t *testing.T) {
	left := map[string]bool{"ID": true, "UpdatedAt": true}

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
// above does for the settings.
func TestTheHostContentCarriesEveryFieldOfAHost(t *testing.T) {
	// PendingHostKey is the one field left out that is not a column about the
	// row itself. It is the key some server presented on a connection this
	// installation was refused on, waiting for a person to say whether it is
	// the right one, so it describes a connection rather than the
	// configuration. Carried in a file, it would ask the installation that
	// imports it to approve a key presented to a machine it is not, about a
	// server it has never spoken to. HostKey, the key already approved, is
	// carried: see hostContent.
	left := map[string]bool{
		"ID": true, "CreatedAt": true, "UpdatedAt": true, "PendingHostKey": true,
	}

	stored := reflect.TypeOf(models.Host{})
	carried := reflect.TypeOf(hostContent{})

	for i := 0; i < stored.NumField(); i++ {
		name := stored.Field(i).Name
		if left[name] {
			continue
		}

		_, found := carried.FieldByName(name)
		if !found {
			t.Errorf("models.Host has %s and hostContent does not, so it is not carried by an export", name)
		}
	}
}

// TestAnExportedFileHoldsNoSecretInTheClear is the rule the whole format rests
// on: the SSH password, the private key and its passphrase are inside the file,
// and the file is one sealed string, so none of the three can be read off it
// without the password.
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

	if strings.Contains(opened, `"id"`) || strings.Contains(opened, `"created_at"`) ||
		strings.Contains(opened, `"updated_at"`) {
		t.Errorf("the file carries the row ids or the timestamps of the installation it came from")
	}
}

// TestAnExportIsRefusedWithoutAPasswordThatHolds keeps the file to the length
// the account is held to, since the file carries the credentials of every Host
// for as long as it is kept.
func TestAnExportIsRefusedWithoutAPasswordThatHolds(t *testing.T) {
	source := newTransferInstall(t)

	for _, password := range []string{"", "short", strings.Repeat("a", maxPasswordBytes+1)} {
		rec := source.call(t, source.handler.ExportTunnels, `{"password":`+jsonString(t, password)+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("the export of a %d byte password answered %d, want %d",
				len(password), rec.Code, http.StatusBadRequest)
		}
	}
}

// TestAnExportedConfigurationIsReadableOnAnotherInstallation is what the format
// exists for. The secrets are sealed in the database with a key that stays on
// the machine, so the check is not that the import answered but that the row it
// wrote opens with the key of the installation that took it in and holds the
// plaintext the other installation had.
func TestAnExportedConfigurationIsReadableOnAnotherInstallation(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)
	source.registerServicePort(t, servicePortContent{
		ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "a service",
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

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	if imported.Added != 3 || imported.Skipped != 0 || imported.Replaced != 0 {
		t.Fatalf("the import added %d, replaced %d and skipped %d, want 3 added",
			imported.Added, imported.Replaced, imported.Skipped)
	}

	for _, want := range []hostContent{withKey, withPassword} {
		var stored models.Host

		err = target.db.Where("ip = ?", want.IP).First(&stored).Error
		if err != nil {
			t.Fatalf("the Host %s was not stored: %v", want.IP, err)
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
						secret.what, want.IP)
				}

				continue
			}

			if !crypto.IsEncrypted(secret.stored) {
				t.Errorf("the %s of the Host %s is not sealed with the key of this installation",
					secret.what, want.IP)
			}

			opened, err := target.cipher.Decrypt(secret.stored)
			if err != nil {
				t.Fatalf("the %s of the Host %s does not open with the key of this installation: %v",
					secret.what, want.IP, err)
			}

			if strings.TrimSpace(opened) != strings.TrimSpace(secret.want) {
				t.Errorf("the %s of the Host %s came across altered", secret.what, want.IP)
			}
		}

		if stored.Port != want.Port || stored.User != want.User ||
			stored.Description != want.Description || stored.Enabled != want.Enabled {
			t.Errorf("the Host %s came across with other fields than it was exported with", want.IP)
		}
	}

	var sp models.ServicePort

	err = target.db.Where("local_port = ?", 18080).First(&sp).Error
	if err != nil {
		t.Fatalf("the service port was not stored: %v", err)
	}

	if sp.ServiceIP != "192.0.2.20" || sp.ServicePort != 80 {
		t.Errorf("the service port came across as %s:%d", sp.ServiceIP, sp.ServicePort)
	}

	if target.manager.count() != 1 {
		t.Errorf("the import asked for %d reconcile passes, want 1", target.manager.count())
	}
}

// TestTheSameFileImportedTwiceSkipsEverything is what a file being imported
// twice has to do: the second time nothing is added and every row is reported
// as skipped, with the reason the operator decides about an overwrite from.
func TestTheSameFileImportedTwiceSkipsEverything(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)
	source.registerServicePort(t, servicePortContent{
		ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18080,
	})

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the first import answered %d: %s", rec.Code, rec.Body.String())
	}

	rec = target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the second import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	if imported.Added != 0 || imported.Replaced != 0 || imported.Skipped != 3 {
		t.Fatalf("the second import added %d, replaced %d and skipped %d, want 3 skipped",
			imported.Added, imported.Replaced, imported.Skipped)
	}

	for _, item := range imported.Items {
		if item.Action != transferSkipped {
			t.Errorf("the item %s was %s on the second import", item.Name, item.Action)
		}

		if item.Reason == "" {
			t.Errorf("the item %s was skipped without saying why", item.Name)
		}
	}

	if target.count(t, &models.Host{}) != 2 ||
		target.count(t, &models.ServicePort{}) != 1 {
		t.Fatalf("the second import wrote rows of its own")
	}
}

// TestAnImportWithOverwriteReplacesTheStoredRows is the other mode: the rows
// that stood in the way are replaced, ids and all, and the values that were
// stored here are gone.
func TestAnImportWithOverwriteReplacesTheStoredRows(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)
	source.registerHost(t, withKey)
	source.registerHost(t, withPassword)
	source.registerServicePort(t, servicePortContent{
		ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "as exported",
	})

	file := source.exportTunnels(t, testExportPassword)

	// The target holds the same rows with other values, which is what an
	// overwrite has to put right.
	stale := withPassword
	stale.Port = 2222
	stale.User = "somebody-else"
	stale.Password = "the password that is stored here"
	stale.Description = "as stored here"
	stale.Enabled = false
	staleHost := target.registerHost(t, stale)
	target.registerServicePort(t, servicePortContent{
		ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "as stored here",
	})

	rec := target.importTunnels(t, file, testExportPassword, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	if imported.Added != 1 || imported.Replaced != 2 || imported.Skipped != 0 {
		t.Fatalf("the import added %d, replaced %d and skipped %d, want 1 added and 2 replaced",
			imported.Added, imported.Replaced, imported.Skipped)
	}

	var replaced models.Host

	err := target.db.Where("ip = ?", withPassword.IP).First(&replaced).Error
	if err != nil {
		t.Fatalf("the Host is gone: %v", err)
	}

	if replaced.ID != staleHost.ID {
		t.Errorf("the replaced Host was written as a new row, so the tunnels of the old one are orphaned")
	}

	if replaced.Port != withPassword.Port || replaced.User != withPassword.User ||
		replaced.Description != withPassword.Description || !replaced.Enabled {
		t.Errorf("the Host was not replaced with what the file carried: %+v", replaced)
	}

	opened, err := target.cipher.Decrypt(replaced.Password)
	if err != nil {
		t.Fatalf("the password of the replaced Host does not open: %v", err)
	}

	if opened != withPassword.Password {
		t.Errorf("the password of the replaced Host is not the one the file carried")
	}

	var sp models.ServicePort

	err = target.db.Where("local_port = ?", 18080).First(&sp).Error
	if err != nil {
		t.Fatalf("the service port is gone: %v", err)
	}

	if sp.Description != "as exported" {
		t.Errorf("the service port was not replaced: its description is %q", sp.Description)
	}

	if target.count(t, &models.Host{}) != 2 ||
		target.count(t, &models.ServicePort{}) != 1 {
		t.Fatalf("the overwrite left rows behind")
	}
}

// TestAnImportThatIsRefusedWritesNothing is the transaction. The second Host of
// the file cannot be stored, and what the import has to leave behind is not the
// first one but the database as it was.
func TestAnImportThatIsRefusedWritesNothing(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, withPassword := twoHosts(t)

	// The file is built by hand rather than exported, because what is being
	// tested is a file an export would never write: the port of the second Host
	// is out of range, which the rules of a create refuse.
	broken := withPassword
	broken.Port = 70000

	file := sealedTunnelsFile(t, source, tunnelsContent{
		Hosts: []hostContent{withKey, broken},
		ServicePorts: []servicePortContent{
			{ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18080},
		},
	}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	answer := decodeTransfer(t, rec)
	if !strings.Contains(answer.Error, broken.IP) {
		t.Errorf("the refusal does not name the Host that stopped the import: %q", answer.Error)
	}

	if target.count(t, &models.Host{}) != 0 {
		t.Fatalf("the Host that came before the refused one was left in the database")
	}

	if target.count(t, &models.ServicePort{}) != 0 {
		t.Fatalf("a service port was left in the database")
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
		Hosts: []hostContent{{IP: "192.0.2.10", Port: 22, User: "operator", Enabled: true}},
	}, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword, false)
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

// TestAServicePortMeetingTwoStoredRowsIsRefused is the one conflict an
// overwrite cannot settle: the service address belongs to one stored row and
// the local port to another, so replacing either leaves the other breaking the
// rule it is under.
func TestAServicePortMeetingTwoStoredRowsIsRefused(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	file := sealedTunnelsFile(t, source, tunnelsContent{
		ServicePorts: []servicePortContent{
			{ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18080},
		},
	}, testExportPassword)

	target.registerServicePort(t, servicePortContent{
		ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18081,
	})
	target.registerServicePort(t, servicePortContent{
		ServiceIP: "192.0.2.21", ServicePort: 80, LocalPort: 18080,
	})

	rec := target.importTunnels(t, file, testExportPassword, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("the import answered %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	if target.count(t, &models.ServicePort{}) != 2 {
		t.Fatalf("the refused import changed the stored service ports")
	}

	// Without the overwrite the same file is skipped rather than refused, and
	// the reason names the rule that stood in the way.
	rec = target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	if imported.Skipped != 1 || imported.Added != 0 {
		t.Fatalf("the import added %d and skipped %d, want 1 skipped", imported.Added, imported.Skipped)
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
		{"a wrong password", target.importTunnels(t, file, "another password entirely", false)},
		{"not a file of ours", target.importTunnels(t, "just some text", testExportPassword, false)},
		{"a damaged file", target.importTunnels(t, damaged, testExportPassword, false)},
		{"the other kind", target.importTunnels(t, settingsFile, testExportPassword, false)},
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
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`)
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

	rec := target.importTunnels(t, settingsFile, testExportPassword, false)
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

	rec := source.importTunnels(t, file, testExportPassword, false)
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

	rec := i.call(t, i.handler.ExportSettings, `{"password":`+jsonString(t, password)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the export answered %d: %s", rec.Code, rec.Body.String())
	}

	var exported exportedSettings

	decodeTransfer(t, rec).into(t, &exported)

	return exported.File
}

// sealedTunnelsFile builds a file the way an export builds it, for the tests
// that need a content no export would write.
func sealedTunnelsFile(t *testing.T, i *transferInstall, content tunnelsContent, password string) string {
	t.Helper()

	file, err := i.handler.seal(transferKindTunnels, content, password, time.Now())
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

	err = settings.Save(source.db, stored)
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
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`)
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
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`)
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
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`)
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

	rec := target.importTunnels(t, sealed, testExportPassword, false)
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

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	secrets := map[string]string{
		"the SSH password":     withPassword.Password,
		"the private key":      strings.Split(strings.TrimSpace(withKey.PrivateKey), "\n")[1],
		"the key passphrase":   withKey.KeyPassphrase,
		"the sealing password": testExportPassword,
	}

	// The answer of the import says what was written and nothing of what is in
	// the rows. The answer of the export carries the file, which is sealed, so
	// it is read for the secrets in the clear rather than left out.
	answers := map[string]string{
		"the answer of the import": rec.Body.String(),
		"the answer of the export": source.call(t, source.handler.ExportTunnels,
			`{"password":`+jsonString(t, testExportPassword)+`}`).Body.String(),
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
		IP:          "192.0.2.30",
		Port:        22,
		User:        "operator",
		Password:    "the password of the Host",
		Description: "the Host somebody turned off",
		Enabled:     false,
	}
	on := hostContent{
		IP:          "192.0.2.31",
		Port:        22,
		User:        "operator",
		Password:    "the password of the Host",
		Description: "the Host that is in use",
		Enabled:     true,
	}

	source.registerHost(t, off)
	source.registerHost(t, on)

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	for _, want := range []hostContent{off, on} {
		var stored models.Host

		err := target.db.Where("ip = ?", want.IP).First(&stored).Error
		if err != nil {
			t.Fatalf("the Host %s did not arrive: %v", want.IP, err)
		}
		if stored.Enabled != want.Enabled {
			t.Errorf("the Host %s arrived with enabled %v, want %v",
				want.IP, stored.Enabled, want.Enabled)
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

	err := i.db.Model(&models.Host{}).Where("ip = ?", hostIP).
		Update("pending_host_key", key).Error
	if err != nil {
		t.Fatalf("failed to store the pending key of the Host %s: %v", hostIP, err)
	}
}

// TestTheTrustedHostKeyComesAcrossAndThePendingOneDoesNot is the round trip of
// the two keys of a Host.
//
// The approved key is what makes a Host reachable: a tunnel is built only to a
// server that presents it, so an installation that took the Hosts in without
// it would connect to none of them until a person had approved every server
// again. The key waiting for approval is the other way round. It is what some
// server presented to the installation the file came from, on a connection
// that was refused, so on the installation reading the file it stands for a
// conversation that never happened there.
func TestTheTrustedHostKeyComesAcrossAndThePendingOneDoesNot(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, _ := twoHosts(t)
	withKey.HostKey = testTrustedHostKey

	source.registerHost(t, withKey)
	source.presentedAKeyNobodyHasApproved(t, withKey.IP, testPresentedHostKey)

	file := source.exportTunnels(t, testExportPassword)

	// Opened rather than read as JSON: the pending key must not be anywhere in
	// the content, under whatever name.
	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the exported file does not open: %v", err)
	}

	if !strings.Contains(opened, testTrustedHostKey) {
		t.Errorf("the file does not carry the key the Host is trusted on")
	}

	if strings.Contains(opened, testPresentedHostKey) {
		t.Errorf("the file carries the key the Host is waiting for an approval of")
	}

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var stored models.Host

	err = target.db.Where("ip = ?", withKey.IP).First(&stored).Error
	if err != nil {
		t.Fatalf("the Host %s did not arrive: %v", withKey.IP, err)
	}

	if stored.HostKey != testTrustedHostKey {
		t.Errorf("the Host arrived trusted on %q, want %q", stored.HostKey, testTrustedHostKey)
	}

	if stored.PendingHostKey != "" {
		t.Errorf("the Host arrived waiting for an approval of %q, want none", stored.PendingHostKey)
	}
}

// TestImportingOverAHostDropsTheKeyItWasWaitingOnApprovalFor is the same two
// keys on a row that is already here.
//
// The Host on this installation is trusted on one key and has been presented
// another, so somebody is being asked whether the server changed. The file
// then says what the key is. Approving the pending one after that would put
// back a key the file did not name, and the question it stands for was asked
// about the trust the row held before, so the import drops it. Nothing is lost
// by that: a server that still presents something else writes the pending key
// again on the next connection.
func TestImportingOverAHostDropsTheKeyItWasWaitingOnApprovalFor(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	withKey, _ := twoHosts(t)
	withKey.HostKey = testTrustedHostKey
	source.registerHost(t, withKey)

	here := withKey
	here.HostKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCtheKeyThisInstallationTrusted"
	target.registerHost(t, here)
	target.presentedAKeyNobodyHasApproved(t, here.IP, testPresentedHostKey)

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	if imported.Replaced != 1 {
		t.Fatalf("the import replaced %d Hosts, want 1", imported.Replaced)
	}

	var stored models.Host

	err := target.db.Where("ip = ?", withKey.IP).First(&stored).Error
	if err != nil {
		t.Fatalf("the Host %s is no longer stored: %v", withKey.IP, err)
	}

	if stored.HostKey != testTrustedHostKey {
		t.Errorf("the Host is trusted on %q, want the %q the file carried",
			stored.HostKey, testTrustedHostKey)
	}

	if stored.PendingHostKey != "" {
		t.Errorf("the Host is still waiting for an approval of %q, want none",
			stored.PendingHostKey)
	}
}

// assign makes a Host carry a service port, the way an installation stores it:
// one row holding the two ids of this installation.
func (i *transferInstall) assign(t *testing.T, hostIP string, localPort int) {
	t.Helper()

	var host models.Host

	err := i.db.Where("ip = ?", hostIP).First(&host).Error
	if err != nil {
		t.Fatalf("the Host %s is not registered here: %v", hostIP, err)
	}

	var sp models.ServicePort

	err = i.db.Where("local_port = ?", localPort).First(&sp).Error
	if err != nil {
		t.Fatalf("no service port is on the local port %d here: %v", localPort, err)
	}

	err = i.db.Create(&models.HostServicePort{HostID: host.ID, SPID: sp.ID}).Error
	if err != nil {
		t.Fatalf("failed to store the assignment: %v", err)
	}
}

// carried is every assignment stored, as "<Host IP> carries <local port>". The
// ids differ between two installations and say nothing to whoever reads a
// failure, so the pairs are named by what means the same on both sides, which
// is what the file carries them as.
func (i *transferInstall) carried(t *testing.T) []string {
	t.Helper()

	var rows []struct {
		IP        string
		LocalPort int
	}

	err := i.db.Model(&models.HostServicePort{}).
		Select("hosts.ip AS ip, service_ports.local_port AS local_port").
		Joins("JOIN hosts ON hosts.id = host_service_ports.host_id").
		Joins("JOIN service_ports ON service_ports.id = host_service_ports.sp_id").
		Order("hosts.ip, service_ports.local_port").
		Scan(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the assignments: %v", err)
	}

	pairs := make([]string, 0, len(rows))
	for _, row := range rows {
		pairs = append(pairs, row.IP+" carries "+strconv.Itoa(row.LocalPort))
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
		{ServiceIP: "192.0.2.20", ServicePort: 80, LocalPort: 18080, Description: "the first service"},
		{ServiceIP: "192.0.2.21", ServicePort: 443, LocalPort: 18081, Description: "the second service"},
		{ServiceIP: "192.0.2.22", ServicePort: 5432, LocalPort: 18082, Description: "the third service"},
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

	source.assign(t, withKey.IP, 18080)
	source.assign(t, withKey.IP, 18081)
	source.assign(t, withPassword.IP, 18081)
	source.assign(t, withPassword.IP, 18082)

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

// TestTheServicePortsAHostCarriesCrossToAnotherInstallation is what the
// assignments are in the file for. Which service ports a Host carries is what
// the operator set up, so an installation that took the file in and runs
// something else is one that has to be set up by hand all over again.
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

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.carried(t), fourPairs) {
		t.Fatalf("the installation that took the file in carries %v, want %v",
			target.carried(t), fourPairs)
	}

	// What the file names them by is the other half of it. Carried as the ids
	// of the source, the pairs above would still be four and would point at
	// whatever holds those ids here.
	opened, err := crypto.DecryptWithPassword(file, testExportPassword)
	if err != nil {
		t.Fatalf("the file does not open: %v", err)
	}

	if !strings.Contains(opened, `"assigned_local_ports":[18080,18081]`) {
		t.Errorf("the file does not name the assignments by their local port: %s", opened)
	}

	if strings.Contains(opened, `"sp_id"`) || strings.Contains(opened, `"host_id"`) {
		t.Errorf("the file carries the row ids of the installation it came from: %s", opened)
	}
}

// TestAFileFromBeforeTheAssignmentsWereStoredCarriesEverything is the one that
// keeps an upgrade from taking every tunnel down. A file exported before the
// assignments existed does not name them, and what it meant is what every
// installation ran on then: every Host carries every service port. Read as "the
// Host carries nothing", such a file imports without a word and leaves the
// installation with no tunnel at all.
func TestAFileFromBeforeTheAssignmentsWereStoredCarriesEverything(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	older := rewriteHosts(t, source.exportTunnels(t, testExportPassword), testExportPassword,
		func(host map[string]interface{}) {
			delete(host, "assigned_local_ports")
		})

	rec := target.importTunnels(t, older, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.carried(t), everyPair) {
		t.Fatalf("a file that does not name the assignments left the installation carrying %v, want %v",
			target.carried(t), everyPair)
	}
}

// TestAHostThatIsCarriedAsCarryingNothingCarriesNothing is the other side of
// the test above, and the two are what the difference between an absent field
// and an empty list is for. The operator who took every service port off a Host
// asked for that, and an import that filled them back in would undo it.
func TestAHostThatIsCarriedAsCarryingNothingCarriesNothing(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	emptied := rewriteHosts(t, source.exportTunnels(t, testExportPassword), testExportPassword,
		func(host map[string]interface{}) {
			host["assigned_local_ports"] = []interface{}{}
		})

	rec := target.importTunnels(t, emptied, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if len(target.carried(t)) != 0 {
		t.Fatalf("a file whose Hosts carry nothing left the installation carrying %v",
			target.carried(t))
	}

	// The Hosts and the service ports themselves came across all the same. An
	// empty list is about what a Host carries and about nothing else.
	if target.count(t, &models.Host{}) != 2 || target.count(t, &models.ServicePort{}) != 3 {
		t.Fatalf("the file left %d Hosts and %d service ports, want 2 and 3",
			target.count(t, &models.Host{}), target.count(t, &models.ServicePort{}))
	}
}

// TestAnAssignmentToAServicePortThatIsNotHereIsSkipped pins what is done with a
// local port the file names and this installation does not hold: the assignment
// is left out and said so in the answer, and the rest of the file is imported.
// It is not refused, because the Hosts and the service ports that are fine
// would go down with it, and it is not passed over in silence, because then
// nobody would know which Host came up carrying less than the file said.
func TestAnAssignmentToAServicePortThatIsNotHereIsSkipped(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	// The service port on 19999 is in no file and in neither installation, so
	// the import has nothing to point the assignment at.
	withOne := rewriteHosts(t, source.exportTunnels(t, testExportPassword), testExportPassword,
		func(host map[string]interface{}) {
			ports, ok := host["assigned_local_ports"].([]interface{})
			if !ok {
				t.Fatalf("the exported file does not name the assignments of a Host")
			}

			host["assigned_local_ports"] = append(ports, float64(19999))
		})

	rec := target.importTunnels(t, withOne, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	var imported importedTunnels

	decodeTransfer(t, rec).into(t, &imported)

	if !reflect.DeepEqual(target.carried(t), fourPairs) {
		t.Fatalf("the installation carries %v, want the four pairs the file could be followed on: %v",
			target.carried(t), fourPairs)
	}

	skipped := make([]string, 0, len(imported.Items))

	for _, item := range imported.Items {
		if item.Kind != "assignment" {
			continue
		}

		if item.Action != transferSkipped || item.Reason == "" {
			t.Errorf("the assignment %q was reported as %q with the reason %q",
				item.Name, item.Action, item.Reason)
		}

		skipped = append(skipped, item.Name)
	}

	want := []string{"192.0.2.10 carries 19999", "192.0.2.11 carries 19999"}
	if !reflect.DeepEqual(skipped, want) {
		t.Fatalf("the import reported the assignments %v as skipped, want %v", skipped, want)
	}

	if imported.Skipped != 2 {
		t.Errorf("the import counted %d skipped, want the 2 assignments it could not follow",
			imported.Skipped)
	}
}

// TestTheAssignmentsOfASkippedHostAreLeftAlone holds the assignments to the
// rule the rest of the import is under. A Host that is registered here already
// is skipped without an overwrite, and what it carries is part of that Host: an
// import that left the Host as it was and moved what it carries under it would
// be a change nobody asked for.
func TestTheAssignmentsOfASkippedHostAreLeftAlone(t *testing.T) {
	source := partlyAssigned(t)
	target := newTransferInstall(t)

	withKey, _ := twoHosts(t)
	target.registerHost(t, withKey)
	threeServicePorts(t, target)
	target.assign(t, withKey.IP, 18082)

	file := source.exportTunnels(t, testExportPassword)

	rec := target.importTunnels(t, file, testExportPassword, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	// The Host that was skipped goes on carrying the one service port it was
	// given here, and the Host the file brought in carries what the file says.
	want := []string{
		"192.0.2.10 carries 18082",
		"192.0.2.11 carries 18081",
		"192.0.2.11 carries 18082",
	}

	if !reflect.DeepEqual(target.carried(t), want) {
		t.Fatalf("after the import the installation carries %v, want %v", target.carried(t), want)
	}

	// With the overwrite the Host is written, and then what it carries is what
	// the file says rather than the two sets put together.
	rec = target.importTunnels(t, file, testExportPassword, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("the import with overwrite answered %d: %s", rec.Code, rec.Body.String())
	}

	if !reflect.DeepEqual(target.carried(t), fourPairs) {
		t.Fatalf("after the overwrite the installation carries %v, want %v",
			target.carried(t), fourPairs)
	}
}
