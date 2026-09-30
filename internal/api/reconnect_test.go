package api

import (
	"net/http"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"go.uber.org/zap"
)

// TestReconnectHostAsksTheManagerAndAnswersItsCounts pins that the request is
// handed to the manager for the Host named, that the loop is woken, and that
// the answer is what the manager said was running.
func TestReconnectHostAsksTheManagerAndAnswersItsCounts(t *testing.T) {
	db := newRowsDB(t, []models.Host{statusHost(1, true), statusHost(2, true)}, nil, nil)
	manager := &wakeRecorder{tx: &txConnPool{},
		reconnectCounts: tunnel.ReconnectCounts{Tunnels: 3, LocalForwards: 1, Socks: 1}}

	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	c, rec := hostServicePortRequest(t, http.MethodPost, "/api/host/2/reconnect", "2", "")

	err := h.ReconnectHost(c)
	if err != nil {
		t.Fatalf("ReconnectHost returned an error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	_, data := decodeResponse(t, rec)
	if data["tunnels"] != float64(3) || data["local_forwards"] != float64(1) || data["socks"] != float64(1) {
		t.Errorf("the answer is %v, want the counts the manager reported", data)
	}

	if len(manager.reconnected) != 1 || manager.reconnected[0] != 2 {
		t.Errorf("the manager was asked to reconnect %v, want Host 2 alone", manager.reconnected)
	}

	if wakes, _ := manager.counts(); wakes != 1 {
		t.Errorf("the loop was woken %d times, want once", wakes)
	}
}

// TestReconnectHostAnswersAHostThatIsNotThere pins the 404, and that nothing
// is asked of the manager for it.
func TestReconnectHostAnswersAHostThatIsNotThere(t *testing.T) {
	db := newRowsDB(t, []models.Host{statusHost(1, true)}, nil, nil)
	manager := &wakeRecorder{tx: &txConnPool{}}

	h := NewHandler(db, manager, zap.NewNop(), newTestCipher(t))

	c, rec := hostServicePortRequest(t, http.MethodPost, "/api/host/9/reconnect", "9", "")

	err := h.ReconnectHost(c)
	if err != nil {
		t.Fatalf("ReconnectHost returned an error: %v", err)
	}

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	if len(manager.reconnected) != 0 {
		t.Errorf("the manager was asked to reconnect %v for a Host that is not there", manager.reconnected)
	}
}
