package settings

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// settingsID is the primary key of the single row this package keeps. The
// settings describe the one process that reads them, and this setup takes one
// process per database file, so a second row would be a set of settings nobody
// reads. A fixed key makes every read and every write name the same row instead
// of depending on what happens to be stored.
const settingsID = 1

// Settings holds what used to be the api, monitoring, reconcile, security and
// logging sections of the configuration file.
//
// It is one row with a column per setting rather than a table of key and value
// pairs. A column keeps the type the setting has, so a port stays an int and
// compress stays a bool instead of being parsed back out of text on every read,
// Validate sees the whole set at once, and a save is a single UPDATE, so the
// process never reads half of one save and half of the next. Pairs would make
// adding a setting cheaper, which is the smaller job here: the settings are
// read once at startup and written from one screen.
type Settings struct {
	ID uint `gorm:"primaryKey" json:"-"`

	APIPort int `json:"api_port"`

	// APIHTTPSEnabled is what decides whether the port above is served over
	// TLS. It is stored with a column default of true so that a database
	// written by a version that had no such column reads back as HTTPS on:
	// SQLite fills the rows that are already there with the default as the
	// column is added, and an installation that is upgraded gets the same
	// answer a fresh one does.
	//
	// The column is named here because the name gorm works out from the field
	// is api_http_s_enabled: it knows HTTP as one word and leaves the S of
	// HTTPS on its own.
	APIHTTPSEnabled bool `gorm:"column:api_https_enabled;default:true" json:"api_https_enabled"`

	MonitoringIntervalSec int `json:"monitoring_interval_sec"`
	// ReconnectMaxIntervalSec is the longest a connection that keeps failing
	// waits before it is tried again. The first wait is the monitoring
	// interval and each failure in a row doubles it up to this, so a Host that
	// is down for an hour is not asked every five seconds for all of it, while
	// one that blinked is back within the interval. A value below the
	// monitoring interval leaves every wait at the interval.
	//
	// The column carries its default so that an installation upgraded onto
	// this version reads back the same answer a fresh one gives.
	ReconnectMaxIntervalSec int `gorm:"default:60" json:"reconnect_max_interval_sec"`
	ReconcileIntervalSec    int `json:"reconcile_interval_sec"`

	SecurityKeyFile string `json:"security_key_file"`

	LoggingLevel  string `json:"logging_level"`
	LoggingFormat string `json:"logging_format"`

	LoggingFilePath       string `json:"logging_file_path"`
	LoggingFileMaxSize    int    `json:"logging_file_max_size"`
	LoggingFileMaxBackups int    `json:"logging_file_max_backups"`
	LoggingFileMaxAge     int    `json:"logging_file_max_age"`
	// LoggingFileCompress is on by default. What it compresses is a log that
	// has already been rotated, which nothing reads again except when something
	// has gone wrong, and the text of a log is most of its size: five backups
	// of 100MB come down to a fraction of that. The cost is paid once per
	// rotation, on a file nobody is writing to any more.
	LoggingFileCompress bool `gorm:"default:true" json:"logging_file_compress"`

	// UIDefaultLanguage is the language a browser that has picked none of its
	// own is shown. A browser that has picked one keeps it: the pick belongs to
	// the person reading the screen, and this is what the installation shows
	// everybody else.
	//
	// It is empty by default, and empty is a value rather than a gap: it means
	// this installation names no language, so a browser falls back to the one
	// it asks for, which is what every browser did before this setting existed.
	// Defaulting it to "en" would read the same way to Go and mean something
	// else entirely on screen, turning a Korean browser English at the upgrade
	// that added the column. The two cases have to be told apart, so the
	// absence is spelled as the absence.
	//
	// That is also what a row written before the column existed reads back as.
	// The column carries no default, so SQLite fills such a row with NULL and
	// gorm reads NULL into a string as the empty one: an installation that is
	// upgraded names no language, which is the same answer a fresh one gives.
	//
	// The column is named here for the reason the HTTPS one is: the name gorm
	// works out from the field is uidefault_language, because it knows UI as
	// one word and then runs it into what follows. Every other column is words
	// divided by underscores, and this one would be the exception nobody
	// remembers when they come to write it in a query.
	UIDefaultLanguage string `gorm:"column:ui_default_language" json:"ui_default_language"`

	// UpdateCheckEnabled is whether the newest release is read on a timer. It
	// is on by default: what it costs is one small answer from a public API,
	// nothing is downloaded and nothing is written, and an installation that
	// does not know a release exists is one whose operator finds out from
	// somewhere else or not at all.
	//
	// The column carries its default so that an installation upgraded onto
	// this version reads back the same answer a fresh one gives.
	UpdateCheckEnabled bool `gorm:"default:true" json:"update_check_enabled"`
	// UpdateCheckIntervalHours is how often that read happens. Releases are
	// not a thing that happens by the minute, and the answer is only ever used
	// to draw a line on a screen or, where the setting below is on, to start
	// an install that takes the service down: a day is often enough for both
	// and rare enough that nobody is rate limited for it.
	UpdateCheckIntervalHours int `gorm:"default:24" json:"update_check_interval_hours"`
	// UpdateAutoInstall is whether a release that is newer is installed without
	// anybody pressing anything. It is off by default, and that default is the
	// decision this setting exists to leave with the operator.
	//
	// What an install does is replace the executable and restart the service,
	// which takes down every tunnel that is up. When that may happen is not
	// something this program can work out: it depends on what runs over those
	// tunnels and who is relying on them at the time. An operator who knows
	// that can turn this on; one who has not thought about it gets a service
	// that stays where they left it.
	UpdateAutoInstall bool `gorm:"default:false" json:"update_auto_install"`

	// AlertAfterSec is how long a tunnel, a local forward or a SOCKS5 proxy
	// has to have been without a connection before it is reported as down.
	// A connection that drops and comes back within the monitoring interval
	// or two is what a network does now and then, and a message for each of
	// those would teach whoever receives them to stop reading. Five minutes
	// is past that and short enough that somebody hears about an outage while
	// it is still one.
	//
	// The columns below carry their defaults so that an installation upgraded
	// onto this version reads back what a fresh one gives, and every column
	// is named here because the name gorm works out for SMTP runs the letters
	// into what follows.
	AlertAfterSec int `gorm:"column:alert_after_sec;default:300" json:"alert_after_sec"`
	// AlertWebhookURL is where a down and an up are posted to as JSON. Empty
	// is the webhook switched off.
	AlertWebhookURL string `gorm:"column:alert_webhook_url" json:"alert_webhook_url"`

	// SMTPHost is the mail server a down and an up are sent through. Empty is
	// mail switched off, which is what a fresh installation is on: there is
	// no server this program could guess at.
	SMTPHost string `gorm:"column:smtp_host" json:"smtp_host"`
	SMTPPort int    `gorm:"column:smtp_port;default:587" json:"smtp_port"`
	// SMTPSecurity is how the connection to the server is protected: "none",
	// "starttls" or "tls". STARTTLS on 587 is what the submission port is for
	// (RFC 8314 names it next to implicit TLS on 465), and it is the default
	// because it is what a mail provider hands out as the settings to use.
	SMTPSecurity string `gorm:"column:smtp_security;default:starttls" json:"smtp_security"`
	// SMTPAuth is how this program logs in to the server: "none", "plain" or
	// "login". Neither of the two that send a password does so over a
	// connection that is not protected by TLS.
	SMTPAuth     string `gorm:"column:smtp_auth;default:plain" json:"smtp_auth"`
	SMTPUsername string `gorm:"column:smtp_username" json:"smtp_username"`
	// SMTPPassword is held sealed with the key of this installation, the way
	// the password of a Host is. It is kept out of the JSON in both
	// directions: a read of the settings says whether one is stored and never
	// what it is, and a save reaches it only through the request that seals
	// it on the way in.
	SMTPPassword string `gorm:"column:smtp_password" json:"-"`
	SMTPFrom     string `gorm:"column:smtp_from" json:"smtp_from"`
	// SMTPTo is the addresses a message goes to, separated by commas.
	SMTPTo string `gorm:"column:smtp_to" json:"smtp_to"`
	// SMTPSkipVerify turns off the check of the certificate the mail server
	// presents. It is off by default, and what turning it on costs is that
	// anybody who can get between this program and the server can read the
	// password it logs in with.
	SMTPSkipVerify bool `gorm:"column:smtp_skip_verify;default:false" json:"smtp_skip_verify"`

	// UpdatedAt is when the row was last written. It is this end's account of
	// that and not a setting anybody chooses, so it is kept out of the JSON: a
	// save binds the request body onto the stored set, which makes every field
	// JSON names a field a client writes, and a stored time a client named is a
	// row claiming it was saved when nobody saved it.
	//
	// It is kept out in both directions because a tag cannot split them, and
	// nothing is reading it. The times the screens draw belong to a Host and to
	// a service port, which are rows of their own, and a settings file an export
	// writes names the settings it carries one by one rather than this struct.
	UpdatedAt time.Time `json:"-"`
}

