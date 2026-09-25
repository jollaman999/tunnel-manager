package alert

import (
	"errors"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeForwards is what the watcher under test is told is running.
type fakeForwards struct {
	conditions []Condition
	err        error
}

func (f *fakeForwards) read() ([]Condition, error) {
	return f.conditions, f.err
}

func (f *fakeForwards) set(conditions ...Condition) {
	f.conditions = conditions
}

// webhookOn is a set of settings with somewhere to send to and a delay of five
// minutes.
func webhookOn() Config {
	return Config{After: 5 * time.Minute, WebhookURL: "https://hooks.example.com/alert"}
}

func newTestWatcher(t *testing.T, forwards *fakeForwards, config *Config) *Watcher {
	t.Helper()

	return NewWatcher(zap.NewNop(), forwards.read, func() (Config, error) {
		return *config, nil
	}, nil, "tm.example.com")
}

// drained takes what the scans queued, without sending it.
func drained(w *Watcher) []Event {
	var events []Event

	for {
		select {
		case item := <-w.queue:
			events = append(events, item.event)
		default:
			return events
		}
	}
}

// tracked returns the keys of the forwards being tracked, in order.
func tracked(w *Watcher) []string {
	keys := make([]string, 0, len(w.outages))
	for key := range w.outages {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func down(key string) Condition {
	return Condition{Key: key, Kind: KindServicePort, Host: "ssh.example.com", LocalPort: 8080,
		LastError: "dial tcp: connection refused"}
}

func up(key string) Condition {
	return Condition{Key: key, Kind: KindServicePort, Host: "ssh.example.com", LocalPort: 8080, Connected: true}
}

// TestADownIsSentOnceTheDelayHasPassed is the rule itself: nothing while the
// outage is shorter than the delay, one down when it reaches it, and nothing
// more for as long as the same outage lasts.
func TestADownIsSentOnceTheDelayHasPassed(t *testing.T) {
	forwards := &fakeForwards{}
	config := webhookOn()
	w := newTestWatcher(t, forwards, &config)

	start := time.Date(2026, time.September, 26, 1, 0, 0, 0, time.UTC)

	forwards.set(down("a"))
	w.Scan(start)
	w.Scan(start.Add(299 * time.Second))

	if events := drained(w); len(events) != 0 {
		t.Fatalf("an outage shorter than the delay sent %v", events)
	}

	w.Scan(start.Add(300 * time.Second))

	events := drained(w)
	if len(events) != 1 {
		t.Fatalf("an outage that reached the delay sent %d alerts, want 1: %v", len(events), events)
	}

	got := events[0]
	want := Event{
		Event:        EventDown,
		Kind:         KindServicePort,
		Host:         "ssh.example.com",
		LocalPort:    8080,
		Since:        "2026-09-26T01:00:00Z",
		LastError:    "dial tcp: connection refused",
		Installation: "tm.example.com",
	}

	if got != want {
		t.Fatalf("the down is %+v, want %+v", got, want)
	}

	for i := 1; i <= 10; i++ {
		w.Scan(start.Add(time.Duration(300+i*5) * time.Second))
	}

	if events := drained(w); len(events) != 0 {
		t.Fatalf("the same outage sent more alerts: %v", events)
	}
}

// TestAnUpIsSentOnceAfterADown covers the way back: one up with the time of the
// down it answers, and nothing for the scans after it.
func TestAnUpIsSentOnceAfterADown(t *testing.T) {
	forwards := &fakeForwards{}
	config := webhookOn()
	w := newTestWatcher(t, forwards, &config)

	start := time.Date(2026, time.September, 26, 1, 0, 0, 0, time.UTC)

	forwards.set(down("a"))
	w.Scan(start)
	w.Scan(start.Add(10 * time.Minute))
	drained(w)

	forwards.set(up("a"))
	w.Scan(start.Add(11 * time.Minute))
	w.Scan(start.Add(12 * time.Minute))

	events := drained(w)
	if len(events) != 1 || events[0].Event != EventUp {
		t.Fatalf("a recovery sent %v, want one up", events)
	}

	if events[0].Since != "2026-09-26T01:00:00Z" {
		t.Fatalf("the up carries since %q, want the start of the outage", events[0].Since)
	}

	if events[0].LastError != "dial tcp: connection refused" {
		t.Fatalf("the up carries last_error %q, want the error of the outage", events[0].LastError)
	}

	if keys := tracked(w); len(keys) != 0 {
		t.Fatalf("a forward that came back is still tracked: %v", keys)
	}

	// A second outage is an outage of its own, with a down of its own.
	forwards.set(down("a"))
	w.Scan(start.Add(20 * time.Minute))
	w.Scan(start.Add(25 * time.Minute))

	events = drained(w)
	if len(events) != 1 || events[0].Event != EventDown || events[0].Since != "2026-09-26T01:20:00Z" {
		t.Fatalf("a second outage sent %v, want one down from 01:20", events)
	}
}

// TestAShortOutageSendsNothing is a forward that comes back before the delay:
// no down was sent, so no up is either.
func TestAShortOutageSendsNothing(t *testing.T) {
	forwards := &fakeForwards{}
	config := webhookOn()
	w := newTestWatcher(t, forwards, &config)

	start := time.Now()

	forwards.set(down("a"))
	w.Scan(start)
	w.Scan(start.Add(time.Minute))

	forwards.set(up("a"))
	w.Scan(start.Add(2 * time.Minute))

	if events := drained(w); len(events) != 0 {
		t.Fatalf("an outage shorter than the delay sent %v", events)
	}
}

// TestAForwardThatStopsRunningIsNotReported covers what is switched off,
// removed or on a Host that was disabled. It stops being among what runs, which
// is not an outage: nothing is sent for it, and a forward that was reported down
// and then removed gets no up.
func TestAForwardThatStopsRunningIsNotReported(t *testing.T) {
	forwards := &fakeForwards{}
	config := webhookOn()
	w := newTestWatcher(t, forwards, &config)

	start := time.Now()

	// "a" is down long enough to be reported and is then removed. "b" is
	// switched off before its delay passes.
	forwards.set(down("a"), down("b"))
	w.Scan(start)

	forwards.set(down("a"))
	w.Scan(start.Add(time.Minute))
	w.Scan(start.Add(2 * time.Minute))
	w.Scan(start.Add(6 * time.Minute))

	events := drained(w)
	if len(events) != 1 || events[0].Event != EventDown {
		t.Fatalf("got %v, want the one down of the forward that stayed", events)
	}

	forwards.set()
	w.Scan(start.Add(7 * time.Minute))
	w.Scan(start.Add(8 * time.Minute))

	if keys := tracked(w); len(keys) != 0 {
		t.Fatalf("forwards that stopped running are still tracked: %v", keys)
	}

	// The key comes back as a forward that is connected, which is a new one
	// rather than the one that was reported.
	forwards.set(up("a"))
	w.Scan(start.Add(9 * time.Minute))

	if events := drained(w); len(events) != 0 {
		t.Fatalf("a removed forward sent %v", events)
	}
}

// TestARebuildIsNotTakenForARemoval is a forward the reconcile pass stops and
// starts again between two scans. It is missing from one scan only, and the up
// of the down that was sent for it still goes out.
func TestARebuildIsNotTakenForARemoval(t *testing.T) {
	forwards := &fakeForwards{}
	config := webhookOn()
	w := newTestWatcher(t, forwards, &config)

	start := time.Now()

	forwards.set(down("a"))
	w.Scan(start)
	w.Scan(start.Add(6 * time.Minute))
	drained(w)

	forwards.set()
	w.Scan(start.Add(7 * time.Minute))

	forwards.set(up("a"))
	w.Scan(start.Add(8 * time.Minute))

	events := drained(w)
	if len(events) != 1 || events[0].Event != EventUp {
		t.Fatalf("a forward rebuilt between two scans sent %v, want one up", events)
	}
}

// TestNothingIsTrackedWhileAlertsAreOff is the installation that has named
// nowhere to send to. An outage that began then is counted from the scan that
// finds alerts on, so turning them on does not set off an alert at once.
func TestNothingIsTrackedWhileAlertsAreOff(t *testing.T) {
	forwards := &fakeForwards{}
	config := Config{After: 5 * time.Minute}
	w := newTestWatcher(t, forwards, &config)

	start := time.Now()

	forwards.set(down("a"))
	w.Scan(start)
	w.Scan(start.Add(time.Hour))

	if keys := tracked(w); len(keys) != 0 {
		t.Fatalf("forwards are tracked while alerts are off: %v", keys)
	}

	config = webhookOn()
	w.Scan(start.Add(time.Hour + time.Minute))

	if events := drained(w); len(events) != 0 {
		t.Fatalf("turning alerts on sent %v at once", events)
	}

	w.Scan(start.Add(time.Hour + 6*time.Minute))

	if events := drained(w); len(events) != 1 {
		t.Fatalf("got %v, want one down five minutes after alerts were turned on", events)
	}
}

// TestAScanThatCannotReadChangesNothing keeps a failed read from being taken
// for every forward having stopped.
func TestAScanThatCannotReadChangesNothing(t *testing.T) {
	forwards := &fakeForwards{}
	config := webhookOn()
	w := newTestWatcher(t, forwards, &config)

	start := time.Now()

	forwards.set(down("a"))
	w.Scan(start)
	w.Scan(start.Add(6 * time.Minute))
	drained(w)

	forwards.err = errors.New("database is locked")
	w.Scan(start.Add(7 * time.Minute))
	w.Scan(start.Add(8 * time.Minute))
	w.Scan(start.Add(9 * time.Minute))

	forwards.err = nil
	forwards.set(up("a"))
	w.Scan(start.Add(10 * time.Minute))

	events := drained(w)
	if len(events) != 1 || events[0].Event != EventUp {
		t.Fatalf("got %v, want the up of the forward that was reported down", events)
	}
}

// TestSMTPAloneTurnsAlertsOn holds that the two ways are switched on apart.
func TestSMTPAloneTurnsAlertsOn(t *testing.T) {
	if (Config{}).Enabled() {
		t.Fatal("a set with nowhere to send to reads as on")
	}

	if !(Config{SMTP: SMTPConfig{Host: "mail.example.com"}}).Enabled() {
		t.Fatal("a set with only a mail server reads as off")
	}

	if !(Config{WebhookURL: "https://hooks.example.com/"}).Enabled() {
		t.Fatal("a set with only a webhook reads as off")
	}
}
