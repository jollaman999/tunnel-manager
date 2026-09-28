package tunnel

import (
	"errors"
	"time"

	"go.uber.org/zap"
)

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
// Two failures are waited out by rules of their own, which after says.
//
// It belongs to the one goroutine that runs the connect loop of its owner, and
// the connect that stands is on that goroutine as well, so it needs no lock.
type reconnectBackoff struct {
	interval time.Duration
	max      time.Duration
	next     time.Duration
	// denied is how many attempts in a row the SSH server refused to open
	// the forwarded port on.
	denied int
}

func newReconnectBackoff(interval time.Duration, max time.Duration) reconnectBackoff {
	return reconnectBackoff{interval: interval, max: max, next: interval}
}

// The reasons after gives for a wait that is not the doubling, which the line
// that says the attempt failed carries as retry_reason.
const (
	retryReasonJumpProhibited = "jump_prohibited"
	retryReasonForwardDenied  = "forward_denied"
)

// quickForwardDenials is how many refusals in a row to open the forwarded port
// are tried again at the interval before the doubling takes over.
const quickForwardDenials = 3

// after returns how long to wait after a connection attempt that failed with
// err, and the reason when the wait is not the doubling.
//
// A Host of the jump route that is prohibited from opening the channel to the
// next is refusing by its configuration (prohibitedChannel), and no number of
// attempts changes that. The attempt is still made again, since the change to
// the configuration is made on that Host and nothing here hears of it, but at
// the ceiling from the first, so a route nobody has put right yet is asked a
// few times a minute rather than at the interval for its first minute.
//
// A Host that refuses to open the forwarded port may still hold it for a
// session of this tunnel that went away without closing, as sshd does when a
// connection somewhere on the way was cut without a FIN, and lets it go
// moments later. Doubling there turns one refusal into a wait of many
// seconds for a port that is free already, so the first refusals in a row are
// tried again at the interval and leave the doubling where it was. A port
// that stays refused past them is taken by another process, and the doubling
// takes it over from where the failures before them had left it.
func (b *reconnectBackoff) after(err error) (time.Duration, string) {
	switch {
	case jumpProhibited(err):
		b.denied = 0
		return b.ceiling(), retryReasonJumpProhibited
	case listenErrorKind(err) == errorKindForwardDenied:
		b.denied++
		if b.denied <= quickForwardDenials {
			return b.interval, retryReasonForwardDenied
		}
		return b.double(), retryReasonForwardDenied
	}

	return b.failed(), ""
}

// jumpProhibited is whether err is a Host of the jump route refusing to open
// the channel to the next.
func jumpProhibited(err error) bool {
	var jumpErr *JumpError

	return errors.As(err, &jumpErr) && prohibitedChannel(jumpErr.Err)
}

// retryReasonField is the reason after gave as a field of the line, and no
// field at all for the doubling.
func retryReasonField(reason string) zap.Field {
	if reason == "" {
		return zap.Skip()
	}

	return zap.String("retry_reason", reason)
}

// ceiling is the longest wait, which is never below the interval.
func (b *reconnectBackoff) ceiling() time.Duration {
	if b.max < b.interval {
		return b.interval
	}

	return b.max
}

// failed returns how long to wait after the failure that was just seen, and
// makes the wait after the next one twice that, up to the ceiling.
func (b *reconnectBackoff) failed() time.Duration {
	b.denied = 0

	return b.double()
}

// double is failed without ending a run of refusals of the forwarded port.
func (b *reconnectBackoff) double() time.Duration {
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
	b.denied = 0
}

// retryInSec is a wait as the whole seconds the log lines state it in.
func retryInSec(wait time.Duration) int {
	return int(wait / time.Second)
}
