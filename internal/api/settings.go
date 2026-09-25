package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/alert"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm"
)

// databaseLogLevel is the handle the level of the logger gorm writes through is
// changed by. *database.LogLevel is what is handed in; it is named as an
// interface so that this package keeps the single dependency it has on that one
// (the *gorm.DB itself) and so that a test can watch what the save puts on it.
type databaseLogLevel interface {
	Set(level string)
}

// SettingsHandler serves the Settings screen. It is apart from Handler because
// it needs no tunnel manager and no cipher, and because it is the one handler
// that changes how this process runs rather than what it keeps rows about.
type SettingsHandler struct {
	db     *gorm.DB
	logger *zap.Logger
	// logLevel and gormLevel are the two ends the stored logging.level reaches
	// the running process through. They are handed in from the startup that
	// built the loggers around them, which is what lets the level hold without a
	// restart. Built here instead they would be handles on nothing: the loggers
	// already in use would keep writing at the level they were built with.
	//
	// There are two because the application lines and the lines gorm writes go
	// through different level checks. Setting only the first would leave the
	// statements gorm reports at the level the process started on, which is the
	// half of "debug" an operator usually turns it on for.
	logLevel  zap.AtomicLevel
	gormLevel databaseLogLevel
	// startup is the set of settings this process read as it came up. It is
	// held rather than dropped so that a read can say which stored settings
	// this process is not running on. Nothing else holds that: the answer to a
	// save says it once, and a screen drawn later, or in another browser, has
	// nothing to read it out of.
	//
	// It is a copy and no save writes to it, so it stays what this process was
	// started with for as long as the process runs. A setting that is put into
	// place as it is stored is left out of the comparison by name, so the copy
	// standing still is not what decides whether a setting is reported.
	startup settings.Settings
	// installDir is the directory the database file is in, which is what a
	// stored path that is not absolute is read against.
	installDir string
	// cipher seals the password of the mail server as it is stored, with the
	// key the passwords of the Hosts are sealed with.
	cipher *crypto.Cipher
	// alerts is what the two test presses send through. It is the sender the
	// alert watcher uses, so a test that goes through is one an alert would
	// go through as well.
	alerts *alert.Sender
}

func NewSettingsHandler(db *gorm.DB, logger *zap.Logger, logLevel zap.AtomicLevel,
	gormLevel databaseLogLevel, startup settings.Settings, installDir string,
	cipher *crypto.Cipher, alerts *alert.Sender) *SettingsHandler {
	return &SettingsHandler{
		db:         db,
		logger:     logger,
		logLevel:   logLevel,
		gormLevel:  gormLevel,
		startup:    startup,
		installDir: installDir,
		cipher:     cipher,
		alerts:     alerts,
	}
}

// appliedNow and appliedRestart are what became of a stored change. A setting
// the process took on is told from one that is only stored, because the two
// leave the operator with different work to do.
const (
	appliedNow     = "now"
	appliedRestart = "restart"
)

// settingsChange is one setting a save altered, named as the configuration file
// used to spell it.
type settingsChange struct {
	Name    string `json:"name"`
	From    string `json:"from"`
	To      string `json:"to"`
	Applied string `json:"applied"`
}

// settingsSaved is what a save answers with. The stored settings come back
// along with the changes, so the screen draws what the database holds rather
// than what it sent.
//
// restart_required is carried as well as the per change field, because what the
// operator has to do next is about the save as a whole: a screen that had to
// work it out from the list would be the second place that knows the rule.
type settingsSaved struct {
	Settings        *settings.Settings `json:"settings"`
	Changes         []settingsChange   `json:"changes"`
	RestartRequired bool               `json:"restart_required"`
}

// pendingSetting is one setting that is stored with a value this process is not
// running on. running is what the process was started with and stored is what
// it would come back on.
type pendingSetting struct {
	Name    string `json:"name"`
	Running string `json:"running"`
	Stored  string `json:"stored"`
}

