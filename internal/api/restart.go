package api

import (
	"net/http"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// restartExitDelay is how long the process keeps serving after the answer to a
// restart is written. It is the same wait the uninstall takes and for the same
// reason: the answer is what the browser draws the screen that says the service
// is coming back, and a process that goes down while that answer is still on
// the wire leaves the operator with a page that cannot say what happened.
//
// Three seconds is far more than a body of this size needs on a link the UI is
// usable over at all, and it is short enough that it does not read as a press
// that did nothing.
const restartExitDelay = 3 * time.Second

// RestartHandler ends this process in an orderly way and, where the platform
// allows it, has it run this program again.
//
// What it is handed is a function of the startup and not the restart itself.
// The order the shutdown has to keep is the one at the bottom of main: the
// servers are drained, the port is released, the reconcile loop is stopped and
// the tunnels come down, and only then is the image replaced. A handler that
// ran the exec itself would do it with the port still held, and the image that
// replaced it would fail to bind the port it was serving on a moment earlier.
type RestartHandler struct {
	logger *zap.Logger
	// comesBack says whether this process can run this program again in place
	// of itself. It is carried into the answer, because it decides what the
	// operator has to do next: on a platform without exec the process ends and
	// starting it again is left to whatever supervises it.
	comesBack bool
	// restart begins the ordered shutdown that ends in the restart. It returns
	// at once; the shutdown runs on the goroutine main is waiting on.
	restart func()
	// restartAfter is the wait between the answer and the shutdown. It is a
	// field and not the constant itself so that a test does not have to sit
	// through it.
	restartAfter time.Duration
}

func NewRestartHandler(logger *zap.Logger, comesBack bool, restart func()) *RestartHandler {
	return &RestartHandler{
		logger:       logger,
		comesBack:    comesBack,
		restart:      restart,
		restartAfter: restartExitDelay,
	}
}

// restartView is what both calls answer with. The two fields are what the
// screen needs before the press as well as after it: how long it is before the
// service goes, and whether it comes back on its own.
type restartView struct {
	// ExitInSec is how long the process stays up after the answer. The screen
	// says it and waits it out, so the number comes from the side that decides
	// it.
	ExitInSec int `json:"exit_in_sec"`
	// ComesBack is whether this process runs this program again in place of
	// itself. It is false on a platform without exec, where the restart is an
	// ordered stop and nothing more.
	ComesBack bool `json:"comes_back"`
}

// GetRestart says what a restart would do, without doing it.
//
// The screen asks before it draws the button. What it has to say in the
// question it puts to the operator differs by the answer: a service that comes
// back on its own is a few seconds of interruption, and one that does not is a
// service that stays down unless something starts it again. Asked only after
// the press, that warning would arrive once it was too late to act on, and a
// screen that always warned would be warning most operators about something
// that does not happen to them.
func (h *RestartHandler) GetRestart(c echo.Context) error {
	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    h.view(),
	})
}

// Restart takes this service down in order and has it come back.
//
// The password of the account is not asked for. The uninstall asks because what
// it does cannot be taken back; this one ends in the service running again, and
// a question that is put to every action is one that stops being read.
func (h *RestartHandler) Restart(c echo.Context) error {
	h.logger.Warn("a restart was asked for on the Settings screen. The API stops answering, "+
		"every tunnel comes down and this program runs again",
		zap.Duration("in", h.restartAfter),
		zap.Bool("comes_back", h.comesBack))

	// The answer is written before the timer starts, so the wait is the time
	// the browser has to receive it in rather than time the writing ate into.
	err := c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    h.view(),
	})
	if err != nil {
		// The restart still happens. It was asked for by a session that is
		// allowed to ask, and nothing about this failure says the service
		// should stay as it is; what is lost is the screen that would have
		// said so.
		h.logger.Error("failed to write the answer of the restart", zap.Error(err))
	}

	time.AfterFunc(h.restartAfter, h.restart)

	return err
}

func (h *RestartHandler) view() restartView {
	return restartView{
		ExitInSec: int(h.restartAfter / time.Second),
		ComesBack: h.comesBack,
	}
}
