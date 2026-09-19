package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
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

	err = db.AutoMigrate(&models.Host{}, &models.ServicePort{}, &settings.Settings{})
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
	left := map[string]bool{"ID": true, "CreatedAt": true, "UpdatedAt": true}

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
