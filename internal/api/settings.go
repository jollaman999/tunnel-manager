package api

import (
	"net/http"

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
}

func NewSettingsHandler(db *gorm.DB, logger *zap.Logger, logLevel zap.AtomicLevel,
	gormLevel databaseLogLevel) *SettingsHandler {
	return &SettingsHandler{
		db:        db,
		logger:    logger,
		logLevel:  logLevel,
		gormLevel: gormLevel,
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

// GetSettings answers with what is stored.
func (h *SettingsHandler) GetSettings(c echo.Context) error {
	stored, err := settings.Load(h.db)
	if err != nil {
		h.logger.Error("failed to read the settings", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to read the settings",
		})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    stored,
	})
}

// UpdateSettings stores what the request carries and puts into place whatever
// the running process can take on.
func (h *SettingsHandler) UpdateSettings(c echo.Context) error {
	stored, err := settings.Load(h.db)
	if err != nil {
		h.logger.Error("failed to read the settings", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to read the settings",
		})
	}

	before := *stored

	// The body is bound onto what is stored, so a request that names some of
	// the settings changes those and leaves the rest alone. Bound onto an empty
	// set, every setting the body left out would arrive as a zero value and be
	// stored as one or refused as one.
	updated := *stored

	err = c.Bind(&updated)
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "Invalid request body: " + err.Error(),
		})
	}

	// The rules are run here as well as inside Save so that a value they refuse
	// is answered as a bad request while a database that could not be written
	// to stays a 500. Save reports the two as one error, and the client can act
	// on only one of them.
	err = updated.Validate()
	if err != nil {
		return c.JSON(http.StatusBadRequest, models.Response{
			Success: false,
			Error:   "The settings are refused: " + err.Error(),
		})
	}

	err = settings.Save(h.db, &updated)
	if err != nil {
		h.logger.Error("failed to store the settings", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, models.Response{
			Success: false,
			Error:   "Failed to store the settings",
		})
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

// applyChanges puts into place what this process can take on and reports every
// change with what became of it.
//
// logging.level is the whole of that list. The loggers were built around the
// handle above at startup, so setting it reaches every logger that was handed
// out, including the one gorm writes through. The periods of the monitoring and
// the reconcile loops are read once by the loops that run on them and the port
// is read once by the server that listens on it, so those are stored and wait
// for a restart.
//
// Which setting belongs in that list is decided here, next to the code that
// puts the value in place, and is carried to the screen in the answer. A screen
// that held the list of its own would go on saying a setting took hold after
// this stopped putting it into place.
func (h *SettingsHandler) applyChanges(before *settings.Settings, after *settings.Settings) []settingsChange {
	applied := map[string]bool{}

	if before.LoggingLevel != after.LoggingLevel && h.setLogLevel(after.LoggingLevel) {
		applied["logging.level"] = true
	}

	diff := settings.Diff(before, after)

	// The list is built even when it is empty, so that a save that changed
	// nothing answers with an empty list rather than with a null the screen
	// would have to tell from a list it failed to read.
	changes := make([]settingsChange, 0, len(diff))

	for _, change := range diff {
		state := appliedRestart
		if applied[change.Name] {
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
			zap.String("level", level),
			zap.Error(err))

		return false
	}

	h.logLevel.SetLevel(parsed)
	h.gormLevel.Set(level)

	return true
}
