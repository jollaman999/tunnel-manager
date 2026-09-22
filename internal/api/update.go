package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// checkResult is what the last look at the releases found, kept so that the
// screen draws what the timer found rather than making a request of its own
// every time it is opened.
//
// It is held in memory and not in the database. What it says is true of a
// moment that has passed, a restart is the one thing that makes it certainly
// stale, and the loop looks again as it starts: a row would be a stored answer
// about the outside world that nothing keeps true.
type checkResult struct {
	At         time.Time
	Tag        string
	Newer      bool
	Comparable bool
	Problem    string
}

// UpdateHandler serves the Update screen.
//
// It holds no policy of its own. Whether to look and whether to install are
// settings, and the loop in main is what reads them; this answers what is known
// and does what a press asks for.
type UpdateHandler struct {
	logger  *zap.Logger
	db      *gorm.DB
	version string
	// installable says whether an install can be started from here at all. It
	// is false where this process is not running as a registered service,
	// because what the install does then is register one, which is not what a
	// press on an update screen is asking for.
	installable bool
	// check reads the newest release. start runs the install. Both are handed
	// in so that a test drives them without reaching the network or replacing
	// anything.
	check func(ctx context.Context, version string) (install.Latest, error)
	start func() error

	mu   sync.Mutex
	last *checkResult
}

func NewUpdateHandler(logger *zap.Logger, db *gorm.DB, version string, installable bool,
	check func(context.Context, string) (install.Latest, error), start func() error) *UpdateHandler {
	return &UpdateHandler{
		logger:      logger,
		db:          db,
		version:     version,
		installable: installable,
		check:       check,
		start:       start,
	}
}

// updateView is what the screen draws.
type updateView struct {
	// Version is what this process is running, without the leading v, the way
	// the version constant carries it.
	Version string `json:"version"`
	// Checked is when the last look happened, and the three fields under it are
	// what it found. Checked is the zero time where nothing has looked yet,
	// which the screen draws as never rather than as 1970.
	Checked    time.Time `json:"checked_at"`
	Tag        string    `json:"tag"`
	Newer      bool      `json:"newer"`
	Comparable bool      `json:"comparable"`
	// Problem is why the last look found nothing, empty where it worked. A
	// check that failed is not a version that is up to date, and this is what
	// keeps the two apart on the screen.
	Problem string `json:"problem"`
	// Installable says whether the press that installs is offered. See the
	// field of the same name on the handler.
	Installable bool `json:"installable"`
}

func (h *UpdateHandler) view() updateView {
	h.mu.Lock()
	defer h.mu.Unlock()

	view := updateView{Version: h.version, Installable: h.installable}

	if h.last != nil {
		view.Checked = h.last.At
		view.Tag = h.last.Tag
		view.Newer = h.last.Newer
		view.Comparable = h.last.Comparable
		view.Problem = h.last.Problem
	}

	return view
}

// Look reads the newest release and keeps what it found.
//
// It is exported because the loop in main calls it on its timer. Both callers
// land here so that the screen shows what the timer found, and so that a press
// on the screen moves the timer's answer on rather than keeping a second one
// beside it.
func (h *UpdateHandler) Look(ctx context.Context) checkResult {
	latest, err := h.check(ctx, h.version)

	result := checkResult{At: time.Now()}

	if err != nil {
		result.Problem = err.Error()

		h.logger.Warn("the newest release could not be read",
			logid.UpdateCheckFailed.Field(), zap.Error(err))
	} else {
		result.Tag = latest.Tag
		result.Newer = latest.Newer
		result.Comparable = latest.Comparable

		h.logger.Info("read the newest release",
			logid.UpdateChecked.Field(),
			zap.String("running", h.version),
			zap.String("latest", latest.Tag),
			zap.Bool("newer", latest.Newer),
			zap.Bool("comparable", latest.Comparable))
	}

	h.mu.Lock()
	h.last = &result
	h.mu.Unlock()

	return result
}

