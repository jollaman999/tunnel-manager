package tunnel

import (
	"testing"

	"go.uber.org/zap"
)

// TestRequestReconnectIsServedOnce pins the set a pass reads: a Host asked
// for is handed to the next pass, the loop is woken for it, and the pass after
// that is handed nothing.
func TestRequestReconnectIsServedOnce(t *testing.T) {
	m, err := NewManager(newTunnelStubDB(t, nil, nil), zap.NewNop(), nil, 5)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	counts := m.RequestReconnect(4)
	if counts != (ReconnectCounts{}) {
		t.Errorf("a Host with nothing running reports %+v, want zero counts", counts)
	}

	select {
	case <-m.reconcileWake:
	default:
		t.Errorf("the loop was not woken")
	}

	taken := m.takeReconnects()
	if len(taken) != 1 || !taken[4] {
		t.Fatalf("the pass was handed %v, want Host 4", taken)
	}

	if again := m.takeReconnects(); len(again) != 0 {
		t.Errorf("the next pass was handed %v, want nothing", again)
	}
}