// settingsView is what a read answers with. The settings are carried flat, the
// way they were before anything stood beside them, so a client that reads a
// setting out of this answer goes on reading it where it always was, and what
// waits for a restart arrives in the same answer the screen is filled from.
type settingsView struct {
	*settings.Settings

	PendingRestart []pendingSetting `json:"pending_restart"`

	// InstallDir is the directory a stored path that is not absolute is read
	// against. It is here so that the screen can say which directory that is
	// rather than describe it, since what it is depends on how the service was
	// started and the operator cannot see it from a browser.
	InstallDir string `json:"install_dir"`

	// SMTPPasswordSet says whether a password for the mail server is stored.
	// It stands in for the password itself, which no answer carries.
	SMTPPasswordSet bool `json:"smtp_password_set"`
}

// GetSettings answers with what is stored, along with what is stored but not
// being run on.
//
// @Summary      The stored settings, and in pending_restart the ones this process is not running on
// @Tags         settings
// @Produce  json
// @Success  200  {object}  models.Response{data=api.settingsView}
// @Router       /settings [get]
func (h *SettingsHandler) GetSettings(c echo.Context) error {
	stored, err := settings.Load(h.db)
	if err != nil {
		h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: settingsView{
			Settings:        stored,
			PendingRestart:  h.pendingRestart(stored),
			InstallDir:      h.installDir,
			SMTPPasswordSet: stored.SMTPPassword != "",
		},
	})
}

// updateSettingsRequest is the whole of what a save is allowed to name. It
// stands between the body and the stored settings so that what a client can
// write is decided here, next to the handler, rather than by which fields
// settings.Settings happens to spell in JSON.
//
// Bound onto the stored set directly, the two lists were the same list: every
// field that struct named in JSON was a field a body wrote, and a field this
// end is meant to decide became writable the moment somebody added it with a
// tag on it. The stored timestamp was such a field once and is kept out of the
// JSON for it, which works only for as long as whoever adds the next one
// remembers why that tag is there. Here there is nothing to remember: a field
// that is not written out below cannot be reached by a request at all.
//
// Every field is a pointer because a save sends the settings it has. The
// screens write the boxes of one card at a time, so a body names some of the
// settings and leaves the rest alone. nil is the field the body left out, and
// a value that arrived is stored as it stands, the zero ones included: an empty
// language is the installation naming none, which is a choice and not a box
// nobody filled in.
type updateSettingsRequest struct {
	APIPort         *int  `json:"api_port"`
	APIHTTPSEnabled *bool `json:"api_https_enabled"`

	MonitoringIntervalSec   *int `json:"monitoring_interval_sec"`
	ReconnectMaxIntervalSec *int `json:"reconnect_max_interval_sec"`
	ReconcileIntervalSec    *int `json:"reconcile_interval_sec"`

	SecurityKeyFile *string `json:"security_key_file"`

	LoggingLevel  *string `json:"logging_level"`
	LoggingFormat *string `json:"logging_format"`

	LoggingFilePath       *string `json:"logging_file_path"`
	LoggingFileMaxSize    *int    `json:"logging_file_max_size"`
	LoggingFileMaxBackups *int    `json:"logging_file_max_backups"`
	LoggingFileMaxAge     *int    `json:"logging_file_max_age"`
	LoggingFileCompress   *bool   `json:"logging_file_compress"`

	UIDefaultLanguage *string `json:"ui_default_language"`

	UpdateCheckEnabled       *bool `json:"update_check_enabled"`
	UpdateCheckIntervalHours *int  `json:"update_check_interval_hours"`
	UpdateAutoInstall        *bool `json:"update_auto_install"`

	AlertAfterSec   *int    `json:"alert_after_sec"`
	AlertWebhookURL *string `json:"alert_webhook_url"`

	SMTPHost     *string `json:"smtp_host"`
	SMTPPort     *int    `json:"smtp_port"`
	SMTPSecurity *string `json:"smtp_security"`
	SMTPAuth     *string `json:"smtp_auth"`
	SMTPUsername *string `json:"smtp_username"`
	// SMTPPassword is the password of the mail server in the clear, sealed
	// before it is stored. It is the one field where an empty value is not
	// stored as it stands: a read never carries the password, so a screen
	// that sends back what it read sends none, and that has to leave the
	// stored one alone. Clearing it is what SMTPPasswordClear is for.
	SMTPPassword      *string `json:"smtp_password"`
	SMTPPasswordClear *bool   `json:"smtp_password_clear"`
	SMTPFrom          *string `json:"smtp_from"`
	SMTPTo            *string `json:"smtp_to"`
	SMTPSkipVerify    *bool   `json:"smtp_skip_verify"`
}