// Defaults returns what a first startup stores.
//
// Every value is the one the configuration file fell back to while the settings
// lived there, so a deployment that drops its settings sections runs on as it
// did. api.port and monitoring.interval_sec are the exception: they had no
// fallback at all, since a file that left them out was refused, so the values
// the configuration file carried before it was removed are used. logging.file.compress had no
// fallback either and a file that left it out ran with it off, which is what
// stands here.
//
// ui.default_language was never in the configuration file at all. It is written
// out as the empty string rather than left to the zero value, because the empty
// string is the choice here and not a field nobody filled in: it is what keeps
// an installation that is upgraded showing each browser the language that
// browser asks for.
func Defaults() Settings {
	return Settings{
		ID:                    settingsID,
		APIPort:               8888,
		APIHTTPSEnabled:       true,
		MonitoringIntervalSec: 5,
		// A minute is long enough that a Host which is down is asked once a
		// minute rather than every interval, and short enough that one which
		// comes back is not left waiting on this long after it answers.
		ReconnectMaxIntervalSec: 60,
		ReconcileIntervalSec:    5,
		SecurityKeyFile:         "keys/tunnel-manager.key",
		LoggingLevel:            "info",
		LoggingFormat:           "json",
		LoggingFilePath:         "logs/tunnel-manager.log",
		LoggingFileMaxSize:      100,
		LoggingFileMaxBackups:   5,
		LoggingFileMaxAge:       30,
		LoggingFileCompress:     true,
		UIDefaultLanguage:       "",
		// The check is on and the install is off. Reading what the newest
		// release is costs one request and changes nothing; installing it
		// takes the service down, and when that may happen is the operator's
		// to say.
		UpdateCheckEnabled:       true,
		UpdateCheckIntervalHours: 24,
		UpdateAutoInstall:        false,
		// Both ways of being told are off until somebody names where to send
		// to, and the mail settings wait on what a submission port expects.
		AlertAfterSec:   300,
		AlertWebhookURL: "",
		SMTPHost:        "",
		SMTPPort:        587,
		SMTPSecurity:    SMTPSecurityStartTLS,
		SMTPAuth:        SMTPAuthPlain,
		SMTPSkipVerify:  false,
	}
}

