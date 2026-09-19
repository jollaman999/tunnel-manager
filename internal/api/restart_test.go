package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// restartWait is how long a test waits for the shutdown that is scheduled after
// the answer. It is far longer than the delay the handler is given below, so a
// machine under load does not turn a passing test into a failing one.
const restartWait = 5 * time.Second

// newRestartHandler returns the handler together with the channel its shutdown
// closes, so a test can see whether the restart was begun and when.
func newRestartHandler(t *testing.T, comesBack bool) (*RestartHandler, chan struct{}) {
	t.Helper()

	begun := make(chan struct{})

	h := NewRestartHandler(zap.NewNop(), comesBack, func() {
		close(begun)
	})

	// The wait between the answer and the shutdown is the time the browser has
	// to draw the screen that says the service is coming back. A test has
	// nothing to draw, so it is cut to something a test can sit through while
	// still being long enough to tell a shutdown that waited from one that ran
	// inside the call.
	h.restartAfter = 50 * time.Millisecond

	return h, begun
}

// callRestart runs one call against the handler.
func callRestart(t *testing.T, h *RestartHandler, method string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(method, "/api/restart", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	var err error

	if method == http.MethodGet {
		err = h.GetRestart(c)
	} else {
		err = h.Restart(c)
	}

	if err != nil {
		t.Fatalf("the handler returned error: %v", err)
	}

	return rec
}

// decodeRestart reads the answer of a restart call.
func decodeRestart(t *testing.T, rec *httptest.ResponseRecorder) restartView {
	t.Helper()

	var resp struct {
		Success bool        `json:"success"`
		Data    restartView `json:"data"`
		Error   string      `json:"error"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer %q: %v", rec.Body.String(), err)
	}

	if !resp.Success {
		t.Fatalf("the call was refused: %q", resp.Error)
	}

	return resp.Data
}

// TestRestartAnswersBeforeTheServiceGoesDown is what the screen hangs on. The
// answer is what it draws the waiting screen from, and an answer written after
// the API stopped serving is one that never arrives.
func TestRestartAnswersBeforeTheServiceGoesDown(t *testing.T) {
	h, begun := newRestartHandler(t, true)

	rec := callRestart(t, h, http.MethodPost)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/restart = %d, want %d", rec.Code, http.StatusOK)
	}

	select {
	case <-begun:
		t.Fatal("the shutdown was begun inside the call, so the answer was written into a " +
			"service that was already going down")
	default:
	}

	select {
	case <-begun:
	case <-time.After(restartWait):
		t.Fatalf("the shutdown was not begun within %s of the answer", restartWait)
	}
}

// TestRestartSaysWhetherTheServiceComesBack covers both builds. The screen puts
// a different question to the operator for each, and it has to be told which
// one this is rather than guessing from the platform the browser runs on.
func TestRestartSaysWhetherTheServiceComesBack(t *testing.T) {
	for _, comesBack := range []bool{true, false} {
		h, begun := newRestartHandler(t, comesBack)

		view := decodeRestart(t, callRestart(t, h, http.MethodPost))

		if view.ComesBack != comesBack {
			t.Fatalf("a handler built with comes_back=%t answered %t", comesBack, view.ComesBack)
		}

		if view.ExitInSec != int(h.restartAfter/time.Second) {
			t.Fatalf("the answer says the service goes in %d seconds, want %d",
				view.ExitInSec, int(h.restartAfter/time.Second))
		}

		// A platform that does not come back is still restarted. Whether
		// something supervises this service cannot be found out from here, and
		// a refusal would take the restart away from every deployment that has
		// one.
		select {
		case <-begun:
		case <-time.After(restartWait):
			t.Fatalf("the shutdown was not begun for a handler with comes_back=%t", comesBack)
		}
	}
}

// TestGetRestartTakesNothingDown is the call the screen makes while it draws.
// It is made on every visit to the Settings screen, so a read that restarted
// anything would restart the service on arrival.
func TestGetRestartTakesNothingDown(t *testing.T) {
	h, begun := newRestartHandler(t, true)

	rec := callRestart(t, h, http.MethodGet)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/restart = %d, want %d", rec.Code, http.StatusOK)
	}

	view := decodeRestart(t, rec)
	if !view.ComesBack {
		t.Fatalf("GET /api/restart answered comes_back=%t for a handler built with true",
			view.ComesBack)
	}

	select {
	case <-begun:
		t.Fatal("reading what a restart would do began one")
	case <-time.After(4 * h.restartAfter):
	}
}

// TestRestartWaitsTheDelayTheAnswerNames keeps the two in step. The screen
// starts waiting from the number in the answer, so a handler that went down
// sooner than it said would be gone while the screen was still counting.
func TestRestartWaitsTheDelayTheAnswerNames(t *testing.T) {
	h := NewRestartHandler(zap.NewNop(), true, func() {})

	if h.restartAfter != restartExitDelay {
		t.Fatalf("the handler waits %s, want %s", h.restartAfter, restartExitDelay)
	}

	if h.view().ExitInSec != int(restartExitDelay/time.Second) {
		t.Fatalf("the answer says %d seconds while the handler waits %s",
			h.view().ExitInSec, restartExitDelay)
	}
}