// apply puts what the body named onto the stored settings and leaves the rest
// of them as they were.
//
// The fields are copied one at a time rather than by anything that walks the
// two structs, because the copying is the list: what a request can write is
// read off these lines, and a field of the stored settings that is not on them
// is one no body reaches. Nothing is validated here. The set that comes out
// goes through the rules as a whole, which is where a value that was refused
// is told apart from a database that could not be written to.
func (r *updateSettingsRequest) apply(s *settings.Settings) {
	if r.APIPort != nil {
		s.APIPort = *r.APIPort
	}
	if r.APIHTTPSEnabled != nil {
		s.APIHTTPSEnabled = *r.APIHTTPSEnabled
	}

	if r.MonitoringIntervalSec != nil {
		s.MonitoringIntervalSec = *r.MonitoringIntervalSec
	}
	if r.ReconnectMaxIntervalSec != nil {
		s.ReconnectMaxIntervalSec = *r.ReconnectMaxIntervalSec
	}
	if r.ReconcileIntervalSec != nil {
		s.ReconcileIntervalSec = *r.ReconcileIntervalSec
	}

	if r.SecurityKeyFile != nil {
		s.SecurityKeyFile = *r.SecurityKeyFile
	}

	if r.LoggingLevel != nil {
		s.LoggingLevel = *r.LoggingLevel
	}
	if r.LoggingFormat != nil {
		s.LoggingFormat = *r.LoggingFormat
	}

	if r.LoggingFilePath != nil {
		s.LoggingFilePath = *r.LoggingFilePath
	}
	if r.LoggingFileMaxSize != nil {
		s.LoggingFileMaxSize = *r.LoggingFileMaxSize
	}
	if r.LoggingFileMaxBackups != nil {
		s.LoggingFileMaxBackups = *r.LoggingFileMaxBackups
	}
	if r.LoggingFileMaxAge != nil {
		s.LoggingFileMaxAge = *r.LoggingFileMaxAge
	}
	if r.LoggingFileCompress != nil {
		s.LoggingFileCompress = *r.LoggingFileCompress
	}

	if r.UIDefaultLanguage != nil {
		s.UIDefaultLanguage = *r.UIDefaultLanguage
	}

	if r.UpdateCheckEnabled != nil {
		s.UpdateCheckEnabled = *r.UpdateCheckEnabled
	}
	if r.UpdateCheckIntervalHours != nil {
		s.UpdateCheckIntervalHours = *r.UpdateCheckIntervalHours
	}
	if r.UpdateAutoInstall != nil {
		s.UpdateAutoInstall = *r.UpdateAutoInstall
	}

	if r.AlertAfterSec != nil {
		s.AlertAfterSec = *r.AlertAfterSec
	}
	if r.AlertWebhookURL != nil {
		s.AlertWebhookURL = strings.TrimSpace(*r.AlertWebhookURL)
	}

	if r.SMTPHost != nil {
		s.SMTPHost = strings.TrimSpace(*r.SMTPHost)
	}
	if r.SMTPPort != nil {
		s.SMTPPort = *r.SMTPPort
	}
	if r.SMTPSecurity != nil {
		s.SMTPSecurity = *r.SMTPSecurity
	}
	if r.SMTPAuth != nil {
		s.SMTPAuth = *r.SMTPAuth
	}
	if r.SMTPUsername != nil {
		s.SMTPUsername = *r.SMTPUsername
	}
	if r.SMTPFrom != nil {
		s.SMTPFrom = strings.TrimSpace(*r.SMTPFrom)
	}
	if r.SMTPTo != nil {
		s.SMTPTo = strings.TrimSpace(*r.SMTPTo)
	}
	if r.SMTPSkipVerify != nil {
		s.SMTPSkipVerify = *r.SMTPSkipVerify
	}
}