var validLevels = map[string]bool{
	"debug":  true,
	"info":   true,
	"warn":   true,
	"error":  true,
	"dpanic": true,
	"panic":  true,
	"fatal":  true,
}

var validFormats = map[string]bool{
	"json":    true,
	"console": true,
}

// uiLanguages is every language the UI is drawn in, in the order it offers
// them. It is the list a stored ui.default_language is held against: a code
// that is not on it is a code no catalog is shipped for, and storing it would
// leave every browser that has picked nothing looking at a screen drawn in the
// name of its keys.
//
// The list is written here and not read from the UI files, because this package
// is under no obligation to know where those files live and a list taken from
// the thing it checks would agree with it whatever it said. It is the fourth
// place the languages are named, after app.js, index.html and the catalogs
// themselves, and internal/web is where a test holds all four together.
var uiLanguages = []string{"en", "ko", "ja", "zh", "es", "fr", "de", "pt-BR", "ru", "ar", "hi", "vi", "th"}

var validLanguages = func() map[string]bool {
	known := make(map[string]bool, len(uiLanguages))
	for _, code := range uiLanguages {
		known[code] = true
	}

	return known
}()

// Languages returns the languages the UI is drawn in, in the order it offers
// them. The copy is handed out rather than the list itself, so that a caller
// which sorts or appends to what it is given does not decide what this package
// accepts.
func Languages() []string {
	codes := make([]string, len(uiLanguages))
	copy(codes, uiLanguages)

	return codes
}

// The names the two path settings are refused under. They are the words an
// operator reads on the Settings screen rather than the column names, because
// the sentence they end up in is shown beside the box that was filled in.
const (
	keyFileSetting = "encryption key file"
	logFileSetting = "log file"
)

