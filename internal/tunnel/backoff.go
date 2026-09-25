package tunnel

import "time"

// reconnectBackoff is how long a connection that keeps failing waits before it
// is tried again. The first wait is the monitoring interval, and every failure
// in a row doubles the next one until it reaches the ceiling, where it stays.
// A connection that stands puts it back to the interval.
//
// The interval alone was the wait before this, for as long as the process ran.
// A Host that is down for an hour was then asked every few seconds for all of
// that hour, by every tunnel, forward and proxy it carries, with a log line for
// each attempt, and the lines that say why it went down were the ones buried
// under them. Doubling keeps a Host that blinked coming back within the
// interval and lets one that is gone be asked a few times a minute at most.
//
// A ceiling below the interval is not a way of retrying sooner than the
// interval. It leaves every wait at the interval, which is what the ceiling
// being reached at once comes to.
//
// It belongs to the one goroutine that runs the connect loop of its owner, and
// the connect that stands is on that goroutine as well, so it needs no lock.
type reconnectBackoff struct {
	interval time.Duration
	max      time.Duration
	next     time.Duration
}

func newReconnectBackoff(interval time.Duration, max time.Duration) reconnectBackoff {
	return reconnectBackoff{interval: interval, max: max, next: interval}
}

// failed returns how long to wait after the failure that was just seen, and
// makes the wait after the next one twice that, up to the ceiling.
func (b *reconnectBackoff) failed() time.Duration {
	wait := b.next

	if b.next < b.max {
		b.next *= 2
		if b.next > b.max {
			b.next = b.max
		}
	}

	return wait
}

// reset puts the next wait back to the interval. It is called once a
// connection stands, so the failure that ends it starts the doubling over.
func (b *reconnectBackoff) reset() {
	b.next = b.interval
}

// retryInSec is a wait as the whole seconds the log lines state it in.
func retryInSec(wait time.Duration) int {
	return int(wait / time.Second)
}