// applyPassword puts the password the body carries onto the stored settings,
// sealed. It is apart from apply because sealing can fail, and a failure there
// is this end failing rather than a value that was refused.
//
// A password that is sent is stored; an empty or missing one leaves the stored
// one as it is; smtp_password_clear with no password removes it.
func (r *updateSettingsRequest) applyPassword(s *settings.Settings, cipher *crypto.Cipher) error {
	if r.SMTPPassword != nil && *r.SMTPPassword != "" {
		sealed, err := cipher.Encrypt(*r.SMTPPassword)
		if err != nil {
			return err
		}

		s.SMTPPassword = sealed

		return nil
	}

	if r.SMTPPasswordClear != nil && *r.SMTPPasswordClear {
		s.SMTPPassword = ""
	}

	return nil
}

// passwordChanged says whether the body asked for the stored password to be
// replaced or removed.
func (r *updateSettingsRequest) passwordChanged() bool {
	return (r.SMTPPassword != nil && *r.SMTPPassword != "") ||
		(r.SMTPPasswordClear != nil && *r.SMTPPasswordClear)
}

// mailTargetMoved says whether the password stored for one mail server would
// now be sent somewhere else, or sent where it can be read on the way: the
// server, its port, the user it logs in as or the connection security changed,
// or the check of the certificate was turned off.
func mailTargetMoved(before *settings.Settings, after *settings.Settings) bool {
	return before.SMTPHost != after.SMTPHost ||
		before.SMTPPort != after.SMTPPort ||
		before.SMTPUsername != after.SMTPUsername ||
		before.SMTPSecurity != after.SMTPSecurity ||
		(!before.SMTPSkipVerify && after.SMTPSkipVerify)
}

// guardStoredPassword keeps the stored mail password with the server it was
// given for.
//
// A read never answers with the password, and that is worth nothing if a body
// can name another server and have the stored password sent to it: a save
// would send it with the next alert and a test would send it at once. So a
// body that moves the target has to carry the password again. One that turns
// the login off or clears the password does not, and the stored password is
// dropped for it: kept, it would go to the new server the moment the login was
// turned back on, which the next body could do without moving anything.
func guardStoredPassword(req *updateSettingsRequest, before *settings.Settings, after *settings.Settings) *refusal {
	if before.SMTPPassword == "" || !mailTargetMoved(before, after) {
		return nil
	}

	if req.SMTPPassword != nil && *req.SMTPPassword != "" {
		return nil
	}

	if after.SMTPAuth == settings.SMTPAuthNone || (req.SMTPPasswordClear != nil && *req.SMTPPasswordClear) {
		after.SMTPPassword = ""

		return nil
	}

	return refuse(http.StatusBadRequest, errSettingsSMTPPasswordRequired)
}