// validateDataPath holds a stored path inside the installation.
//
// Both paths this is used on, the log file and the encryption key file, are
// read against the directory the database file is in, and an absolute one used
// to win over that directory. That made either setting a way to name any file
// on the machine: the startup creates the log file and appends to it, with the
// process running as root in the installation the service registers, and the
// Logs screen reads the tail of that same file back. Saving one setting and
// pressing Restart was therefore enough to have a root process create
// /etc/cron.d/something and write into it. An operator who may change the
// settings is trusted with this installation, which is not the same as being
// trusted with the host it runs on, and the two were the same thing as long as
// the path was free.
//
// So both paths name a place inside the installation directory: a relative
// path, which every default already is, that does not climb back out of it.
// The path is refused rather than repaired, for the reason the language codes
// are: a value repaired on the way in is one the screen goes on showing as it
// was typed while the server runs on something else.
//
// The check is made with filepath, so it is the rules of the platform the
// process runs on that decide what is absolute. A drive letter and a UNC share
// are absolute on Windows and are two more ways of naming a place outside,
// which is what the volume test covers.
func validateDataPath(setting string, path string) error {
	if path == "" {
		return fmt.Errorf("the %s is required", setting)
	}

	if filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return fmt.Errorf("invalid %s path: %s. It is read against the directory the database "+
			"file is in, so it has to be a relative path and an absolute one is refused", setting, path)
	}

	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid %s path: %s. It has to stay under the directory the database "+
			"file is in, so a path that climbs out of it with .. is refused", setting, path)
	}

	if cleaned == "." {
		return fmt.Errorf("invalid %s path: %s. It names the installation directory itself "+
			"rather than a file in it", setting, path)
	}

	return nil
}

// ErrLanguageUnsupported is what a ui.default_language that names no catalog is
// refused with. It is a value rather than a sentence built on the spot, so that
// the API can tell this refusal from every other thing Validate says no to and
// answer it under a code of its own, with the list of languages in it.
var ErrLanguageUnsupported = errors.New("invalid UI language")

// The ways the connection to the mail server is protected.
const (
	SMTPSecurityNone     = "none"
	SMTPSecurityStartTLS = "starttls"
	SMTPSecurityTLS      = "tls"
)

// The ways this program logs in to the mail server.
const (
	SMTPAuthNone  = "none"
	SMTPAuthPlain = "plain"
	SMTPAuthLogin = "login"
)

// The bounds of alert.after_sec. Below ten seconds a connection that drops
// for one monitoring interval is already an outage, and past a day the alert
// arrives after whoever needed the tunnel has found out on their own.
const (
	MinAlertAfterSec = 10
	MaxAlertAfterSec = 86400
)

// maxWebhookURLLength bounds the webhook address. It is a URL somebody pastes
// out of another service, and a value much longer than any of those is a paste
// that went wrong.
const maxWebhookURLLength = 2048

// The rules of the alert settings that are refused under a code of their own.
// Each of them is a value rather than a sentence for the reason
// ErrLanguageUnsupported is: the API tells them apart and answers each with a
// sentence a screen can put in its own language.
var (
	ErrAlertAfterInvalid    = errors.New("invalid alert delay")
	ErrWebhookURLInvalid    = errors.New("invalid webhook URL")
	ErrSMTPHostInvalid      = errors.New("invalid mail server")
	ErrSMTPPortInvalid      = errors.New("invalid mail server port")
	ErrSMTPSecurityInvalid  = errors.New("invalid mail connection security")
	ErrSMTPAuthInvalid      = errors.New("invalid mail login method")
	ErrSMTPFromRequired     = errors.New("the sender address is required")
	ErrSMTPFromInvalid      = errors.New("invalid sender address")
	ErrSMTPToRequired       = errors.New("at least one recipient address is required")
	ErrSMTPToInvalid        = errors.New("invalid recipient address")
	ErrSMTPUsernameRequired = errors.New("the mail user name is required")
)

// SettingError is a refusal of one setting that carries the value that was
// refused next to the rule it broke, so that the answer can name the value
// without parsing it back out of a sentence.
type SettingError struct {
	Rule  error
	Value string
}

func (e *SettingError) Error() string {
	if e.Value == "" {
		return e.Rule.Error()
	}

	return e.Rule.Error() + ": " + e.Value
}

func (e *SettingError) Unwrap() error {
	return e.Rule
}

func refused(rule error, value string) error {
	return &SettingError{Rule: rule, Value: value}
}

// hasLineBreak says whether a value would end a mail header line early. A
// header is a line, so a value carrying a line break is a header of its own
// written by whoever typed the value.
func hasLineBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

