package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// checkResult is what the last look at the releases found, kept so that the
// screen draws what the timer found rather than making a request of its own
// every time it is opened.
//
// It is held in memory and not in the database. What it says is true of a
// moment that has passed, a restart is the one thing that makes it certainly
// stale, and the loop looks again an interval after the last look: a row would
// be a stored answer about the outside world that nothing keeps true. Only the
// time of the last look outlives a restart, in a file of its own that main
// keeps for the loop to measure the interval from, and it is not drawn here.
type checkResult struct {
	At         time.Time
	Tag        string
	Newer      bool
	Comparable bool
	Problem    string
}

// UpdateHandler serves the Update tab of Settings.
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
	// installing is up from the moment an install is handed to start until
	// InstallEnded is called, or until start fails. A press and the loop that
	// installs on its own both go through it, so that one install this process
	// started is out at a time: two would each stop the service and copy over
	// the other, and on systemd the second collided with the unit of the first
	// and failed while the press answered that it had started.
	//
	// Nothing in this process sees an install that went well end, because
	// what it ends with is a restart that ends this process too, and the flag
	// goes with it. What does have to bring it down is an install that ended
	// without that restart, refused or failed, or every press after it would
	// be refused until the service was restarted by hand. The caller of start
	// reports that through InstallEnded, since it holds the process the
	// install runs in and this does not.
	installing bool
	// installProblem is why the last install ended without going through, as
	// InstallEnded or InstallEndedBefore was told it. It stays until the next
	// install is begun, so that the screen still says it when it is opened
	// after the press.
	installProblem string
	// begun is up once an install has been begun from this process, and it
	// stays up. From then on the last install is one this process knows the
	// end of, and a reason read off the disk about one before it is not the
	// last one's.
	begun bool
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
	// InstallProblem is why the last install started from the Update screen or
	// the loop ended without going through, left out where none has, where it
	// went through, or where another has been started since. It is the one this
	// process started, or else the one a process before it started, as that
	// install left it in the data directory: one that failed after it had
	// stopped the service has only the process it started again to answer it.
	// An install that went through ends with a restart and leaves no reason,
	// so the process that answers after it has none to give.
	InstallProblem string `json:"install_problem,omitempty"`
	// DevBuild says this process is the development build, which installs a
	// release on its checksum without checking the signature over it
	// (install.DevBuild). The version is the same string either way, so this is
	// the one place the screen can learn it from.
	DevBuild bool `json:"dev_build"`
}

func (h *UpdateHandler) view() updateView {
	h.mu.Lock()
	defer h.mu.Unlock()

	view := updateView{Version: h.version, Installable: h.installable, InstallProblem: h.installProblem,
		DevBuild: install.DevBuild}

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
	if !h.beginInstall() {
		return fmt.Errorf("an install started from this process has not ended yet: %w", install.ErrInstallRunning)
	}

	return h.handOver()
}

// handOver runs start with the flag already raised, and brings the flag down
// again wherever start did not hand an install over: an error, or a panic,
// which goes on up once the flag is down. A start that panicked started
// nothing that could report its end, so the flag would otherwise stay up and
// refuse every install after it until the service restarted.
//
// No reason is kept for a start that failed. The error is the answer to the
// press, or the log line of the loop, and an install that never ran has no end
// to report.
func (h *UpdateHandler) handOver() error {
	handed := false

	defer func() {
		if !handed {
			h.InstallEnded("")
		}
	}()

	err := h.start()
	handed = err == nil

	return err
}

// beginInstall raises the flag that one install is out and says whether it was
// down, which is whether this caller may start one. The reason the last one
// ended with goes with it, since it is about an install that is no longer the
// last.
func (h *UpdateHandler) beginInstall() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.installing {
		return false
	}

	h.installing = true
	h.begun = true
	h.installProblem = ""

	return true
}

// InstallEnded says that the install handed to start has ended without ending
// this process, so another one may be started. main calls it once the install
// it started is over; see the installing field for why nothing else can.
//
// problem is why the install did not go through, as the install left it, and
// empty where it left nothing. GET /api/update answers it as install_problem.
func (h *UpdateHandler) InstallEnded(problem string) {
	h.mu.Lock()
	h.installing = false
	h.installProblem = problem
	h.mu.Unlock()
}

// InstallEndedBefore says why an install that ended before this process
// started did not go through, as the install left it. main calls it at startup
// with the reason it finds in the data directory.
//
// An install that fails after it has stopped the service starts the service
// again on its way out, and the process that started the install is gone by
// then: it was stopped. The process that comes up in its place is the only one
// left to say why, and the reason is otherwise a file nobody reads.
//
// It may be told after this process has served for a while, since the install
// lets its lock go only once it has written the reason, and the process it
// started may be up before that. An install begun from here in between is the
// last one from then on, so the reason is not taken once one has been.
func (h *UpdateHandler) InstallEndedBefore(problem string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.begun {
		return
	}

	h.installProblem = problem
}

// GetUpdate answers with what is known, without looking again.
//
// @Summary      What the last look for a newer release found
// @Description  The version running, the newest release, whether it is newer, whether an install can be started from here, and why the last install started from here did not go through.
// @Tags         update
// @Produce  json
// @Success  200  {object}  models.Response{data=api.updateView}
// @Router       /update [get]
func (h *UpdateHandler) GetUpdate(c *echo.Context) error {
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
func (h *UpdateHandler) CheckUpdate(c *echo.Context) error {
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
// @Failure  409  {object}  api.errorBody  "There is nothing newer, this build cannot install one, or an install is already running"
// @Router       /update/install [post]
func (h *UpdateHandler) InstallUpdate(c *echo.Context) error {
	var req updateInstallRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
	}

	if !h.installable {
		return failure(c, http.StatusConflict, errUpdateNotInstallable)
	}

	// The install takes whatever the newest release is, whatever this process
	// is running. A press where the last look found nothing newer would put the
	// same release in place again, which takes every tunnel down for nothing,
	// or put an older one over a build that is ahead of what was released. So
	// a press asks what the loop asks before it installs on its own, and what
	// the screen asks before it offers the press.
	if !h.NewerAvailable() {
		return failure(c, http.StatusConflict, errUpdateNothingNewer)
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

	// The flag is raised after the password, so that a press with the wrong
	// one is told about the password and not about an install it could not
	// have started anyway.
	if !h.beginInstall() {
		return failure(c, http.StatusConflict, errUpdateInstallRunning)
	}

	h.logger.Warn("an update was asked for on the Update tab of Settings. The newest release is being "+
		"installed and this service is restarted at the end of it, which takes every tunnel down",
		logid.UpdateInstallAsked.Field(), zap.String("running", h.version))

	err = h.handOver()
	if err != nil {
		h.logger.Error("failed to start the install",
			logid.UpdateInstallStartFailed.Field(), zap.Error(err))

		// An install this process did not start, such as one an operator typed
		// at a terminal, is found by start, and it is the same answer.
		if errors.Is(err, install.ErrInstallRunning) {
			return failure(c, http.StatusConflict, errUpdateInstallRunning)
		}

		return failure(c, http.StatusInternalServerError, errUpdateInstallFailed,
			errorArgs{"reason": err.Error()})
	}

	return c.JSON(http.StatusOK, models.Response{Success: true, Data: h.view()})
}