// settingsRefusal answers a set Validate refused. The language and the alert
// settings are answered under codes of their own; the rest arrive as a
// sentence in {reason}, since there is nothing for a screen to do with a port
// number but repeat it.
func settingsRefusal(err error, updated *settings.Settings, fallback errorCode) *refusal {
	if errors.Is(err, settings.ErrLanguageUnsupported) {
		return refuse(http.StatusBadRequest, errSettingsLanguageUnsupported, errorArgs{
			"language":  updated.UIDefaultLanguage,
			"languages": strings.Join(settings.Languages(), ", "),
		})
	}

	var refused *settings.SettingError
	if errors.As(err, &refused) {
		value := refused.Value

		switch refused.Rule {
		case settings.ErrAlertAfterInvalid:
			return refuse(http.StatusBadRequest, errSettingsAlertAfterInvalid, errorArgs{
				"value": value,
				"min":   strconv.Itoa(settings.MinAlertAfterSec),
				"max":   strconv.Itoa(settings.MaxAlertAfterSec),
			})
		case settings.ErrWebhookURLInvalid:
			return refuse(http.StatusBadRequest, errSettingsWebhookURLInvalid, errorArgs{"value": value})
		case settings.ErrSMTPHostInvalid:
			return refuse(http.StatusBadRequest, errSettingsSMTPHostInvalid, errorArgs{"value": value})
		case settings.ErrSMTPPortInvalid:
			return refuse(http.StatusBadRequest, errSettingsSMTPPortInvalid, errorArgs{"value": value})
		case settings.ErrSMTPSecurityInvalid:
			return refuse(http.StatusBadRequest, errSettingsSMTPSecurityInvalid, errorArgs{"value": value})
		case settings.ErrSMTPAuthInvalid:
			return refuse(http.StatusBadRequest, errSettingsSMTPAuthInvalid, errorArgs{"value": value})
		case settings.ErrSMTPFromRequired:
			return refuse(http.StatusBadRequest, errSettingsSMTPFromRequired)
		case settings.ErrSMTPFromInvalid:
			return refuse(http.StatusBadRequest, errSettingsSMTPFromInvalid, errorArgs{"value": value})
		case settings.ErrSMTPToRequired:
			return refuse(http.StatusBadRequest, errSettingsSMTPToRequired)
		case settings.ErrSMTPToInvalid:
			return refuse(http.StatusBadRequest, errSettingsSMTPToInvalid, errorArgs{"value": value})
		case settings.ErrSMTPUsernameRequired:
			return refuse(http.StatusBadRequest, errSettingsSMTPUserRequired)
		}
	}

	return refuse(http.StatusBadRequest, fallback, errorArgs{"reason": err.Error()})
}