// validMailHost says whether the mail server is a name or an address that can
// be dialled. It is held to the characters a host name is made of rather than
// resolved, because whether it resolves is a fact about the network at the time
// and not about the setting.
func validMailHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}

	if net.ParseIP(host) != nil {
		return true
	}

	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}

		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}

	return true
}

// MailRecipients splits smtp_to into the addresses it names. An entry left
// empty between two commas is passed over, so a list typed with a comma at the
// end is the list without it.
func MailRecipients(to string) []string {
	var recipients []string

	for _, entry := range strings.Split(to, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			recipients = append(recipients, entry)
		}
	}

	return recipients
}

// validateAlerts holds the alert settings to their rules. The mail settings
// that only matter while mail is on are checked only then, except that a
// value that is there is held to its shape either way, so that what the screen
// shows is never a value the server would refuse the moment mail is turned on.
func (s *Settings) validateAlerts() error {
	if s.AlertAfterSec < MinAlertAfterSec || s.AlertAfterSec > MaxAlertAfterSec {
		return refused(ErrAlertAfterInvalid, strconv.Itoa(s.AlertAfterSec))
	}

	if s.AlertWebhookURL != "" {
		parsed, err := url.Parse(s.AlertWebhookURL)
		if err != nil || len(s.AlertWebhookURL) > maxWebhookURLLength ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			hasLineBreak(s.AlertWebhookURL) {
			return refused(ErrWebhookURLInvalid, s.AlertWebhookURL)
		}
	}

	if s.SMTPHost != "" && !validMailHost(s.SMTPHost) {
		return refused(ErrSMTPHostInvalid, s.SMTPHost)
	}

	if s.SMTPPort < 1 || s.SMTPPort > 65535 {
		return refused(ErrSMTPPortInvalid, strconv.Itoa(s.SMTPPort))
	}

	switch s.SMTPSecurity {
	case SMTPSecurityNone, SMTPSecurityStartTLS, SMTPSecurityTLS:
	default:
		return refused(ErrSMTPSecurityInvalid, s.SMTPSecurity)
	}

	switch s.SMTPAuth {
	case SMTPAuthNone, SMTPAuthPlain, SMTPAuthLogin:
	default:
		return refused(ErrSMTPAuthInvalid, s.SMTPAuth)
	}

	if s.SMTPFrom != "" {
		_, err := mail.ParseAddress(s.SMTPFrom)
		if err != nil || hasLineBreak(s.SMTPFrom) {
			return refused(ErrSMTPFromInvalid, s.SMTPFrom)
		}
	}

	for _, recipient := range MailRecipients(s.SMTPTo) {
		_, err := mail.ParseAddress(recipient)
		if err != nil || hasLineBreak(recipient) {
			return refused(ErrSMTPToInvalid, recipient)
		}
	}

	if s.SMTPHost == "" {
		return nil
	}

	if s.SMTPFrom == "" {
		return refused(ErrSMTPFromRequired, "")
	}

	if len(MailRecipients(s.SMTPTo)) == 0 {
		return refused(ErrSMTPToRequired, "")
	}

	if s.SMTPAuth != SMTPAuthNone && s.SMTPUsername == "" {
		return refused(ErrSMTPUsernameRequired, "")
	}

	return nil
}

