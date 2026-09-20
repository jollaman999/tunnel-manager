package api

import (
	"errors"
	"net/http"
	"strings"

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
}

func NewSettingsHandler(db *gorm.DB, logger *zap.Logger, logLevel zap.AtomicLevel,
	gormLevel databaseLogLevel, startup settings.Settings, installDir string) *SettingsHandler {
	return &SettingsHandler{
		db:         db,
		logger:     logger,
		logLevel:   logLevel,
		gormLevel:  gormLevel,
		startup:    startup,
		installDir: installDir,
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
}

// GetSettings answers with what is stored, along with what is stored but not
// being run on.
func (h *SettingsHandler) GetSettings(c echo.Context) error {
	stored, err := settings.Load(h.db)
	if err != nil {
		h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: settingsView{
			Settings:       stored,
			PendingRestart: h.pendingRestart(stored),
			InstallDir:     h.installDir,
		},
	})
}

// UpdateSettings stores what the request carries and puts into place whatever
// the running process can take on.
func (h *SettingsHandler) UpdateSettings(c echo.Context) error {
	stored, err := settings.Load(h.db)
	if err != nil {
		h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	before := *stored

	// The body is bound onto what is stored, so a request that names some of
	// the settings changes those and leaves the rest alone. Bound onto an empty
	// set, every setting the body left out would arrive as a zero value and be
	// stored as one or refused as one.
	updated := *stored

	err = c.Bind(&updated)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	// The rules are run here as well as inside Save so that a value they refuse
	// is answered as a bad request while a database that could not be written
	// to stays a 500. Save reports the two as one error, and the client can act
	// on only one of them.
	err = updated.Validate()
	if err != nil {
		// The language is the one rule whose refusal is answered under a code
		// of its own, because a screen that says no to it has to list what it
		// would say yes to. The rest arrive as a sentence in {reason}: there is
		// nothing for a screen to do with a port number but repeat it.
		if errors.Is(err, settings.ErrLanguageUnsupported) {
			return failure(c, http.StatusBadRequest, errSettingsLanguageUnsupported, errorArgs{
				"language":  updated.UIDefaultLanguage,
				"languages": strings.Join(settings.Languages(), ", "),
			})
		}

		return failure(c, http.StatusBadRequest, errSettingsRefused, errorArgs{"reason": err.Error()})
	}

	err = settings.Save(h.db, &updated)
	if err != nil {
		h.logger.Error("failed to store the settings", logid.SettingsStoreFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsStoreFailed)
	}

	changes := h.applyChanges(&before, &updated)

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
