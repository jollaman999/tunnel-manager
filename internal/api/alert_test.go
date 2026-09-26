package api

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/alert"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// alertCall runs one of the two test presses against the handler.
func alertCall(t *testing.T, handler func(echo.Context) error, body string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()

	req := httptest.NewRequest(http.MethodPost, "/api/settings/alert/test", strings.NewReader(body))
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}

	rec := httptest.NewRecorder()

	err := handler(e.NewContext(req, rec))
	if err != nil {
		t.Fatalf("the handler returned error: %v", err)
	}

	return rec
}

// refusalOf reads the code and the values of a refusal.
func refusalOf(t *testing.T, rec *httptest.ResponseRecorder) (string, errorArgs) {
	t.Helper()

	var body struct {
		Success bool      `json:"success"`
		Code    string    `json:"error_code"`
		Args    errorArgs `json:"error_args"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if body.Success {
		t.Fatalf("the answer is a success: %s", rec.Body.String())
	}

	return body.Code, body.Args
}

const mailPassword = "correct-mail-password" // hook:allow

// mailBody is a save that turns mail on, with the password.
func mailBody(t *testing.T) string {
	t.Helper()

	return `{"smtp_host":"mail.example.com","smtp_username":"alerts","smtp_password":` +
		jsonString(t, mailPassword) + `,"smtp_from":"tm@example.com","smtp_to":"ops@example.com"}`
}

// TestTheMailPasswordIsSealedAndNeverAnswered stores a password and reads
// everything the API says afterwards. The password is in the database sealed
// with the key of the installation, and no answer carries it or its sealed
// form: a read says only that one is stored.
func TestTheMailPasswordIsSealedAndNeverAnswered(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, mailBody(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	saved := rec.Body.String()

	stored, err := settings.LoadOpened(db, h.cipher)
	if err != nil {
		t.Fatalf("failed to read the settings: %v", err)
	}

	if !crypto.IsEncrypted(stored.SMTPPassword) {
		t.Fatalf("the password is stored as %q, which is not sealed", stored.SMTPPassword)
	}

	opened, err := h.cipher.Decrypt(stored.SMTPPassword)
	if err != nil || opened != mailPassword {
		t.Fatalf("the stored password opens to %q, %v", opened, err)
	}

	read := settingsRequest(t, h, "").Body.String()

	for what, answer := range map[string]string{"save": saved, "read": read} {
		if strings.Contains(answer, mailPassword) || strings.Contains(answer, stored.SMTPPassword) {
			t.Errorf("the answer to the %s carries the password: %s", what, answer)
		}
	}

	if !strings.Contains(read, `"smtp_password_set":true`) {
		t.Errorf("the read does not say a password is stored: %s", read)
	}

	// The save reports that the password changed, under the mask.
	changes := decodeSaved(t, rec).Changes
	if !namesChange(changes, smtpPasswordSetting) {
		t.Errorf("the save does not report the password: %+v", changes)
	}

	for _, change := range changes {
		if change.Name == smtpPasswordSetting && (change.To != settings.SecretMask || change.Applied != appliedNow) {
			t.Errorf("the password change is reported as %+v", change)
		}
	}
}

// TestASaveWithoutAPasswordKeepsTheStoredOne covers the saves of the other
// cards, which send back what they read and so never carry the password, and
// the tick that removes it.
func TestASaveWithoutAPasswordKeepsTheStoredOne(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, mailBody(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	first, _ := settings.LoadOpened(db, h.cipher)

	for _, body := range []string{
		`{"monitoring_interval_sec":7}`,
		`{"smtp_password":""}`,
		`{"smtp_password":null,"smtp_password_clear":false}`,
	} {
		rec = settingsRequest(t, h, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", body, rec.Code, rec.Body.String())
		}

		after, _ := settings.LoadOpened(db, h.cipher)
		if after.SMTPPassword != first.SMTPPassword {
			t.Fatalf("%s changed the stored password", body)
		}
	}

	rec = settingsRequest(t, h, `{"smtp_auth":"none","smtp_password_clear":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the clear answered %d: %s", rec.Code, rec.Body.String())
	}

	cleared, _ := settings.LoadOpened(db, h.cipher)
	if cleared.SMTPPassword != "" {
		t.Fatalf("the password was not removed: %q", cleared.SMTPPassword)
	}

	if !strings.Contains(settingsRequest(t, h, "").Body.String(), `"smtp_password_set":false`) {
		t.Fatal("the read still says a password is stored")
	}
}

const webhookSecret = "https://hooks.example.com/services/T000/B000/webhook-secret" // hook:allow

// TestTheWebhookURLIsNeverAnswered stores a webhook address and reads what
// the API says afterwards. The address lets whoever holds it post to the
// channel, so no answer carries it: a read says only that one is stored, and
// the save names the change under the mask.
func TestTheWebhookURLIsNeverAnswered(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	if !strings.Contains(settingsRequest(t, h, "").Body.String(), `"alert_webhook_url_set":false`) {
		t.Fatal("a fresh read does not say that no webhook address is stored")
	}

	rec := settingsRequest(t, h, `{"alert_webhook_url":`+jsonString(t, webhookSecret)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	stored, _ := settings.LoadOpened(db, h.cipher)
	if stored.AlertWebhookURL != webhookSecret {
		t.Fatalf("the save stored %q", stored.AlertWebhookURL)
	}

	read := settingsRequest(t, h, "").Body.String()

	for what, answer := range map[string]string{"save": rec.Body.String(), "read": read} {
		if strings.Contains(answer, webhookSecret) || strings.Contains(answer, "webhook-secret") {
			t.Errorf("the answer to the %s carries the webhook address: %s", what, answer)
		}

		if strings.Contains(answer, `"alert_webhook_url":`) {
			t.Errorf("the answer to the %s names alert_webhook_url: %s", what, answer)
		}
	}

	if !strings.Contains(read, `"alert_webhook_url_set":true`) {
		t.Errorf("the read does not say a webhook address is stored: %s", read)
	}

	found := false

	for _, change := range decodeSaved(t, rec).Changes {
		if change.Name == "alert.webhook_url" {
			found = true

			if change.From != "" || change.To != settings.SecretMask {
				t.Errorf("the webhook change is reported as %+v", change)
			}
		}
	}

	if !found {
		t.Error("the save does not report the webhook address")
	}
}

// TestASaveWithoutAWebhookURLKeepsTheStoredOne covers the saves that send no
// address, which is every save of a screen that read the settings, a new
// address put over the stored one, and the tick that removes it.
func TestASaveWithoutAWebhookURLKeepsTheStoredOne(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"alert_webhook_url":`+jsonString(t, webhookSecret)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	for _, body := range []string{
		`{"monitoring_interval_sec":7}`,
		`{"alert_webhook_url":""}`,
		`{"alert_webhook_url":"   "}`,
		`{"alert_webhook_url":null,"alert_webhook_url_clear":false}`,
	} {
		rec = settingsRequest(t, h, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", body, rec.Code, rec.Body.String())
		}

		after, _ := settings.LoadOpened(db, h.cipher)
		if after.AlertWebhookURL != webhookSecret {
			t.Fatalf("%s changed the stored address to %q", body, after.AlertWebhookURL)
		}

		for _, change := range decodeSaved(t, rec).Changes {
			if change.Name == "alert.webhook_url" {
				t.Fatalf("%s reports a change of the webhook address: %+v", body, change)
			}
		}
	}

	const replaced = "https://hooks.example.com/services/T000/B000/replaced" // hook:allow

	rec = settingsRequest(t, h, `{"alert_webhook_url":`+jsonString(t, replaced)+`,"alert_webhook_url_clear":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the replace answered %d: %s", rec.Code, rec.Body.String())
	}

	after, _ := settings.LoadOpened(db, h.cipher)
	if after.AlertWebhookURL != replaced {
		t.Fatalf("a new address with the clear stored %q, want the new address", after.AlertWebhookURL)
	}

	if strings.Contains(rec.Body.String(), replaced) {
		t.Fatalf("the answer to the replace carries the address: %s", rec.Body.String())
	}

	reported := false

	for _, change := range decodeSaved(t, rec).Changes {
		if change.Name == "alert.webhook_url" {
			reported = true

			if change.From != settings.SecretMask || change.To != settings.SecretMask {
				t.Errorf("the replace is reported as %+v", change)
			}
		}
	}

	if !reported {
		t.Error("the replace does not report the webhook address")
	}

	rec = settingsRequest(t, h, `{"alert_webhook_url_clear":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the clear answered %d: %s", rec.Code, rec.Body.String())
	}

	cleared, _ := settings.LoadOpened(db, h.cipher)
	if cleared.AlertWebhookURL != "" {
		t.Fatalf("the address was not removed: %q", cleared.AlertWebhookURL)
	}

	reported = false

	for _, change := range decodeSaved(t, rec).Changes {
		if change.Name == "alert.webhook_url" {
			reported = true

			if change.From != settings.SecretMask || change.To != "" {
				t.Errorf("the clear is reported as %+v", change)
			}
		}
	}

	if !reported {
		t.Error("the clear does not report the webhook address")
	}

	if !strings.Contains(settingsRequest(t, h, "").Body.String(), `"alert_webhook_url_set":false`) {
		t.Fatal("the read still says a webhook address is stored")
	}
}

// TestTheWebhookTestPostsToTheStoredURL stores the address of a webhook and
// presses the test with a body that names none, which is what the card sends
// when its box is left empty. The stored address is posted to without being
// answered. A body that names another address is posted to that one, and a
// body that clears the address has nowhere to post to.
func TestTheWebhookTestPostsToTheStoredURL(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	stored := make(chan string, 4)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stored <- r.URL.Path
	}))
	defer server.Close()

	other := make(chan string, 4)

	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other <- r.URL.Path
	}))
	defer elsewhere.Close()

	address := server.URL + "/services/T000/B000/webhook-secret" // hook:allow

	rec := settingsRequest(t, h, `{"alert_webhook_url":`+jsonString(t, address)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	for _, body := range []string{"", `{"alert_webhook_url":""}`, `{"alert_after_sec":60}`} {
		rec = alertCall(t, h.TestAlertWebhook, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("the test with %q answered %d: %s", body, rec.Code, rec.Body.String())
		}

		if strings.Contains(rec.Body.String(), "webhook-secret") {
			t.Fatalf("the answer to the test carries the address: %s", rec.Body.String())
		}

		select {
		case path := <-stored:
			if path != "/services/T000/B000/webhook-secret" {
				t.Fatalf("the test with %q posted to %s", body, path)
			}
		default:
			t.Fatalf("the test with %q did not post to the stored address", body)
		}
	}

	rec = alertCall(t, h.TestAlertWebhook, `{"alert_webhook_url":`+jsonString(t, elsewhere.URL+"/typed")+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the test with another address answered %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case path := <-other:
		if path != "/typed" {
			t.Fatalf("the test with another address posted to %s", path)
		}
	default:
		t.Fatal("the test with another address did not post to it")
	}

	if len(stored) != 0 {
		t.Fatal("the test with another address posted to the stored one as well")
	}

	rec = alertCall(t, h.TestAlertWebhook, `{"alert_webhook_url_clear":true}`)
	if code, _ := refusalOf(t, rec); rec.Code != http.StatusBadRequest || code != string(errAlertTestWebhookOff) {
		t.Fatalf("a test that clears the address answered %d under %s", rec.Code, code)
	}

	after, _ := settings.LoadOpened(db, h.cipher)
	if after.AlertWebhookURL != address {
		t.Fatalf("a test changed the stored address to %q", after.AlertWebhookURL)
	}
}

// TestAFailedWebhookTestDoesNotNameTheAddress fails the test press against an
// address nothing listens on and against a webhook that answers 500, with the
// token in the path and in the query, both as the stored address and as one
// the body names. The answer and the logs say what went wrong and carry no
// part of the token.
func TestAFailedWebhookTestDoesNotNameTheAddress(t *testing.T) {
	const token = "T000-B000-webhook-token" // hook:allow

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	refused := listener.Addr().String()
	_ = listener.Close()

	secret := "/services/" + token + "?token=" + token

	for _, address := range []string{"http://" + refused + secret, failing.URL + secret} {
		db := newSettingsDB(t)
		h, _, _, logs := newSettingsHandler(t, db)

		rec := settingsRequest(t, h, `{"alert_webhook_url":`+jsonString(t, address)+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
		}

		for _, body := range []string{"", `{"alert_webhook_url":` + jsonString(t, address) + `}`} {
			rec = alertCall(t, h.TestAlertWebhook, body)

			code, args := refusalOf(t, rec)
			if rec.Code != http.StatusBadGateway || code != string(errAlertTestFailed) {
				t.Fatalf("the test of %s with %q answered %d under %s", address, body, rec.Code, code)
			}

			if args["reason"] == "" {
				t.Errorf("the test of %s with %q gives no reason", address, body)
			}

			if strings.Contains(rec.Body.String(), token) {
				t.Errorf("the answer to the test with %q carries the token: %s", body, rec.Body.String())
			}
		}

		for _, entry := range logs.All() {
			line := entry.Message
			for key, value := range entry.ContextMap() {
				line += fmt.Sprintf(" %s=%v", key, value)
			}

			if strings.Contains(line, token) {
				t.Errorf("a log line carries the token: %s", line)
			}
		}
	}
}

// TestAnAlertSettingIsRefusedUnderItsOwnCode holds two of the rules to the
// code and the values a screen writes its sentence from.
func TestAnAlertSettingIsRefusedUnderItsOwnCode(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"alert_after_sec":5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	code, args := refusalOf(t, rec)
	if code != string(errSettingsAlertAfterInvalid) || args["value"] != "5" || args["min"] != "10" ||
		args["max"] != "86400" {
		t.Fatalf("refused under %s with %v", code, args)
	}

	rec = settingsRequest(t, h, `{"smtp_host":"mail.example.com","smtp_from":"tm@example.com",`+
		`"smtp_to":"ops@example.com, nobody","smtp_auth":"none"}`)

	code, args = refusalOf(t, rec)
	if rec.Code != http.StatusBadRequest || code != string(errSettingsSMTPToInvalid) || args["value"] != "nobody" {
		t.Fatalf("refused with %d under %s with %v", rec.Code, code, args)
	}

	stored, _ := settings.LoadOpened(db, h.cipher)
	if stored.SMTPHost != "" || stored.AlertAfterSec != 300 {
		t.Fatalf("a refused save was stored: %+v", stored)
	}
}

// TestTheAlertSettingsTakeHoldWithoutARestart holds them out of what a read
// reports as waiting for one: the watcher reads them on every scan.
func TestTheAlertSettingsTakeHoldWithoutARestart(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, `{"alert_after_sec":60,"alert_webhook_url":"https://hooks.example.com/a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	saved := decodeSaved(t, rec)
	if saved.RestartRequired {
		t.Fatalf("a save of alert settings asks for a restart: %+v", saved.Changes)
	}

	pending, _ := decodePending(t, settingsRequest(t, h, ""))
	if len(pending) != 0 {
		t.Fatalf("alert settings are reported as waiting for a restart: %+v", pending)
	}
}

// TestTheWebhookTestPostsWhatTheBoxesHold sends a test to a webhook named only
// in the body, and checks that nothing was stored for it.
func TestTheWebhookTestPostsWhatTheBoxesHold(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := alertCall(t, h.TestAlertWebhook, "")
	if code, _ := refusalOf(t, rec); rec.Code != http.StatusBadRequest || code != string(errAlertTestWebhookOff) {
		t.Fatalf("a test with no webhook answered %d under %s", rec.Code, code)
	}

	got := make(chan map[string]interface{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		got <- body
	}))
	defer server.Close()

	rec = alertCall(t, h.TestAlertWebhook, `{"alert_webhook_url":`+jsonString(t, server.URL)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the test answered %d: %s", rec.Code, rec.Body.String())
	}

	if body := <-got; body["event"] != "test" {
		t.Fatalf("the webhook was posted %v, want a test event", body)
	}

	stored, _ := settings.LoadOpened(db, h.cipher)
	if stored.AlertWebhookURL != "" {
		t.Fatalf("the test stored the webhook: %q", stored.AlertWebhookURL)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	rec = alertCall(t, h.TestAlertWebhook, `{"alert_webhook_url":`+jsonString(t, failing.URL)+`}`)

	code, args := refusalOf(t, rec)
	if rec.Code != http.StatusBadGateway || code != string(errAlertTestFailed) || !strings.Contains(args["reason"], "500") {
		t.Fatalf("a failing webhook answered %d under %s with %v", rec.Code, code, args)
	}
}

// TestTheMailTestSaysWhatWentWrong is a mail server nobody listens on. The
// answer carries what the sender said, since that is what the operator needs.
func TestTheMailTestSaysWhatWentWrong(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := alertCall(t, h.TestAlertSMTP, "")
	if code, _ := refusalOf(t, rec); rec.Code != http.StatusBadRequest || code != string(errAlertTestSMTPOff) {
		t.Fatalf("a test with no mail server answered %d under %s", rec.Code, code)
	}

	// A port that was listened on and let go is one nothing answers on.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	rec = alertCall(t, h.TestAlertSMTP, `{"smtp_host":"127.0.0.1","smtp_port":`+strconv.Itoa(port)+
		`,"smtp_security":"none","smtp_auth":"none","smtp_from":"tm@example.com","smtp_to":"ops@example.com"}`)

	code, args := refusalOf(t, rec)
	if rec.Code != http.StatusBadGateway || code != string(errAlertTestFailed) || args["reason"] == "" {
		t.Fatalf("an unreachable mail server answered %d under %s with %v", rec.Code, code, args)
	}
}

// TestTheAlertSettingsTravelWithAnExport moves the alert settings, the mail
// password among them, to an installation with a key of its own. The password
// arrives sealed with the key of the one that imported it.
func TestTheAlertSettingsTravelWithAnExport(t *testing.T) {
	source := newTransferInstall(t)
	target := newTransferInstall(t)

	stored, _ := settings.LoadOpened(source.db, source.cipher)
	stored.AlertAfterSec = 120
	stored.AlertWebhookURL = "https://hooks.example.com/tm"
	stored.SMTPHost = "mail.example.com"
	stored.SMTPPort = 465
	stored.SMTPSecurity = settings.SMTPSecurityTLS
	stored.SMTPAuth = settings.SMTPAuthLogin
	stored.SMTPUsername = "alerts"
	stored.SMTPFrom = "tm@example.com"
	stored.SMTPTo = "ops@example.com, oncall@example.net"
	stored.SMTPSkipVerify = true

	sealed, err := source.cipher.Encrypt(mailPassword)
	if err != nil {
		t.Fatalf("failed to seal: %v", err)
	}

	stored.SMTPPassword = sealed

	err = settings.Save(source.db, stored, source.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	file := source.exportSettings(t, testExportPassword)

	rec := target.call(t, target.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), mailPassword) {
		t.Fatalf("the answer to the import carries the password: %s", rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), stored.AlertWebhookURL) {
		t.Fatalf("the answer to the import carries the webhook address: %s", rec.Body.String())
	}

	after, _ := settings.LoadOpened(target.db, target.cipher)

	if after.AlertAfterSec != 120 || after.AlertWebhookURL != stored.AlertWebhookURL ||
		after.SMTPHost != stored.SMTPHost || after.SMTPPort != 465 || after.SMTPSecurity != "tls" ||
		after.SMTPAuth != "login" || after.SMTPUsername != "alerts" || after.SMTPFrom != stored.SMTPFrom ||
		after.SMTPTo != stored.SMTPTo || !after.SMTPSkipVerify {
		t.Fatalf("the alert settings arrived as %+v", after)
	}

	if after.SMTPPassword == sealed {
		t.Fatal("the password arrived sealed with the key of the other installation")
	}

	opened, err := target.cipher.Decrypt(after.SMTPPassword)
	if err != nil || opened != mailPassword {
		t.Fatalf("the imported password opens to %q, %v", opened, err)
	}

	// The webhook address and the mail settings arrive sealed with the key
	// of the installation that imported them, and in the clear nowhere.
	row := rawSettingsRow(t, target.db)
	requireNothingInTheClear(t, row, stored)

	_, err = source.cipher.Decrypt(row["alert_secrets"])
	if err == nil {
		t.Fatal("the imported settings open with the key of the other installation")
	}

	_, err = target.cipher.Decrypt(row["alert_secrets"])
	if err != nil {
		t.Fatalf("the imported settings do not open with the key of the installation: %v", err)
	}
}

// rawSettingsRow reads the settings row as the database holds it, every
// column as text, the way a copy of the database file would be read.
func rawSettingsRow(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()

	var rows []map[string]interface{}

	err := db.Raw("SELECT * FROM settings").Scan(&rows).Error
	if err != nil || len(rows) != 1 {
		t.Fatalf("failed to read the settings row: %d rows, %v", len(rows), err)
	}

	row := map[string]string{}

	for column, value := range rows[0] {
		switch v := value.(type) {
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

// requireNothingInTheClear fails when a column of the row holds one of the
// sealed settings of s as it was typed.
func requireNothingInTheClear(t *testing.T, row map[string]string, s *settings.Settings) {
	t.Helper()

	for column, stored := range row {
		for _, value := range []string{s.AlertWebhookURL, s.SMTPHost, s.SMTPUsername, s.SMTPFrom, s.SMTPTo} {
			if value != "" && strings.Contains(stored, value) {
				t.Errorf("column %s holds %q in the clear", column, value)
			}
		}
	}
}

// sealedAlertBody is a save that names every one of the six sealed settings.
const sealedAlertBody = `{"alert_webhook_url":"https://hooks.example.com/services/T000/secret-token",` +
	`"smtp_host":"mail.example.com","smtp_port":465,"smtp_security":"tls","smtp_username":"alerts-sender",` +
	`"smtp_password":"sealed-mail-password","smtp_from":"tm@example.com","smtp_to":"ops@example.com"}` // hook:allow

// TestTheAlertSettingsAreStoredSealedAndReadInTheClear saves the six, reads
// the row the way a copy of the database file would be read, and reads the
// settings the way the screen does.
func TestTheAlertSettingsAreStoredSealedAndReadInTheClear(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, logs := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, sealedAlertBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	want := settings.Settings{
		AlertWebhookURL: "https://hooks.example.com/services/T000/secret-token",
		SMTPHost:        "mail.example.com",
		SMTPPort:        465,
		SMTPUsername:    "alerts-sender",
		SMTPFrom:        "tm@example.com",
		SMTPTo:          "ops@example.com",
	}

	row := rawSettingsRow(t, db)
	requireNothingInTheClear(t, row, &want)

	if !crypto.IsEncrypted(row["alert_secrets"]) {
		t.Fatalf("alert_secrets is stored as %q, which is not sealed", row["alert_secrets"])
	}

	// The changes the save answers with name the six under the mask.
	changes := decodeSaved(t, rec).Changes
	for _, name := range []string{"alert.webhook_url", "alert.smtp.host", "alert.smtp.port",
		"alert.smtp.username", "alert.smtp.from", "alert.smtp.to"} {
		found := false

		for _, change := range changes {
			if change.Name != name {
				continue
			}

			found = true

			if change.To != settings.SecretMask || (change.From != "" && change.From != settings.SecretMask) {
				t.Errorf("%s is reported as %q to %q, want the mask", name, change.From, change.To)
			}
		}

		if !found {
			t.Errorf("the save does not report %s: %+v", name, changes)
		}
	}

	// Nothing the save logged carries them either.
	for _, entry := range logs.All() {
		line := entry.Message
		for _, field := range entry.Context {
			line += " " + field.String
		}

		requireNothingInTheClear(t, map[string]string{"log": line}, &want)
	}

	var read struct {
		Data map[string]interface{} `json:"data"`
	}

	err := json.Unmarshal(settingsRequest(t, h, "").Body.Bytes(), &read)
	if err != nil {
		t.Fatalf("failed to read the answer: %v", err)
	}

	for name, value := range map[string]interface{}{
		"alert_webhook_url_set": true,
		"smtp_host":             want.SMTPHost,
		"smtp_port":             float64(want.SMTPPort),
		"smtp_username":         want.SMTPUsername,
		"smtp_from":             want.SMTPFrom,
		"smtp_to":               want.SMTPTo,
		"smtp_security":         "tls",
	} {
		if read.Data[name] != value {
			t.Errorf("a read answers %s = %v, want %v", name, read.Data[name], value)
		}
	}

	if _, found := read.Data["alert_webhook_url"]; found {
		t.Error("a read carries the webhook address")
	}

	if _, found := read.Data["alert_secrets"]; found {
		t.Error("a read carries the sealed value")
	}
}

// TestAStoredAlertGoesWhereTheSealedSettingsSay stores a webhook and a mail
// server on this system and sends to them the two ways the stored settings
// are read: the test presses with no body, and the read the alert watcher
// makes on every scan.
func TestAStoredAlertGoesWhereTheSealedSettingsSay(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	posted := make(chan map[string]interface{}, 4)

	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		posted <- body
	}))
	defer webhook.Close()

	mail := newCapturingMailServer(t)

	rec := settingsRequest(t, h, `{"alert_webhook_url":`+jsonString(t, webhook.URL)+
		`,"smtp_host":"127.0.0.1","smtp_port":`+strconv.Itoa(mail.port)+
		`,"smtp_security":"none","smtp_auth":"none","smtp_from":"tm@example.com","smtp_to":"ops@example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	rec = alertCall(t, h.TestAlertWebhook, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the webhook test answered %d: %s", rec.Code, rec.Body.String())
	}

	if body := <-posted; body["event"] != alert.EventTest {
		t.Fatalf("the webhook was posted %v, want a test event", body)
	}

	rec = alertCall(t, h.TestAlertSMTP, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the mail test answered %d: %s", rec.Code, rec.Body.String())
	}

	if _, lines := mail.seen(); !strings.Contains(lines, "RCPT TO:<ops@example.com>") {
		t.Fatalf("the mail test did not reach the stored server:\n%s", lines)
	}

	// The read main hands the watcher.
	stored, err := settings.LoadOpened(db, h.cipher)
	if err != nil {
		t.Fatalf("LoadOpened: %v", err)
	}

	h.alerts.Send(t.Context(), alert.ConfigOf(stored), alert.Event{
		Event:     alert.EventDown,
		Kind:      "local_forward",
		Host:      "192.0.2.10",
		LocalPort: 15432,
	})

	if body := <-posted; body["event"] != alert.EventDown || body["host"] != "192.0.2.10" {
		t.Fatalf("the webhook was posted %v, want the down", body)
	}

	if accepted, lines := mail.seen(); accepted != 2 || !strings.Contains(lines, "192.0.2.10") {
		t.Fatalf("the mail server was connected to %d times and was sent:\n%s", accepted, lines)
	}
}

// TestAWrongKeyIsAFailureAndNotMailSwitchedOff reads settings sealed with
// one key through a process holding another. Every reader says so rather
// than reading the six as their defaults, which would be alerts switched off.
func TestAWrongKeyIsAFailureAndNotMailSwitchedOff(t *testing.T) {
	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, sealedAlertBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	sealed := rawSettingsRow(t, db)["alert_secrets"]

	other, _, _, _ := newSettingsHandler(t, db)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"read":         settingsRequest(t, other, ""),
		"save":         settingsRequest(t, other, `{"monitoring_interval_sec":7}`),
		"webhook test": alertCall(t, other.TestAlertWebhook, ""),
		"mail test":    alertCall(t, other.TestAlertSMTP, ""),
	} {
		code, _ := refusalOf(t, rec)
		if rec.Code != http.StatusInternalServerError || code != string(errSettingsReadFailed) {
			t.Errorf("the %s answered %d under %s, want 500 under %s", name, rec.Code, code, errSettingsReadFailed)
		}
	}

	if rawSettingsRow(t, db)["alert_secrets"] != sealed {
		t.Fatal("a process with another key changed the sealed settings")
	}

	_, err := settings.LoadOpened(db, other.cipher)
	if !errors.Is(err, settings.ErrAlertSecretsDoNotOpen) {
		t.Fatalf("the read the watcher makes = %v, want %v", err, settings.ErrAlertSecretsDoNotOpen)
	}
}

// TestAFileWithoutThePasswordLeavesTheStoredOne is a file from before the
// setting existed, which names no password at all, against one that names an
// empty one.
func TestAFileWithoutThePasswordLeavesTheStoredOne(t *testing.T) {
	install := newTransferInstall(t)

	stored, _ := settings.LoadOpened(install.db, install.cipher)

	sealed, err := install.cipher.Encrypt(mailPassword)
	if err != nil {
		t.Fatalf("failed to seal: %v", err)
	}

	stored.SMTPPassword = sealed

	err = settings.Save(install.db, stored, install.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	older := settingsOf(stored)
	older.SMTPPassword = nil

	file, err := install.handler.seal(transferKindSettings, older, testExportPassword, stored.UpdatedAt)
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec := install.call(t, install.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	after, _ := settings.LoadOpened(install.db, install.cipher)
	if after.SMTPPassword != sealed {
		t.Fatal("a file that names no password changed the stored one")
	}

	empty := ""
	older.SMTPPassword = &empty

	file, err = install.handler.seal(transferKindSettings, older, testExportPassword, stored.UpdatedAt)
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec = install.call(t, install.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	after, _ = settings.LoadOpened(install.db, install.cipher)
	if after.SMTPPassword != "" {
		t.Fatal("a file that names an empty password left the stored one")
	}
}

// capturingMailServer is a mail server on this system that takes whatever it
// is sent, a login included, and keeps every line. It is what a server named
// by somebody who wants the stored password looks like.
type capturingMailServer struct {
	port int

	mu       sync.Mutex
	accepted int
	lines    []string
}

func newCapturingMailServer(t *testing.T) *capturingMailServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	server := &capturingMailServer{port: listener.Addr().(*net.TCPAddr).Port}

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			server.mu.Lock()
			server.accepted++
			server.mu.Unlock()

			wg.Add(1)

			go func() {
				defer wg.Done()
				server.serve(conn)
			}()
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})

	return server
}

func (s *capturingMailServer) serve(conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	write("220 capture ESMTP")

	inData := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")

		s.mu.Lock()
		s.lines = append(s.lines, line)
		s.mu.Unlock()

		if inData {
			if line == "." {
				inData = false
				write("250 queued")
			}

			continue
		}

		switch strings.ToUpper(strings.SplitN(line, " ", 2)[0]) {
		case "EHLO":
			write("250-capture")
			write("250-AUTH PLAIN LOGIN")
			write("250 OK")
		case "AUTH":
			write("235 ok")
		case "DATA":
			inData = true
			write("354 go ahead")
		case "QUIT":
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func (s *capturingMailServer) seen() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.accepted, strings.Join(s.lines, "\n")
}

// storeMailLogin stores mail settings with a password, the way an operator
// left them, and returns the handler.
func storeMailLogin(t *testing.T) (*SettingsHandler, *gorm.DB) {
	t.Helper()

	db := newSettingsDB(t)
	h, _, _, _ := newSettingsHandler(t, db)

	rec := settingsRequest(t, h, mailBody(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("the save answered %d: %s", rec.Code, rec.Body.String())
	}

	return h, db
}

// TestTheStoredPasswordIsNotSentToAServerTheBodyNames is the leak this guards
// against: a test that names another server, and nothing else, would have
// this system log in there with the stored password. The server named here is
// on this system, which is where net/smtp lets a password go without TLS, so
// without the guard the password would arrive in the clear.
func TestTheStoredPasswordIsNotSentToAServerTheBodyNames(t *testing.T) {
	h, _ := storeMailLogin(t)
	server := newCapturingMailServer(t)
	port := strconv.Itoa(server.port)

	for _, body := range []string{
		`{"smtp_host":"127.0.0.1","smtp_port":` + port + `,"smtp_security":"none"}`,
		`{"smtp_host":"127.0.0.1","smtp_port":` + port + `,"smtp_security":"tls","smtp_skip_verify":true}`,
		`{"smtp_host":"127.0.0.1","smtp_port":` + port + `,"smtp_security":"none","smtp_password":""}`,
	} {
		rec := alertCall(t, h.TestAlertSMTP, body)

		code, _ := refusalOf(t, rec)
		if rec.Code != http.StatusBadRequest || code != string(errSettingsSMTPPasswordRequired) {
			t.Errorf("%s answered %d under %s, want 400 under %s", body, rec.Code, code, errSettingsSMTPPasswordRequired)
		}
	}

	accepted, lines := server.seen()
	if accepted != 0 {
		t.Fatalf("the server named in the body was connected to %d times and was sent:\n%s", accepted, lines)
	}

	// With the password typed again, the test goes to the server it names,
	// and what it logs in with is what was typed.
	rec := alertCall(t, h.TestAlertSMTP, `{"smtp_host":"127.0.0.1","smtp_port":`+port+
		`,"smtp_security":"none","smtp_password":"typed-for-this-server"}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("a test with its own password answered %d: %s", rec.Code, rec.Body.String())
	}

	_, lines = server.seen()
	if strings.Contains(lines, base64.StdEncoding.EncodeToString([]byte("\x00alerts\x00"+mailPassword))) {
		t.Fatal("the stored password reached the server")
	}

	if !strings.Contains(lines, base64.StdEncoding.EncodeToString([]byte("\x00alerts\x00typed-for-this-server"))) {
		t.Fatalf("the typed password was not what the test logged in with:\n%s", lines)
	}
}

// TestASaveThatMovesTheMailTargetNeedsThePassword holds a save to the same rule:
// the next alert would go to the server it names.
func TestASaveThatMovesTheMailTargetNeedsThePassword(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"another server", `{"smtp_host":"mail.example.net"}`},
		{"another port", `{"smtp_port":2525}`},
		{"another user", `{"smtp_username":"someone-else"}`},
		{"another security", `{"smtp_security":"none"}`},
		{"the certificate check turned off", `{"smtp_skip_verify":true}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := storeMailLogin(t)
			before, _ := settings.LoadOpened(db, h.cipher)

			rec := settingsRequest(t, h, tc.body)

			code, _ := refusalOf(t, rec)
			if rec.Code != http.StatusBadRequest || code != string(errSettingsSMTPPasswordRequired) {
				t.Fatalf("answered %d under %s: %s", rec.Code, code, rec.Body.String())
			}

			after, _ := settings.LoadOpened(db, h.cipher)
			if *after != *before {
				t.Fatalf("a refused save changed the settings: %+v", after)
			}
		})
	}
}

// TestASaveThatMovesTheMailTargetWithAPasswordIsStored covers the ways out:
// a save that moves nothing, the password typed again, and the login turned
// off.
func TestASaveThatMovesTheMailTargetWithAPasswordIsStored(t *testing.T) {
	h, db := storeMailLogin(t)
	first, _ := settings.LoadOpened(db, h.cipher)

	rec := settingsRequest(t, h, `{"smtp_from":"other@example.com","smtp_skip_verify":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a save that moves nothing answered %d: %s", rec.Code, rec.Body.String())
	}

	kept, _ := settings.LoadOpened(db, h.cipher)
	if kept.SMTPPassword != first.SMTPPassword {
		t.Fatal("a save that moves nothing changed the stored password")
	}

	rec = settingsRequest(t, h, `{"smtp_host":"mail.example.net","smtp_password":"new-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a move with a new password answered %d: %s", rec.Code, rec.Body.String())
	}

	moved, _ := settings.LoadOpened(db, h.cipher)

	opened, err := h.cipher.Decrypt(moved.SMTPPassword)
	if moved.SMTPHost != "mail.example.net" || err != nil || opened != "new-password" {
		t.Fatalf("stored host %q and password %q, %v", moved.SMTPHost, opened, err)
	}

	// Turning the login off while moving drops the password rather than
	// keeping it for a server it was not given for, where turning the login
	// back on would send it.
	rec = settingsRequest(t, h, `{"smtp_host":"relay.example.com","smtp_auth":"none"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a move with the login off answered %d: %s", rec.Code, rec.Body.String())
	}

	relay, _ := settings.LoadOpened(db, h.cipher)
	if relay.SMTPPassword != "" {
		t.Fatal("the password was kept for a server it was not given for")
	}
}

// TestAnImportThatMovesTheMailTargetWithoutAPasswordDropsIt is a file that
// names another mail server and carries no password.
func TestAnImportThatMovesTheMailTargetWithoutAPasswordDropsIt(t *testing.T) {
	install := newTransferInstall(t)

	stored, _ := settings.LoadOpened(install.db, install.cipher)
	stored.SMTPHost = "mail.example.com"
	stored.SMTPUsername = "alerts"
	stored.SMTPFrom = "tm@example.com"
	stored.SMTPTo = "ops@example.com"

	sealed, err := install.cipher.Encrypt(mailPassword)
	if err != nil {
		t.Fatalf("failed to seal: %v", err)
	}

	stored.SMTPPassword = sealed

	err = settings.Save(install.db, stored, install.cipher)
	if err != nil {
		t.Fatalf("failed to store the settings: %v", err)
	}

	content := settingsOf(stored)
	content.SMTPHost = "mail.example.net"
	content.SMTPPassword = nil

	file, err := install.handler.seal(transferKindSettings, content, testExportPassword, time.Now())
	if err != nil {
		t.Fatalf("failed to seal a file: %v", err)
	}

	rec := install.call(t, install.handler.ImportSettings,
		`{"password":`+jsonString(t, testExportPassword)+`,"file":`+jsonString(t, file)+`}`) // hook:allow
	if rec.Code != http.StatusOK {
		t.Fatalf("the import answered %d: %s", rec.Code, rec.Body.String())
	}

	after, _ := settings.LoadOpened(install.db, install.cipher)
	if after.SMTPHost != "mail.example.net" || after.SMTPPassword != "" {
		t.Fatalf("after the import the host is %q and a password is stored: %v", after.SMTPHost, after.SMTPPassword != "")
	}
}
