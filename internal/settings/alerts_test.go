package settings

import (
	"errors"
	"testing"
)

// TestTheAlertDefaults holds what a fresh installation starts on: both ways of
// being told off, five minutes of delay, and a mail setup that waits on what a
// port of implicit TLS expects, with the certificate checked.
func TestTheAlertDefaults(t *testing.T) {
	d := Defaults()

	cases := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"alert.after_sec", d.AlertAfterSec, 300},
		{"alert.webhook_url", d.AlertWebhookURL, ""},
		{"alert.smtp.host", d.SMTPHost, ""},
		{"alert.smtp.port", d.SMTPPort, 465},
		{"alert.smtp.security", d.SMTPSecurity, "tls"},
		{"alert.smtp.auth", d.SMTPAuth, "login"},
		{"alert.smtp.skip_verify", d.SMTPSkipVerify, false},
	}

	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestARowWrittenBeforeTheAlertSettingsReadsAsTheDefaults drops the columns to
// build a row from before they existed and migrates them back, which is what an
// upgrade does.
func TestARowWrittenBeforeTheAlertSettingsReadsAsTheDefaults(t *testing.T) {
	db := newDB(t)

	_, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, column := range []string{"alert_after_sec", "smtp_security", "smtp_auth", "smtp_password",
		"smtp_skip_verify", "alert_secrets"} {
		err = db.Exec("ALTER TABLE settings DROP COLUMN " + column).Error
		if err != nil {
			t.Fatalf("dropping %s: %v", column, err)
		}
	}

	err = db.AutoMigrate(&Settings{})
	if err != nil {
		t.Fatalf("migrating the columns back: %v", err)
	}

	upgraded, err := Load(db)
	if err != nil {
		t.Fatalf("Load after the migration: %v", err)
	}

	d := Defaults()

	if upgraded.AlertAfterSec != d.AlertAfterSec || upgraded.SMTPPort != d.SMTPPort ||
		upgraded.SMTPSecurity != d.SMTPSecurity || upgraded.SMTPAuth != d.SMTPAuth ||
		upgraded.SMTPSkipVerify || upgraded.SMTPHost != "" || upgraded.AlertWebhookURL != "" {
		t.Fatalf("an upgraded row reads as after=%d port=%d security=%q auth=%q skip=%v host=%q webhook=%q",
			upgraded.AlertAfterSec, upgraded.SMTPPort, upgraded.SMTPSecurity, upgraded.SMTPAuth,
			upgraded.SMTPSkipVerify, upgraded.SMTPHost, upgraded.AlertWebhookURL)
	}
}

// mailOn is a valid set with mail switched on.
func mailOn() Settings {
	s := Defaults()
	s.SMTPHost = "mail.example.com"
	s.SMTPUsername = "alerts"
	s.SMTPFrom = "Tunnel Manager <tunnel-manager@example.com>"
	s.SMTPTo = "ops@example.com, oncall@example.net,"

	return s
}

// TestTheAlertRules holds each rule of the alert settings to the error it is
// refused under and the value that error names.
func TestTheAlertRules(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(s *Settings)
		rule  error
		value string
	}{
		{"delay below the floor", func(s *Settings) { s.AlertAfterSec = 9 }, ErrAlertAfterInvalid, "9"},
		{"delay past a day", func(s *Settings) { s.AlertAfterSec = 86401 }, ErrAlertAfterInvalid, "86401"},
		{"webhook without a scheme", func(s *Settings) { s.AlertWebhookURL = "hooks.example.com/x" },
			ErrWebhookURLInvalid, "hooks.example.com/x"},
		{"webhook over ftp", func(s *Settings) { s.AlertWebhookURL = "ftp://hooks.example.com/x" },
			ErrWebhookURLInvalid, "ftp://hooks.example.com/x"},
		{"mail server with a space", func(s *Settings) { s.SMTPHost = "mail example.com" },
			ErrSMTPHostInvalid, "mail example.com"},
		{"mail server with a port", func(s *Settings) { s.SMTPHost = "mail.example.com:25" },
			ErrSMTPHostInvalid, "mail.example.com:25"},
		{"port zero", func(s *Settings) { s.SMTPPort = 0 }, ErrSMTPPortInvalid, "0"},
		{"unknown security", func(s *Settings) { s.SMTPSecurity = "ssl" }, ErrSMTPSecurityInvalid, "ssl"},
		{"unknown login", func(s *Settings) { s.SMTPAuth = "cram-md5" }, ErrSMTPAuthInvalid, "cram-md5"},
		{"no sender", func(s *Settings) { s.SMTPFrom = "" }, ErrSMTPFromRequired, ""},
		{"bad sender", func(s *Settings) { s.SMTPFrom = "not an address" }, ErrSMTPFromInvalid, "not an address"},
		{"sender with a line break", func(s *Settings) { s.SMTPFrom = "a@example.com\r\nBcc: b@example.com" },
			ErrSMTPFromInvalid, "a@example.com\r\nBcc: b@example.com"},
		{"no recipient", func(s *Settings) { s.SMTPTo = " , " }, ErrSMTPToRequired, ""},
		{"bad recipient", func(s *Settings) { s.SMTPTo = "ops@example.com, nobody" }, ErrSMTPToInvalid, "nobody"},
		{"login without a user", func(s *Settings) { s.SMTPUsername = "" }, ErrSMTPUsernameRequired, ""},
	}

	base := mailOn()
	if err := base.Validate(); err != nil {
		t.Fatalf("the base set is refused: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mailOn()
			tc.edit(&s)

			err := s.Validate()

			var refused *SettingError
			if !errors.As(err, &refused) || !errors.Is(err, tc.rule) {
				t.Fatalf("got %v, want %v", err, tc.rule)
			}

			if refused.Value != tc.value {
				t.Fatalf("the refusal names %q, want %q", refused.Value, tc.value)
			}
		})
	}
}

// TestMailOffNeedsNothingElse is the set a fresh installation stores: no mail
// server, so no sender, no recipient and no user are asked for.
func TestMailOffNeedsNothingElse(t *testing.T) {
	s := Defaults()
	s.AlertWebhookURL = "https://hooks.example.com/alert"

	if err := s.Validate(); err != nil {
		t.Fatalf("a webhook alone is refused: %v", err)
	}

	s.SMTPAuth = SMTPAuthNone
	s.SMTPHost = "192.0.2.25"
	s.SMTPFrom = "tm@example.com"
	s.SMTPTo = "ops@example.com"

	if err := s.Validate(); err != nil {
		t.Fatalf("a relay without a login is refused: %v", err)
	}
}

// TestThePasswordIsNeverWrittenOut holds the list the changes are read from to
// the mask, since it goes to a browser and to the log file.
func TestThePasswordIsNeverWrittenOut(t *testing.T) {
	before := Defaults()
	after := Defaults()
	after.SMTPPassword = "tmenc:v1:sealed-value"

	changes := Diff(&before, &after)
	if len(changes) != 1 || changes[0].Name != "alert.smtp.password" ||
		changes[0].From != "" || changes[0].To != SecretMask {
		t.Fatalf("setting the password is reported as %+v", changes)
	}

	for _, v := range values(&after) {
		if v.text == after.SMTPPassword {
			t.Fatalf("%s is written out as the stored password", v.name)
		}
	}
}