// UpdateSettings stores what the request carries and puts into place whatever
// the running process can take on.
//
// @Summary      Store the settings in the body over the stored ones
// @Description  Answers with what changed and whether a restart is needed. logging.level is the one setting this process takes on without being started again.
// @Description  A new api_port that a local forward opens as its local port is refused with 409; data then carries that local forward and suggested_port, a port neither a local forward, a SOCKS5 proxy nor the stored or asked for api_port holds, or 0 when there is none.
// @Description  A new api_port that the SOCKS5 proxy of a Host opens is refused with 409 under its own error_code; data then carries that Host in socks_host, local_forward left empty, and suggested_port.
// @Description  smtp_password is write-only: a read says only smtp_password_set, an empty or missing one keeps the stored password, and smtp_password_clear removes it. While a password is stored, a body that changes smtp_host, smtp_port, smtp_username or smtp_security, or turns smtp_skip_verify on, has to carry smtp_password, or it is refused with 400 under settings.smtp_password.required; with smtp_auth none or smtp_password_clear the stored password is removed instead.
// @Tags         settings
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.updateSettingsRequest  true  "The settings as they should stand"
// @Success  200  {object}  models.Response{data=api.settingsSaved}
// @Failure  400  {object}  api.errorBody  "A setting broke one of the rules. Nothing was stored"
// @Failure  409  {object}  api.errorBody{data=api.apiPortTaken}  "api_port is the local port of a local forward or the port of a SOCKS5 proxy. Nothing was stored"
// @Router       /settings [put]
func (h *SettingsHandler) UpdateSettings(c echo.Context) error {
	stored, err := settings.Load(h.db)
	if err != nil {
		h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	before := *stored

	// The body is read into a request of its own and put onto what is stored,
	// so a request that names some of the settings changes those and leaves the
	// rest alone, and a field the request does not carry is one no body can
	// write. Bound onto the stored set itself, every setting the body left out
	// would still keep its value, but the list of what a body may name would be
	// whatever settings.Settings spells in JSON.
	var req updateSettingsRequest

	err = c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	updated := *stored
	req.apply(&updated)

	// The rules are run here as well as inside Save so that a value they refuse
	// is answered as a bad request while a database that could not be written
	// to stays a 500. Save reports the two as one error, and the client can act
	// on only one of them.
	err = updated.Validate()
	if err != nil {
		return settingsRefusal(err, &updated, errSettingsRefused).answer(c)
	}

	refused := guardStoredPassword(&req, &before, &updated)
	if refused != nil {
		return refused.answer(c)
	}

	// Sealed after the rules are run, so that a set that is refused never
	// has a password sealed for it.
	err = req.applyPassword(&updated, h.cipher)
	if err != nil {
		h.logger.Error("failed to encrypt the mail password", logid.SettingsSmtpPasswordSealFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsSMTPPasswordSeal)
	}

	// The settings were read above, before the transaction, for the reason
	// storedAPIPort gives: the pool holds one connection.
	tx := h.db.Begin()
	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

	// Only a port that changes is held to the local forwards, so a save of
	// another setting is not refused over a port that is already stored.
	if updated.APIPort != before.APIPort {
		refused, err := apiPortRefused(tx, apiPortCodes{errSettingsAPIPortLocalForward, errSettingsAPIPortSocks}, updated.APIPort, before.APIPort,
			h.startup.APIPort)
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to fetch local forwards", logid.LocalForwardListFetchFailed.Field(),
				zap.Error(err), zap.Int("api_port", updated.APIPort))
			return failure(c, http.StatusInternalServerError, errLocalForwardListFailed)
		}
		if refused != nil {
			tx.Rollback()
			return refused.answer(c)
		}
	}

	err = settings.Save(tx, &updated)
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to store the settings", logid.SettingsStoreFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsStoreFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	changes := h.applyChanges(&before, &updated)

	// A password replaced by another is written as the same mask on both
	// sides, so the list above does not see it. The save says so itself,
	// since a save that answered "nothing changed" to a new password would be
	// read as one that did not take it.
	if req.passwordChanged() && before.SMTPPassword != updated.SMTPPassword && !namesChange(changes, smtpPasswordSetting) {
		changes = append(changes, settingsChange{
			Name:    smtpPasswordSetting,
			From:    maskedPassword(before.SMTPPassword),
			To:      maskedPassword(updated.SMTPPassword),
			Applied: appliedNow,
		})
	}

	restart := false
	for _, change := range changes {
		if change.Applied == appliedRestart {
			restart = true
		}
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: settingsSaved{
			Settings:        &updated,
			Changes:         changes,
			RestartRequired: restart,
		},
	})
}

// appliedNowSettings is every setting this process puts into place as it is
// stored, each along with what putting it into place means.
//
// logging.level and ui.default_language are the list. The loggers were built
// around the handle above at startup, so setting the level reaches every logger
// that was handed out, including the one gorm writes through. The periods of
// the monitoring and the reconcile loops are read once by the loops that run on
// them and the port is read once by the server that listens on it, so those are
// stored and wait for a restart.
//
// Which setting belongs in that list is decided here, next to the code that
// puts the value in place, and is carried to the screen in the answer. A screen
// that held the list of its own would go on saying a setting took hold after
// this stopped putting it into place.
//
// It is the one list because two answers are built from it: a save says what
// became of each change, and a read leaves out of what waits for a restart the
// settings that never wait for one. Decided twice, the screen could call the
// same setting in place in one card and still waiting in another.
var appliedNowSettings = map[string]func(h *SettingsHandler, s *settings.Settings) bool{
	"logging.level": func(h *SettingsHandler, s *settings.Settings) bool {
		return h.setLogLevel(s.LoggingLevel)
	},
	// The language has nothing to put into place. This process never reads it:
	// it is read by the browser, out of the answer to the very request that
	// stored it and out of every read after that, so the next screen that is
	// drawn is already drawn in it. Left out of this list it would be reported
	// as waiting for a restart that would change nothing, and the screen would
	// carry that notice until somebody restarted the service to clear it.
	"ui.default_language": func(h *SettingsHandler, s *settings.Settings) bool {
		return true
	},
	// The alert settings are read by the alert watcher on every scan, so the
	// scan after a save runs on what was stored.
	"alert.after_sec":        alertSettingInPlace,
	"alert.webhook_url":      alertSettingInPlace,
	"alert.smtp.host":        alertSettingInPlace,
	"alert.smtp.port":        alertSettingInPlace,
	"alert.smtp.security":    alertSettingInPlace,
	"alert.smtp.auth":        alertSettingInPlace,
	"alert.smtp.username":    alertSettingInPlace,
	smtpPasswordSetting:      alertSettingInPlace,
	"alert.smtp.from":        alertSettingInPlace,
	"alert.smtp.to":          alertSettingInPlace,
	"alert.smtp.skip_verify": alertSettingInPlace,
}

