// Package alert tells somebody when a tunnel, a local forward or a SOCKS5 proxy
// has been without a connection for longer than the stored alert delay, and
// again when it comes back.
//
// What it watches is read on a timer rather than hooked into every place a
// connection changes state. There are three kinds of forward with four places
// each that set a status, and a hook in each of them would be twelve places
// that have to remember to call it, while a scan reads the one answer the
// Status screen already reads. What the timer costs is that an outage is seen
// up to one scan late, which is nothing next to a delay counted in minutes,
// and that a connection which drops and returns between two scans is never
// seen at all, which is a connection that did not stay down.
//
// What has been reported is kept in memory. A restart forgets it: a forward
// that was reported down and is still down when the process comes back is
// reported again once the delay has passed from the start, and one that came
// back while the process was away is never reported as up. The process that
// restarts has just rebuilt every connection, so what it knew about the old
// ones is about connections that are gone.
package alert

import (
	"context"
	"os"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"go.uber.org/zap"
)

// The kinds of forward a condition is about.
const (
	KindServicePort  = "service_port"
	KindLocalForward = "local_forward"
	KindSocks        = "socks"
)

// The events an alert reports. EventTest is what the two test presses on the
// Settings screen send, so that whoever receives it can tell it from a real
// outage.
const (
	EventDown = "down"
	EventUp   = "up"
	EventTest = "test"
)

// ScanInterval is how often the forwards are looked at. It is the monitoring
// interval a fresh installation runs on, which is how often a status can change
// at all.
const ScanInterval = 5 * time.Second

// forgetAfterMissing is how many scans in a row a forward has to be missing
// from before what is known about it is dropped. A forward whose settings
// changed is stopped and started again by the reconcile pass, and a scan that
// lands between the two would otherwise take the rebuild for a removal and
// never report the up of a forward that was reported down.
const forgetAfterMissing = 2

// queueSize is how many alerts may wait to be sent. Sending is one at a time
// so that an up never overtakes the down it answers, and a mail server that
// takes its time holds up the ones behind it; past this many the newest are
// dropped and logged rather than holding the scan up as well.
const queueSize = 64

// Condition is one running forward as a scan sees it.
type Condition struct {
	// Key names the forward across scans. It is unique over all three kinds.
	Key  string
	Kind string
	// Host is the address of the Host the forward goes through.
	Host string
	// LocalPort is the port the forward opens: on the Host for a service
	// port, on this system for a local forward and a SOCKS5 proxy.
	LocalPort int
	Connected bool
	LastError string
}

// Event is one alert, and it is also the body the webhook is posted.
type Event struct {
	Event     string `json:"event"`
	Kind      string `json:"kind"`
	Host      string `json:"host"`
	LocalPort int    `json:"local_port"`
	// Since is when the forward was first seen without a connection, written
	// as RFC 3339 in UTC. An up carries the same time as the down it answers,
	// so the two are matched by it and the length of the outage is the
	// difference to when the up arrived.
	Since     string `json:"since"`
	LastError string `json:"last_error"`
	// Installation is the host name of the system this program runs on, so
	// that alerts from several installations sent to one place can be told
	// apart.
	Installation string `json:"installation"`
}

// outage is what is known about one forward that is not connected.
type outage struct {
	condition Condition
	since     time.Time
	lastError string
	downSent  bool
	missing   int
}

// Watcher scans the forwards and queues an alert for each down and up.
type Watcher struct {
	logger       *zap.Logger
	conditions   func() ([]Condition, error)
	config       func() (Config, error)
	sender       *Sender
	installation string

	outages map[string]*outage
	queue   chan queued
}

// queued is an alert waiting to be sent, with the settings it is sent under.
type queued struct {
	config Config
	event  Event
}

// NewWatcher builds a watcher. conditions is what is running now, and config is
// the stored alert settings; both are asked on every scan, so a change on the
// Settings screen holds from the next scan on without a restart.
func NewWatcher(logger *zap.Logger, conditions func() ([]Condition, error), config func() (Config, error),
	sender *Sender, installation string) *Watcher {
	return &Watcher{
		logger:       logger,
		conditions:   conditions,
		config:       config,
		sender:       sender,
		installation: installation,
		outages:      make(map[string]*outage),
		queue:        make(chan queued, queueSize),
	}
}