// Validate holds the rules the configuration file was checked against before
// the settings moved into the database. They matter more here than they did
// there: a file that refuses to start can be edited, while a stored setting
// that refuses to start leaves nothing to edit but the database itself.
func (s *Settings) Validate() error {
	if s.APIPort < 1 || s.APIPort > 65535 {
		return fmt.Errorf("invalid API port: %d", s.APIPort)
	}

	// api.https_enabled carries no rule of its own. It is a bool, and both of
	// its values start a server: one that speaks TLS and one that speaks in the
	// clear. What could go wrong with it, a certificate that cannot be built or
	// cannot be read back, is not a property of this set and is reported where
	// the certificate is read.

	if s.MonitoringIntervalSec <= 0 {
		return fmt.Errorf("invalid monitoring interval: %d", s.MonitoringIntervalSec)
	}

	// The ceiling is an hour. Past it a Host that came back would sit unused
	// for longer than anybody would wait on it without going to look, so a
	// number beyond it is a typo rather than a choice.
	if s.ReconnectMaxIntervalSec < 1 || s.ReconnectMaxIntervalSec > 3600 {
		return fmt.Errorf("invalid reconnect max interval: %d. It is in seconds, from 1 to 3600",
			s.ReconnectMaxIntervalSec)
	}

	if s.ReconcileIntervalSec <= 0 {
		return fmt.Errorf("invalid reconcile interval: %d", s.ReconcileIntervalSec)
	}

	// The key file is the one rule that was not in the configuration file. An
	// empty path stops the startup where the key is loaded, and a setting that
	// stops the startup can no longer be corrected by editing a file.
	err := validateDataPath(keyFileSetting, s.SecurityKeyFile)
	if err != nil {
		return err
	}

	if !validLevels[s.LoggingLevel] {
		return fmt.Errorf("invalid log level: %s", s.LoggingLevel)
	}

	if !validFormats[s.LoggingFormat] {
		return fmt.Errorf("invalid log format: %s", s.LoggingFormat)
	}

	// The log file is held to the same rule as the key file, and an empty one
	// is refused with it. Empty is not how file logging is turned off: the
	// Settings screen refuses an empty box, the startup would open no file and
	// say nothing about why, and the Logs screen would then read a file that
	// nothing writes.
	err = validateDataPath(logFileSetting, s.LoggingFilePath)
	if err != nil {
		return err
	}

	if s.LoggingFileMaxSize < 0 {
		return fmt.Errorf("invalid log max size: %d", s.LoggingFileMaxSize)
	}
	if s.LoggingFileMaxBackups < 0 {
		return fmt.Errorf("invalid log max backups: %d", s.LoggingFileMaxBackups)
	}
	if s.LoggingFileMaxAge < 0 {
		return fmt.Errorf("invalid log max age: %d", s.LoggingFileMaxAge)
	}

	// The empty language passes, because it is not a language that was got
	// wrong but this installation naming none: a browser then reads the screen
	// in the language it asks for. Everything else has to be a code a catalog
	// is shipped for, and the comparison is exact. A code is matched against
	// what the browser asks for in lowercase by the UI, but what is stored here
	// is the code itself, so "EN" and "ko-KR" are refused rather than repaired:
	// a setting repaired on the way in is one the screen goes on showing as the
	// operator typed it while the server holds something else.
	if s.UIDefaultLanguage != "" && !validLanguages[s.UIDefaultLanguage] {
		return fmt.Errorf("%w: %s", ErrLanguageUnsupported, s.UIDefaultLanguage)
	}

	// The interval is held away from zero rather than only away from negative.
	// A zero here would be a timer that fires as fast as it can be rearmed,
	// against an API that answers a public repository and counts the requests
	// of whoever asks, so the one value that must not be stored is the one a
	// box left empty reads as. The ceiling is a year, which is the far side of
	// the same thought: a number past it is a typo rather than a schedule.
	//
	// Neither of the two switches carries a rule. Both of their values are
	// states this program runs in, and what is risky about the install one is
	// what it does rather than what it is set to.
	if s.UpdateCheckIntervalHours < 1 || s.UpdateCheckIntervalHours > 8760 {
		return fmt.Errorf("invalid update check interval: %d. It is in hours, from 1 to 8760",
			s.UpdateCheckIntervalHours)
	}

	return s.validateAlerts()
}

// read returns the stored row, or nil when there is none. It does not validate,
// because the callers that reach for it are the ones that have to look at a row
// which does not pass.
func read(db *gorm.DB) (*Settings, error) {
	var s Settings

	err := db.First(&s, settingsID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the settings: %w", err)
	}

	return &s, nil
}

// Load returns the stored settings, storing the defaults first when there is no
// row yet, which is what a first startup finds.
//
// A stored set that does not pass Validate stops the startup rather than being
// repaired here. Repairing it would put the process on values nobody chose
// while the screen keeps showing the stored ones, so the failure names the flag
// that puts the defaults back.
func Load(db *gorm.DB) (*Settings, error) {
	stored, err := read(db)
	if err != nil {
		return nil, err
	}

	if stored == nil {
		created := Defaults()

		err = db.Create(&created).Error
		if err != nil {
			return nil, fmt.Errorf("failed to store the default settings: %w", err)
		}

		return &created, nil
	}

	err = stored.Validate()
	if err != nil {
		return nil, fmt.Errorf("the stored settings are refused: %w. Start with -reset-settings "+
			"to put every setting back to its default", err)
	}

	return stored, nil
}