// NewerAvailable reports whether the last look found a release that is both
// comparable with what is running and after it.
//
// It is the one condition an install may start on when nobody is asking for it,
// which is why it is here rather than written out at the loop: a check that
// failed and a version that could not be compared both have to answer false,
// and three conditions spelled out at the call site are three chances to drop
// one of them.
func (h *UpdateHandler) NewerAvailable() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.last == nil {
		return false
	}

	return h.last.Problem == "" && h.last.Comparable && h.last.Newer
}

// StartInstall runs the install without a request behind it.
//
// It is what the loop calls where the settings ask for an update to be
// installed on its own. The password the handler asks a person for is not asked
// here and there is nobody to ask: what stands in its place is the setting,
// which is off unless an operator turned it on.
func (h *UpdateHandler) StartInstall() error {
	return h.start()
}

// GetUpdate answers with what is known, without looking again.
//
// @Summary      What the last look for a newer release found
// @Description  The version running, the newest release, whether it is newer, and whether an install can be started from here.
// @Tags         update
// @Produce  json
// @Success  200  {object}  models.Response{data=api.updateView}
// @Router       /update [get]
func (h *UpdateHandler) GetUpdate(c echo.Context) error {
	return c.JSON(http.StatusOK, models.Response{Success: true, Data: h.view()})
}

// CheckUpdate looks now.
//
// @Summary      Read the newest release now
// @Description  Answers what GET /api/update would then answer. A POST because it makes a request to another host, which is not a thing a link or a prefetch may set off.
// @Tags         update
// @Produce  json
// @Security  CSRFToken
// @Success  200  {object}  models.Response{data=api.updateView}
// @Router       /update/check [post]
func (h *UpdateHandler) CheckUpdate(c echo.Context) error {
	result := h.Look(c.Request().Context())

	if result.Problem != "" {
		return failure(c, http.StatusBadGateway, errUpdateCheckFailed,
			errorArgs{"reason": result.Problem})
	}

	return c.JSON(http.StatusOK, models.Response{Success: true, Data: h.view()})
}

// updateInstallRequest is what the press sends. The password is the password of
// the account, the way the uninstall and the log emptying ask for one.
type updateInstallRequest struct {
	Password string `json:"password"`
}

// InstallUpdate starts the install and answers that it started.
//
// It cannot answer that it finished. What the install does at the end is
// restart the service, which ends this process, so the last thing this handler
// can say is that the work was handed over. The screen says that and then
// waits for the service to answer again.
//
// The password of the account is asked for because what this starts replaces
// the executable and takes every tunnel down with the restart.
//
// @Summary      Start the install of the newest release
// @Description  Takes the account password. It answers that the install started and never that it finished: what the install ends with is a restart of the service answering the request.
// @Tags         update
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.updateInstallRequest  true  "The account password"
// @Success  200  {object}  models.Response{data=api.updateView}
// @Failure  401  {object}  api.errorBody  "The password does not open this account"
// @Failure  409  {object}  api.errorBody  "There is nothing newer, or this build cannot install one"
// @Router       /update/install [post]
func (h *UpdateHandler) InstallUpdate(c echo.Context) error {
	var req updateInstallRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	if !h.installable {
		return failure(c, http.StatusConflict, errUpdateNotInstallable)
	}

	if req.Password == "" {
		return failure(c, http.StatusBadRequest, errUpdatePasswordMissing)
	}

	refused := accountPasswordRefused(c, h.db, h.logger, req.Password, errUpdatePasswordWrong)
	if refused != nil {
		if refused.code == errUpdatePasswordWrong {
			h.logger.Warn("an update was asked for with a password that does not open the account. "+
				"Nothing was installed",
				logid.UpdateInstallPasswordWrong.Field())
		}

		return refused.answer(c)
	}

	h.logger.Warn("an update was asked for on the Update screen. The newest release is being "+
		"installed and this service is restarted at the end of it, which takes every tunnel down",
		logid.UpdateInstallAsked.Field(), zap.String("running", h.version))

	err = h.start()
	if err != nil {
		h.logger.Error("failed to start the install",
			logid.UpdateInstallStartFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errUpdateInstallFailed,
			errorArgs{"reason": err.Error()})
	}

	return c.JSON(http.StatusOK, models.Response{Success: true, Data: h.view()})
}
