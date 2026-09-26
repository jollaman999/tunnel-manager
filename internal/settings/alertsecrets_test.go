package settings

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"gorm.io/gorm"
)

// newCipher returns a cipher on a key made of one byte, so that a test that
// needs two installations gets two keys.
func newCipher(t *testing.T, b byte) *crypto.Cipher {
	t.Helper()

	c, err := crypto.NewCipher(bytes.Repeat([]byte{b}, crypto.KeySize))
	if err != nil {
		t.Fatalf("failed to create a cipher: %v", err)
	}

	return c
}

// alertsOn is a valid set with the webhook and mail switched on and every one
// of the six sealed settings away from its default.
func alertsOn() Settings {
	s := mailOn()
	s.AlertWebhookURL = "https://hooks.example.com/services/T000/B000/secret-token"
	s.SMTPPort = 587
	s.SMTPSecurity = SMTPSecurityStartTLS

	return s
}

// plainValues are the sealed settings of alertsOn that are text, which are
// what a copy of the database file must not show.
func plainValues() []string {
	s := alertsOn()

	return []string{s.AlertWebhookURL, s.SMTPHost, s.SMTPUsername, s.SMTPFrom, s.SMTPTo}
}

// rawRow reads the settings row as the database holds it, every column as
// text.
func rawRow(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()

	rows, err := db.Raw("SELECT * FROM settings WHERE id = ?", settingsID).Rows()
	if err != nil {
		t.Fatalf("failed to read the row: %v", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("failed to read the columns: %v", err)
	}

	if !rows.Next() {
		t.Fatal("no settings row is stored")
	}

	values := make([]interface{}, len(columns))
	pointers := make([]interface{}, len(columns))

	for i := range values {
		pointers[i] = &values[i]
	}

	err = rows.Scan(pointers...)
	if err != nil {
		t.Fatalf("failed to scan the row: %v", err)
	}

	row := map[string]string{}

	for i, column := range columns {
		switch v := values[i].(type) {
		case nil:
			row[column] = ""
		case []byte:
			row[column] = string(v)
		default:
			row[column] = fmt.Sprint(v)
		}
	}

	return row
}

// requireNoPlainValue fails when any column of the row holds one of the
// sealed settings in the clear.
func requireNoPlainValue(t *testing.T, row map[string]string, plain []string) {
	t.Helper()

	for column, stored := range row {
		for _, value := range plain {
			if value != "" && strings.Contains(stored, value) {
				t.Errorf("column %s holds %q in the clear: %q", column, value, stored)
			}
		}
	}
}

// TestTheAlertSecretsAreStoredSealed saves the six and reads the row the
// way a copy of the database file would be read.
func TestTheAlertSecretsAreStoredSealed(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	_, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s := alertsOn()

	err = Save(db, &s, cipher)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	row := rawRow(t, db)

	requireNoPlainValue(t, row, plainValues())

	if !crypto.IsEncrypted(row["alert_secrets"]) {
		t.Fatalf("alert_secrets is stored as %q, which is not sealed", row["alert_secrets"])
	}

	// A database made by this version has no column for any of the six.
	for _, column := range legacyAlertColumns {
		if _, found := row[column]; found {
			t.Errorf("a fresh database has the column %s", column)
		}
	}

	// Load alone leaves them sealed and the six at their defaults.
	closed, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if closed.alertSecrets() != defaultAlertSecrets() || closed.AlertSecrets == "" {
		t.Fatalf("Load opened the sealed settings: %+v", closed.alertSecrets())
	}

	opened, err := LoadOpened(db, cipher)
	if err != nil {
		t.Fatalf("LoadOpened: %v", err)
	}

	want := alertsOn()
	if opened.alertSecrets() != want.alertSecrets() {
		t.Fatalf("the sealed settings open to %+v, want %+v", opened.alertSecrets(), want.alertSecrets())
	}

	if opened.AlertSecrets != "" {
		t.Fatal("an opened set still holds the sealed value")
	}

	if opened.SMTPSecurity != SMTPSecurityStartTLS {
		t.Fatalf("the connection security reads back as %q", opened.SMTPSecurity)
	}
}

// TestTheDefaultsAreStoredWithoutAKey is what a first startup and a reset
// do: they store before the key is loaded.
func TestTheDefaultsAreStoredWithoutAKey(t *testing.T) {
	db := newDB(t)

	d := Defaults()

	err := Save(db, &d, nil)
	if err != nil {
		t.Fatalf("Save of the defaults without a key: %v", err)
	}

	if row := rawRow(t, db); row["alert_secrets"] != "" {
		t.Fatalf("the defaults are stored as %q", row["alert_secrets"])
	}

	s := alertsOn()

	err = Save(db, &s, nil)
	if err == nil {
		t.Fatal("a set with something to seal was stored without a key")
	}
}

// TestASetReadWithoutTheKeyIsNotStored is a caller that read the settings
// with Load and saved them: storing that set would seal the defaults over the
// six that are stored.
func TestASetReadWithoutTheKeyIsNotStored(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	s := alertsOn()

	err := Save(db, &s, cipher)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	sealed := rawRow(t, db)["alert_secrets"]

	closed, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = Save(db, closed, cipher)
	if !errors.Is(err, ErrAlertSecretsNotOpened) {
		t.Fatalf("Save of a set that was not opened = %v, want %v", err, ErrAlertSecretsNotOpened)
	}

	if rawRow(t, db)["alert_secrets"] != sealed {
		t.Fatal("the refused save changed the sealed settings")
	}
}

// TestAWrongKeyFailsTheRead is a key file that is not the one the settings
// were sealed with. The read fails rather than coming back with mail off.
func TestAWrongKeyFailsTheRead(t *testing.T) {
	db := newDB(t)

	s := alertsOn()

	err := Save(db, &s, newCipher(t, 1))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err = LoadOpened(db, newCipher(t, 2))
	if !errors.Is(err, ErrAlertSecretsDoNotOpen) || !errors.Is(err, crypto.ErrWrongKey) {
		t.Fatalf("LoadOpened with another key = %v, want %v and %v", err, ErrAlertSecretsDoNotOpen, crypto.ErrWrongKey)
	}

	_, err = LoadOpened(db, nil)
	if !errors.Is(err, ErrAlertSecretsDoNotOpen) {
		t.Fatalf("LoadOpened with no key = %v, want %v", err, ErrAlertSecretsDoNotOpen)
	}
}

// TestADiffOfTheSealedSettingsIsMasked holds the changes a save answers with
// and a reset logs to the mask for the six, while still listing them.
func TestADiffOfTheSealedSettingsIsMasked(t *testing.T) {
	before := Defaults()
	after := alertsOn()

	changes := Diff(&before, &after)

	listed := map[string]bool{}

	for _, change := range changes {
		listed[change.Name] = true

		if !sealedSettings[change.Name] {
			continue
		}

		if change.To != SecretMask {
			t.Errorf("%s is reported as changing to %q, want the mask", change.Name, change.To)
		}

		if change.From != "" && change.From != SecretMask {
			t.Errorf("%s is reported as changing from %q, want the mask or nothing", change.Name, change.From)
		}
	}

	for name := range sealedSettings {
		if !listed[name] {
			t.Errorf("a change of %s is not listed", name)
		}
	}

	other := alertsOn()
	other.SMTPHost = "mail.example.net"

	changes = Diff(&after, &other)
	if len(changes) != 1 || changes[0].Name != "alert.smtp.host" || changes[0].From != SecretMask ||
		changes[0].To != SecretMask {
		t.Fatalf("a changed mail server is reported as %+v", changes)
	}

	if Diff(&after, &after) != nil {
		t.Fatal("a set is reported as differing from itself")
	}
}

// addLegacyColumns gives the table the six columns the version before this
// one stored the settings in, in the clear, and fills them in.
func addLegacyColumns(t *testing.T, db *gorm.DB, s Settings) {
	t.Helper()

	for _, ddl := range []string{
		"ALTER TABLE settings ADD COLUMN alert_webhook_url text",
		"ALTER TABLE settings ADD COLUMN smtp_host text",
		"ALTER TABLE settings ADD COLUMN smtp_port integer DEFAULT 587",
		"ALTER TABLE settings ADD COLUMN smtp_username text",
		"ALTER TABLE settings ADD COLUMN smtp_from text",
		"ALTER TABLE settings ADD COLUMN smtp_to text",
	} {
		err := db.Exec(ddl).Error
		if err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	err := db.Exec("UPDATE settings SET alert_webhook_url = ?, smtp_host = ?, smtp_port = ?, "+
		"smtp_username = ?, smtp_from = ?, smtp_to = ? WHERE id = ?",
		s.AlertWebhookURL, s.SMTPHost, s.SMTPPort, s.SMTPUsername, s.SMTPFrom, s.SMTPTo, settingsID).Error
	if err != nil {
		t.Fatalf("failed to fill the legacy columns: %v", err)
	}
}

// TestTheColumnsInTheClearAreSealed is a database written by the version
// before this one, which held the six in columns of their own.
func TestTheColumnsInTheClearAreSealed(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	// The modes are stored by this version's Save, and the six are what the
	// older version left in its columns.
	legacy := alertsOn()
	modes := Defaults()
	modes.SMTPSecurity = legacy.SMTPSecurity
	modes.SMTPAuth = legacy.SMTPAuth

	err := Save(db, &modes, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	addLegacyColumns(t, db, legacy)

	// The startup reads the settings before the key is loaded, and the
	// columns do not stand in its way.
	_, err = Load(db)
	if err != nil {
		t.Fatalf("Load before the columns are sealed: %v", err)
	}

	changed, err := SealLegacyAlertColumns(db, cipher)
	if err != nil || !changed {
		t.Fatalf("SealLegacyAlertColumns = %v, %v, want true", changed, err)
	}

	row := rawRow(t, db)

	requireNoPlainValue(t, row, plainValues())

	if row["smtp_port"] != "465" {
		t.Errorf("the port column is left as %q", row["smtp_port"])
	}

	opened, err := LoadOpened(db, cipher)
	if err != nil {
		t.Fatalf("LoadOpened: %v", err)
	}

	if opened.alertSecrets() != legacy.alertSecrets() {
		t.Fatalf("the sealed settings open to %+v, want %+v", opened.alertSecrets(), legacy.alertSecrets())
	}

	// A second run finds nothing to do and leaves the sealed value alone.
	sealed := row["alert_secrets"]

	changed, err = SealLegacyAlertColumns(db, cipher)
	if err != nil || changed {
		t.Fatalf("a second SealLegacyAlertColumns = %v, %v, want false", changed, err)
	}

	if rawRow(t, db)["alert_secrets"] != sealed {
		t.Fatal("a second run changed the sealed settings")
	}
}

// TestWhatIsSealedWinsOverTheColumns is the older version started on the
// database again after this one sealed the settings. The sealed value is
// kept and the columns are emptied.
func TestWhatIsSealedWinsOverTheColumns(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	s := alertsOn()

	err := Save(db, &s, cipher)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	sealed := rawRow(t, db)["alert_secrets"]

	older := alertsOn()
	older.SMTPHost = "old-mail.example.org"
	addLegacyColumns(t, db, older)

	changed, err := SealLegacyAlertColumns(db, cipher)
	if err != nil || !changed {
		t.Fatalf("SealLegacyAlertColumns = %v, %v, want true", changed, err)
	}

	row := rawRow(t, db)
	if row["alert_secrets"] != sealed {
		t.Fatal("the sealed settings were written over")
	}

	requireNoPlainValue(t, row, append(plainValues(), older.SMTPHost))
}

// TestAFreshDatabaseHasNothingToSeal is the startup of a database this
// version made, with and without settings stored.
func TestAFreshDatabaseHasNothingToSeal(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	changed, err := SealLegacyAlertColumns(db, cipher)
	if err != nil || changed {
		t.Fatalf("before a first startup = %v, %v, want false", changed, err)
	}

	_, err = Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	changed, err = SealLegacyAlertColumns(db, cipher)
	if err != nil || changed {
		t.Fatalf("on a fresh database = %v, %v, want false", changed, err)
	}

	// The columns there but empty, which is what a sealed database holds
	// after the first run, is nothing to seal either.
	addLegacyColumns(t, db, Defaults())

	changed, err = SealLegacyAlertColumns(db, cipher)
	if err != nil || changed {
		t.Fatalf("with the columns empty = %v, %v, want false", changed, err)
	}
}

// TestResetEmptiesTheColumnsInTheClear holds a reset to what it says: it
// runs before the key is loaded, and the columns it left would be sealed back
// in by the startup that follows.
func TestResetEmptiesTheColumnsInTheClear(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	_, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	addLegacyColumns(t, db, alertsOn())

	_, _, err = Reset(db)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}

	requireNoPlainValue(t, rawRow(t, db), plainValues())

	changed, err := SealLegacyAlertColumns(db, cipher)
	if err != nil || changed {
		t.Fatalf("SealLegacyAlertColumns after a reset = %v, %v, want false", changed, err)
	}

	opened, err := LoadOpened(db, cipher)
	if err != nil {
		t.Fatalf("LoadOpened: %v", err)
	}

	if opened.alertSecrets() != defaultAlertSecrets() {
		t.Fatalf("after a reset the sealed settings are %+v", opened.alertSecrets())
	}
}

// TestAResetOfSealedSettingsIsReportedMasked is the reset of a set that was
// never opened, since the reset has no key: the six are listed as changing,
// under the mask.
func TestAResetOfSealedSettingsIsReportedMasked(t *testing.T) {
	db := newDB(t)

	s := alertsOn()

	err := Save(db, &s, newCipher(t, 1))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	before, after, err := Reset(db)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}

	listed := map[string]bool{}

	for _, change := range Diff(before, after) {
		for _, value := range plainValues() {
			if strings.Contains(change.From, value) || strings.Contains(change.To, value) {
				t.Errorf("the reset reports %s as %q to %q", change.Name, change.From, change.To)
			}
		}

		if strings.Contains(change.From, before.AlertSecrets) && before.AlertSecrets != "" {
			t.Errorf("the reset reports the sealed value of %s", change.Name)
		}

		listed[change.Name] = true
	}

	for name := range sealedSettings {
		if !listed[name] {
			t.Errorf("the reset does not list %s", name)
		}
	}

	if rawRow(t, db)["alert_secrets"] != "" {
		t.Fatal("the reset left the sealed settings stored")
	}
}

// TestAResetOfTheColumnsInTheClearIsReportedMasked is -reset-settings on a
// database written by the version before this one, before the key was ever
// loaded on it: the six are in their columns in the clear and nothing is
// sealed. The reset puts them back to their defaults, so it reports each of
// them as changed, and what the startup logs for each change, the name and
// the two sides, carries the mask and never the value.
func TestAResetOfTheColumnsInTheClearIsReportedMasked(t *testing.T) {
	db := newDB(t)

	_, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	addLegacyColumns(t, db, alertsOn())

	before, after, err := Reset(db)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if before == nil {
		t.Fatal("the reset reports that nothing was stored")
	}

	changes := Diff(before, after)

	reported := map[string]Change{}

	for _, change := range changes {
		line := change.Name + " " + change.From + " " + change.To
		for _, value := range append(plainValues(), "587") {
			if strings.Contains(line, value) {
				t.Errorf("the change logged for %s carries %q: %q to %q", change.Name, value, change.From, change.To)
			}
		}

		if sealedSettings[change.Name] {
			reported[change.Name] = change
		}
	}

	if len(reported) != len(sealedSettings) {
		t.Fatalf("the reset reports %d of the six as changed, want %d: %+v", len(reported), len(sealedSettings), changes)
	}

	for name, change := range reported {
		if change.From != SecretMask {
			t.Errorf("%s is reported as changing from %q, want the mask", name, change.From)
		}

		wantTo := ""
		if name == "alert.smtp.port" {
			wantTo = SecretMask
		}

		if change.To != wantTo {
			t.Errorf("%s is reported as changing to %q, want %q", name, change.To, wantTo)
		}
	}

	row := rawRow(t, db)

	requireNoPlainValue(t, row, plainValues())

	for _, column := range []string{"alert_webhook_url", "smtp_host", "smtp_username", "smtp_from", "smtp_to"} {
		if row[column] != "" {
			t.Errorf("the reset left %s as %q", column, row[column])
		}
	}

	if row["smtp_port"] != "465" {
		t.Errorf("the reset left smtp_port as %q", row["smtp_port"])
	}

	if row["alert_secrets"] != "" {
		t.Errorf("the reset left alert_secrets as %q", row["alert_secrets"])
	}
}

// storeOlderFirstStartModes leaves the row the way the first startup of
// v3.14.0 left it: the mail modes of its defaults, STARTTLS and PLAIN, in
// their columns and nothing sealed.
func storeOlderFirstStartModes(t *testing.T, db *gorm.DB) {
	t.Helper()

	_, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = db.Exec("UPDATE settings SET smtp_security = ?, smtp_auth = ?, alert_secrets = ? WHERE id = ?",
		SMTPSecurityStartTLS, SMTPAuthPlain, "", settingsID).Error
	if err != nil {
		t.Fatalf("failed to store the older modes: %v", err)
	}
}

// requireModes holds a read set to a connection security, a login and a port.
func requireModes(t *testing.T, what string, s *Settings, security string, auth string, port int) {
	t.Helper()

	if s.SMTPSecurity != security || s.SMTPAuth != auth || s.SMTPPort != port {
		t.Errorf("%s reads %s / %s / %d, want %s / %s / %d", what,
			s.SMTPSecurity, s.SMTPAuth, s.SMTPPort, security, auth, port)
	}
}

// TestAnInstallationWithoutMailReadsTheDefaultModes is an installation that
// never set mail up. Its columns hold the modes an older version stored at its
// first startup, and they are read as the defaults, so that the card starts on
// a port that fits its security. The read writes nothing.
func TestAnInstallationWithoutMailReadsTheDefaultModes(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	storeOlderFirstStartModes(t, db)

	loaded, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	requireModes(t, "Load", loaded, SMTPSecurityTLS, SMTPAuthLogin, 465)

	opened, err := LoadOpened(db, cipher)
	if err != nil {
		t.Fatalf("LoadOpened: %v", err)
	}

	requireModes(t, "LoadOpened", opened, SMTPSecurityTLS, SMTPAuthLogin, 465)

	row := rawRow(t, db)
	if row["smtp_security"] != SMTPSecurityStartTLS || row["smtp_auth"] != SMTPAuthPlain {
		t.Errorf("the read wrote the modes: %s / %s", row["smtp_security"], row["smtp_auth"])
	}
}

// TestAStoredMailServerKeepsItsModes is the other half: with a server stored,
// the modes are the ones stored for it, the port among them.
func TestAStoredMailServerKeepsItsModes(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	storeOlderFirstStartModes(t, db)

	s := mailOn()
	s.SMTPPort = 587
	s.SMTPSecurity = SMTPSecurityStartTLS
	s.SMTPAuth = SMTPAuthPlain

	err := Save(db, &s, cipher)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	opened, err := LoadOpened(db, cipher)
	if err != nil {
		t.Fatalf("LoadOpened: %v", err)
	}

	requireModes(t, "LoadOpened", opened, SMTPSecurityStartTLS, SMTPAuthPlain, 587)

	// Before the key is loaded the server is not known, so the stored modes
	// are left as they are rather than read as the defaults.
	loaded, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.SMTPSecurity != SMTPSecurityStartTLS || loaded.SMTPAuth != SMTPAuthPlain {
		t.Errorf("Load of a sealed set reads %s / %s", loaded.SMTPSecurity, loaded.SMTPAuth)
	}
}

// TestAWebhookWithoutAMailServerReadsTheDefaultModes has something sealed and
// still no mail server: the modes are settled once the set is opened.
func TestAWebhookWithoutAMailServerReadsTheDefaultModes(t *testing.T) {
	db := newDB(t)
	cipher := newCipher(t, 1)

	storeOlderFirstStartModes(t, db)

	s := Defaults()
	s.AlertWebhookURL = "https://hooks.example.com/services/T000/B000/secret-token"
	s.SMTPPort = 587
	s.SMTPSecurity = SMTPSecurityStartTLS
	s.SMTPAuth = SMTPAuthPlain

	err := Save(db, &s, cipher)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	opened, err := LoadOpened(db, cipher)
	if err != nil {
		t.Fatalf("LoadOpened: %v", err)
	}

	requireModes(t, "LoadOpened", opened, SMTPSecurityTLS, SMTPAuthLogin, 465)

	if opened.AlertWebhookURL != s.AlertWebhookURL {
		t.Errorf("the webhook address opens to %q", opened.AlertWebhookURL)
	}
}