// RepairPaths puts a stored path that names a place outside the installation
// directory back to its default, and reports what it changed so the caller can
// log it. It is run by the startup, before the settings are read.
//
// It is here rather than left to Load refusing the row, because the rule the
// two paths are held to is newer than the installations it applies to. A path
// that was stored while an absolute one was accepted is a setting somebody
// chose and the server used to run on, so a startup that refused it would take
// an installation that was working and leave it with no server, and the screen
// that would correct the setting is served by that server. The way out would
// be -reset-settings, which puts back every other setting as well.
//
// Coming up on the default is the safe half of both: the process runs, the log
// and the key are inside the installation directory where the rule wants them,
// and the operator reads in the log what was replaced. What it costs is that a
// key deliberately kept on a volume of its own is no longer read, and the
// startup then creates a new key beside the database and cannot open the
// stored passwords with it. That is loud rather than quiet: the check of the
// stored passwords stops the startup, and the line this writes says which path
// was dropped.
//
// The repair is stored and not just held for this process, so that the screen
// shows the path the server is running on. Only the columns that were repaired
// are written, so a row that is refused for some other reason is left for Load
// to report rather than being half repaired here.
func RepairPaths(db *gorm.DB) ([]Change, error) {
	stored, err := read(db)
	if err != nil {
		return nil, err
	}

	if stored == nil {
		return nil, nil
	}

	defaults := Defaults()

	var changes []Change

	repaired := map[string]interface{}{}

	if validateDataPath(keyFileSetting, stored.SecurityKeyFile) != nil {
		changes = append(changes, Change{
			Name: "security.key_file",
			From: stored.SecurityKeyFile,
			To:   defaults.SecurityKeyFile,
		})
		repaired["security_key_file"] = defaults.SecurityKeyFile
	}

	if validateDataPath(logFileSetting, stored.LoggingFilePath) != nil {
		changes = append(changes, Change{
			Name: "logging.file.path",
			From: stored.LoggingFilePath,
			To:   defaults.LoggingFilePath,
		})
		repaired["logging_file_path"] = defaults.LoggingFilePath
	}

	if len(repaired) == 0 {
		return nil, nil
	}

	err = db.Model(&Settings{}).Where("id = ?", settingsID).Updates(repaired).Error
	if err != nil {
		return nil, fmt.Errorf("failed to store the repaired path settings: %w", err)
	}

	return changes, nil
}

// Save validates before it writes, so a set that would keep the process from
// starting again never reaches the database. The whole row is written in one
// statement: a save that landed field by field could leave a set that no screen
// ever asked for if the process ended in the middle of it.
func Save(db *gorm.DB, s *Settings) error {
	err := s.Validate()
	if err != nil {
		return err
	}

	stored := *s
	stored.ID = settingsID

	err = db.Save(&stored).Error
	if err != nil {
		return fmt.Errorf("failed to store the settings: %w", err)
	}

	*s = stored

	return nil
}

// Reset puts every setting back to its default and reports what was stored
// before, so the caller can say what it changed. It is the way out of a stored
// set that keeps the process from starting, which is why it reads the row
// without validating it.
func Reset(db *gorm.DB) (before *Settings, after *Settings, err error) {
	before, err = read(db)
	if err != nil {
		return nil, nil, err
	}

	defaults := Defaults()

	err = Save(db, &defaults)
	if err != nil {
		return nil, nil, err
	}

	return before, &defaults, nil
}

// WarnForwardsOnAPIPort names every local forward and every SOCKS5 proxy that
// opens the port the API is set to be served on. Reset calls for it: it puts the API port back to its
// default, and a forward may have been given that port while the API was on
// another one. The reset is not refused over it, because it is the way out of a
// process that will not start, and the next start is not stopped by it either:
// whichever of the two opens the port first holds it, and when that is the
// forward the API is served on another port. What is left is to say so, so that
// the operator knows to look for the screen elsewhere or to move the forward.
//
// A read that fails is reported and left there, since the reset it follows has
// already been stored.
func WarnForwardsOnAPIPort(db *gorm.DB, logger *zap.Logger, apiPort int) {
	var forwards []models.LocalForward

	err := db.Where("local_port = ?", apiPort).Order("host_id, number").Find(&forwards).Error
	if err != nil {
		logger.Warn("failed to fetch local forwards",
			logid.LocalForwardListFetchFailed.Field(),
			zap.Error(err))

		return
	}

	for _, forward := range forwards {
		logger.Warn("a local forward opens the port the API was put back to, "+
			"so the next start may listen on another port",
			logid.SettingsDefaultPortHeldByForward.Field(),
			zap.Uint("local_forward_number", forward.Number),
			zap.Uint("host_id", forward.HostID),
			zap.Int("local_port", forward.LocalPort))
	}

	// A proxy is named while it is switched on, whether its Host is enabled or
	// not, since enabling the Host is what opens it.
	var hosts []models.Host

	err = db.Where("socks_enabled = ? AND socks_port = ?", true, apiPort).Order("id").Find(&hosts).Error
	if err != nil {
		logger.Warn("failed to fetch Hosts",
			logid.HostListFetchFailed.Field(),
			zap.Error(err))

		return
	}

	for _, host := range hosts {
		logger.Warn("the SOCKS5 proxy of a Host opens the port the API was put back to, "+
			"so the next start may listen on another port",
			logid.SettingsDefaultPortHeldBySocks.Field(),
			zap.Uint("host_id", host.ID),
			zap.Int("socks_port", host.SocksPort))
	}
}