// Run scans every interval and sends what the scans queue, until ctx ends.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	go w.deliver(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.Scan(now)
		}
	}
}

// deliver sends the queued alerts one at a time.
func (w *Watcher) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-w.queue:
			w.sender.Send(ctx, item.config, item.event)
		}
	}
}

// Scan looks at every running forward once. It is exported so that a test can
// drive it with a clock of its own; Run is what calls it otherwise, and only
// from the one goroutine.
func (w *Watcher) Scan(now time.Time) {
	config, err := w.config()
	if err != nil {
		w.logger.Warn("failed to read the alert settings, so no forward was looked at",
			logid.AlertSettingsReadFailed.Field(),
			zap.Error(err))

		return
	}

	// With nowhere to send to, nothing is tracked either. An outage that began
	// while alerts were off is counted from the scan that finds them on, so
	// turning them on does not set off a burst of alerts for things that were
	// down before anybody asked to hear about them.
	if !config.Enabled() {
		if len(w.outages) > 0 {
			w.outages = make(map[string]*outage)
		}

		return
	}

	conditions, err := w.conditions()
	if err != nil {
		w.logger.Warn("failed to read what the forwards report, so none was looked at",
			logid.AlertConditionsReadFailed.Field(),
			zap.Error(err))

		return
	}

	seen := make(map[string]bool, len(conditions))

	for _, condition := range conditions {
		seen[condition.Key] = true

		known := w.outages[condition.Key]

		if condition.Connected {
			if known != nil {
				if known.downSent {
					w.queueEvent(config, EventUp, condition, known)
				}

				delete(w.outages, condition.Key)
			}

			continue
		}

		if known == nil {
			known = &outage{since: now}
			w.outages[condition.Key] = known
		}

		known.condition = condition
		known.missing = 0

		// The last error that was seen is kept across a scan that reports
		// none, which is what a forward that is starting again after a
		// failure reports.
		if condition.LastError != "" {
			known.lastError = condition.LastError
		}

		if !known.downSent && now.Sub(known.since) >= config.After {
			known.downSent = true
			w.queueEvent(config, EventDown, condition, known)
		}
	}

	// A forward that is no longer running was stopped: switched off,
	// removed, or on a Host that was disabled. None of those is an outage, so
	// it is forgotten without an up, even where a down was sent.
	for key, known := range w.outages {
		if seen[key] {
			continue
		}

		known.missing++
		if known.missing >= forgetAfterMissing {
			delete(w.outages, key)
		}
	}
}

// queueEvent logs one alert and queues it to be sent.
func (w *Watcher) queueEvent(config Config, kind string, condition Condition, known *outage) {
	event := Event{
		Event:        kind,
		Kind:         condition.Kind,
		Host:         condition.Host,
		LocalPort:    condition.LocalPort,
		Since:        known.since.UTC().Format(time.RFC3339),
		LastError:    known.lastError,
		Installation: w.installation,
	}

	if kind == EventUp {
		w.logger.Info("a forward that was reported down is connected again, sending an alert",
			logid.AlertUp.Field(),
			zap.String("kind", event.Kind),
			zap.String("host", event.Host),
			zap.Int("local_port", event.LocalPort),
			zap.String("since", event.Since))
	} else {
		w.logger.Warn("a forward has stayed down past the alert delay, sending an alert",
			logid.AlertDown.Field(),
			zap.String("kind", event.Kind),
			zap.String("host", event.Host),
			zap.Int("local_port", event.LocalPort),
			zap.String("since", event.Since))
	}

	select {
	case w.queue <- queued{config: config, event: event}:
	default:
		w.logger.Warn("too many alerts are waiting to be sent, so this one was dropped",
			logid.AlertDropped.Field(),
			zap.String("event", event.Event),
			zap.String("kind", event.Kind),
			zap.String("host", event.Host),
			zap.Int("local_port", event.LocalPort))
	}
}

// InstallationName is the host name of this system, which every alert carries
// so that alerts from several installations sent to one place can be told
// apart. A system that will not say is named by nothing rather than by a guess.
func InstallationName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}

	return name
}