// smtpPasswordSetting is the name the password of the mail server goes by in
// the list of changes.
const smtpPasswordSetting = "alert.smtp.password"

func alertSettingInPlace(h *SettingsHandler, s *settings.Settings) bool {
	return true
}

// namesChange says whether a list of changes has one for the setting.
func namesChange(changes []settingsChange, name string) bool {
	for _, change := range changes {
		if change.Name == name {
			return true
		}
	}

	return false
}

func maskedPassword(sealed string) string {
	if sealed == "" {
		return ""
	}

	return settings.SecretMask
}

// applyChanges puts into place what this process can take on and reports every
// change with what became of it.
func (h *SettingsHandler) applyChanges(before *settings.Settings, after *settings.Settings) []settingsChange {
	diff := settings.Diff(before, after)

	// The list is built even when it is empty, so that a save that changed
	// nothing answers with an empty list rather than with a null the screen
	// would have to tell from a list it failed to read.
	changes := make([]settingsChange, 0, len(diff))

	for _, change := range diff {
		state := appliedRestart

		put, now := appliedNowSettings[change.Name]
		if now && put(h, after) {
			state = appliedNow
		}

		changes = append(changes, settingsChange{
			Name:    change.Name,
			From:    change.From,
			To:      change.To,
			Applied: state,
		})
	}

	return changes
}

// pendingRestart is every setting that is stored with a value this process is
// not running on. It is what the screen says a restart is still owed for.
//
// It is worked out on every read rather than remembered from the save that
// caused it, so that a session opened an hour later and a second browser are
// told the same thing, and so that it clears itself: the process that comes
// back from a restart reads the stored settings as the ones it runs on, and
// there is nothing left to compare against.
//
// The settings above are left out. Their stored value is in place already,
// while the set this process started with still holds the value it started
// with, so a plain comparison would report a log level that is being written
// at. A level the logger refused is the one case that is left out here and
// reported as waiting by a save; it is logged where it is refused, and a level
// that passed Validate is one zap knows.
func (h *SettingsHandler) pendingRestart(stored *settings.Settings) []pendingSetting {
	diff := settings.Diff(&h.startup, stored)

	// The list is built even when it is empty for the same reason the changes
	// of a save are: the screen tells an empty list from one it failed to read
	// by what is in it, not by whether it is there.
	pending := make([]pendingSetting, 0, len(diff))

	for _, change := range diff {
		_, now := appliedNowSettings[change.Name]
		if now {
			continue
		}

		pending = append(pending, pendingSetting{
			Name:    change.Name,
			Running: change.From,
			Stored:  change.To,
		})
	}

	return pending
}

// setLogLevel puts the stored level on the running loggers and reports whether
// it holds. The level passed the rules before it was stored, so a level zap
// cannot read means the two lists of levels drifted apart: that is worth a log
// of its own, and the process stays at the level it was running at rather than
// the answer claiming a level nothing is writing at.
//
// The database handle is set after zap and only once zap took the level, so
// that a level that was refused does not reach half of the logging.
func (h *SettingsHandler) setLogLevel(level string) bool {
	parsed, err := zapcore.ParseLevel(level)
	if err != nil {
		h.logger.Error("the stored log level is not one the logger knows",
			logid.SettingsLogLevelUnknown.Field(),
			zap.String("level", level),
			zap.Error(err))

		return false
	}

	h.logLevel.SetLevel(parsed)
	h.gormLevel.Set(level)

	return true
}