// Change is one setting that differs between two sets, named as the
// configuration file used to spell it.
type Change struct {
	Name string
	From string
	To   string
}

// value is one setting as a name and the text of its value. Everything that
// walks the settings one by one goes through this list, so a setting that is
// added is named in one place.
type value struct {
	name string
	text string
}

func values(s *Settings) []value {
	return []value{
		{"api.port", strconv.Itoa(s.APIPort)},
		{"api.https_enabled", strconv.FormatBool(s.APIHTTPSEnabled)},
		{"monitoring.interval_sec", strconv.Itoa(s.MonitoringIntervalSec)},
		{"monitoring.reconnect_max_interval_sec", strconv.Itoa(s.ReconnectMaxIntervalSec)},
		{"reconcile.interval_sec", strconv.Itoa(s.ReconcileIntervalSec)},
		{"security.key_file", s.SecurityKeyFile},
		{"logging.level", s.LoggingLevel},
		{"logging.format", s.LoggingFormat},
		{"logging.file.path", s.LoggingFilePath},
		{"logging.file.max_size", strconv.Itoa(s.LoggingFileMaxSize)},
		{"logging.file.max_backups", strconv.Itoa(s.LoggingFileMaxBackups)},
		{"logging.file.max_age", strconv.Itoa(s.LoggingFileMaxAge)},
		{"logging.file.compress", strconv.FormatBool(s.LoggingFileCompress)},
		{"ui.default_language", s.UIDefaultLanguage},
		{"update.check_enabled", strconv.FormatBool(s.UpdateCheckEnabled)},
		{"update.check_interval_hours", strconv.Itoa(s.UpdateCheckIntervalHours)},
		{"update.auto_install", strconv.FormatBool(s.UpdateAutoInstall)},
		{"alert.after_sec", strconv.Itoa(s.AlertAfterSec)},
		{"alert.webhook_url", s.AlertWebhookURL},
		{"alert.smtp.host", s.SMTPHost},
		{"alert.smtp.port", strconv.Itoa(s.SMTPPort)},
		{"alert.smtp.security", s.SMTPSecurity},
		{"alert.smtp.auth", s.SMTPAuth},
		{"alert.smtp.username", s.SMTPUsername},
		{"alert.smtp.password", maskedSecret(s.SMTPPassword)},
		{"alert.smtp.from", s.SMTPFrom},
		{"alert.smtp.to", s.SMTPTo},
		{"alert.smtp.skip_verify", strconv.FormatBool(s.SMTPSkipVerify)},
	}
}

// SecretMask is what a stored secret is written as wherever the settings are
// named one by one: in the changes a save answers with and in the lines a
// reset logs. Those go to a browser and to the log file, and the password of
// the mail server is not for either, sealed or not.
//
// The mask is the same whatever the secret is, so one password replaced by
// another is not a change this list can see. The save that replaces it says so
// itself.
const SecretMask = "********"

func maskedSecret(secret string) string {
	if secret == "" {
		return ""
	}

	return SecretMask
}

// Diff returns the settings that differ between the two sets. A nil before
// stands for a database that held no settings at all, and every setting is
// reported as new.
func Diff(before *Settings, after *Settings) []Change {
	newValues := values(after)

	if before == nil {
		changes := make([]Change, 0, len(newValues))
		for _, v := range newValues {
			changes = append(changes, Change{Name: v.name, From: "", To: v.text})
		}
		return changes
	}

	oldValues := values(before)

	var changes []Change

	for i, v := range newValues {
		if oldValues[i].text == v.text {
			continue
		}
		changes = append(changes, Change{Name: v.name, From: oldValues[i].text, To: v.text})
	}

	return changes
}
