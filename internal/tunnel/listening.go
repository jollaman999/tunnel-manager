package tunnel

import (
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"golang.org/x/crypto/ssh"
)

// listenAskTimeout bounds the whole of asking the Host what is listening: the
// session channel, the command and everything it prints.
//
// It is one bound over all the commands that are tried rather than one each,
// because what it is there for is the time an operator's screen is out of date
// by, and that is the time of the whole ask. An account whose shell hangs
// instead of refusing would otherwise hold a session open per command.
//
// Ten seconds is what forwardProbeTimeout uses, so there is one number to
// reason about. The SSH connection is up by the time this runs, so what is
// being allowed for is a Host that is slow to answer rather than a distance.
const listenAskTimeout = 10 * time.Second

// listenAskOutputLimit is how much of what the Host prints is kept. The output
// arrives over a connection to another machine and its size is that machine's
// to choose, so a reader with no bound on it is this process holding whatever
// the far side decided to send.
//
// Sixty-four kibibytes is far more than either command prints for a whole
// table of listening sockets, so the bound is not one an honest answer meets.
const listenAskOutputLimit = 64 << 10

// listenCommands is what the Host is asked, in the order it is asked, and the
// port is the one value that goes into them.
//
// The port is an int, so what is written into the command line is digits and
// can be nothing else. Nothing the Host itself said, its banner among it, is
// ever put there: that is a string the far side chose, and the far side does
// not get to choose what runs on it.
//
// ss is asked first and asked only about the one port, which is an answer of a
// line or two instead of a table of every socket on the machine. netstat comes
// after it for a Host that has no ss, and it is asked plainly: its filtering is
// not the same from one system to the next, and what this end has to be sure
// of is the shape of the output rather than the flags.
//
// Both were measured against a real sshd before being written down here, on
// Linux (iproute2 ss and net-tools netstat). A Host of another sort answers
// with something this does not read, and that is an empty reading, which is
// what not being able to ask means everywhere else here.
func listenCommands(port int) []string {
	number := strconv.Itoa(port)

	return []string{
		"ss -ltnH sport = :" + number,
		"netstat -an",
	}
}

// cappedWriter keeps the first listenAskOutputLimit bytes of what is written
// to it and counts the rest away. It never fails the write: a command stopped
// half way through by a write error would leave the SSH library reporting a
// failure of the session, and what is wanted is the opposite of that, an
// answer that is merely cut short.
type cappedWriter struct {
	kept      strings.Builder
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	room := listenAskOutputLimit - w.kept.Len()
	if room <= 0 {
		w.truncated = true

		return len(p), nil
	}

	if len(p) > room {
		w.kept.Write(p[:room])
		w.truncated = true

		return len(p), nil
	}

	w.kept.Write(p)

	return len(p), nil
}

// text is what was kept, without the line the cap cut in half. A line that
// arrived in part is not an answer: an address cut anywhere still reads as a
// field, and half of one is not half an address but a different one.
func (w *cappedWriter) text() string {
	kept := w.kept.String()
	if !w.truncated {
		return kept
	}

	at := strings.LastIndexByte(kept, '\n')
	if at < 0 {
		return ""
	}

	return kept[:at+1]
}

// runRemoteCommand runs one command on the Host and hands back what it printed
// on stdout, up to the cap and within the time left.
//
// The exit status is not looked at, and neither is the error. A Host with no ss
// prints a refusal from its shell and exits non-zero, a Host with ss prints the
// table and exits zero, and there are enough shells and enough ways to be
// missing a command that the status is not what tells those apart. What tells
// them apart is the output: an address is an address whatever the status beside
// it was, and everything else is read as nothing at all.
//
// The session is closed on the way out and the SSH connection is not touched.
// Nothing here may cost the tunnel anything: the forwarded port is what the
// operator asked for, and what is going on here is a question about it.
func runRemoteCommand(client *ssh.Client, command string, timeout time.Duration) string {
	if timeout <= 0 {
		return ""
	}

	opened := make(chan *ssh.Session, 1)
	done := make(chan *cappedWriter, 1)

	go func() {
		session, err := client.NewSession()
		if err != nil {
			opened <- nil
			done <- nil

			return
		}

		out := &cappedWriter{}
		session.Stdout = out
		opened <- session

		// The error is dropped on purpose, and so is anything the command
		// wrote to stderr. See above.
		_ = session.Run(command)
		_ = session.Close()

		done <- out
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case out := <-done:
		if out == nil {
			return ""
		}

		return out.text()
	case <-timer.C:
		// The session is closed so that the goroutine above is let go rather
		// than held for as long as the far side keeps the command running. The
		// output it collected is never read after this, which is what keeps the
		// two ends off the same buffer.
		select {
		case session := <-opened:
			if session != nil {
				_ = session.Close()
			}
		default:
		}

		return ""
	}
}

// askListenAddresses asks the Host what is listening on the forwarded port and
// hands back what it named, in the shape models.Tunnel.ListenAddresses is
// stored in.
//
// The commands are tried in turn and the first one that names an address is
// the answer. A command that names none is not an answer that nothing is
// listening: it is a command that is not there, or a shell that refused, or a
// system whose output this does not read, and the next one is tried. What
// comes of all of them naming none is the empty value, which everything here
// reads as not known.
func askListenAddresses(client *ssh.Client, port int, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)

	for _, command := range listenCommands(port) {
		output := runRemoteCommand(client, command, time.Until(deadline))

		if found := listeningAddresses(output, port); len(found) > 0 {
			return strings.Join(found, ",")
		}
	}

	return ""
}

// listeningAddresses picks the addresses that are listening on port out of
// what one of the commands printed, sorted and without repeats.
//
// A line counts where it carries the word LISTEN as a field of its own, which
// is the state column of ss and the last column of netstat, and where one of
// its fields is an address at this port. Everything else on the line is passed
// over, so a column this end has never seen costs nothing.
//
// A field is cut at its last colon or, where that gives nothing, at its last
// dot. Which of the two separates the port from the address is the one thing
// that differs between the systems this runs against.
//
//   - ss and the netstat of net-tools write a colon: 0.0.0.0:22, [::]:22 and
//     :::22 all come apart at the last one, which is why it is the last and
//     not the first.
//   - the netstat of the BSDs and of macOS writes a dot. Their manual pages
//     have it as "Address formats are of the form 'host.port' or
//     'network.port'", so 127.0.0.1.22 and *.22 are what a listening socket
//     reads as there.
//
// A wildcard is written differently as well. ss and net-tools name the address
// itself, and the BSDs write "*", which their manual pages have as
// "Unspecified, or 'wildcard', addresses and ports appear as '*'". Which
// family that "*" stands for is on the same line, in the protocol column that
// reads tcp4, tcp6 or tcp46, and a line that carries neither is passed over
// rather than guessed at.
//
// **The BSD shapes are written from those manual pages and have not been run
// against a BSD or a Mac.** What the tests below hold is that output of the
// documented shape is read; whether a given system prints that shape is not
// something this end has measured. The guard below is what makes that safe to
// have written: the address has to parse as an IP, so a shape that is read
// wrong yields nothing rather than an address that is wrong.
//
// That same guard keeps everything else the far side printed out of the row:
// the peer column of a listening socket, the sentence an account with no shell
// prints, the refusal of a shell that has no such command. It also drops the
// few addresses this end cannot read, an IPv4 address that ss wrote a scope
// onto among them, and an address dropped is read as not known, which is the
// safe way round.
func listeningAddresses(output string, port int) []string {
	wanted := strconv.Itoa(port)
	seen := make(map[string]bool)

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if !listeningLine(fields) {
			continue
		}

		wildcards := wildcardAddresses(fields)

		for _, field := range fields {
			host, ok := hostAtPort(field, wanted)
			if !ok {
				continue
			}

			// The wildcard of the BSDs stands for whichever families the
			// protocol column named, which is one address or two.
			if host == "*" {
				for _, address := range wildcards {
					seen[address] = true
				}

				continue
			}

			address, err := netip.ParseAddr(strings.Trim(host, "[]"))
			if err != nil {
				continue
			}

			seen[address.String()] = true
		}
	}

	found := make([]string, 0, len(seen))
	for address := range seen {
		found = append(found, address)
	}

	sort.Strings(found)

	return found
}

// hostAtPort splits a field of output into the address and the port, and says
// whether the port is the one being asked about.
//
// Both separators are tried and each attempt stands or falls on the port
// matching, which is what lets an IPv4 address carry dots of its own without
// being taken apart at one of them: 0.0.0.0:19601 cut at its last dot gives a
// port of 0:19601, that is not the port being asked about, and the colon is
// tried next. The order the two are tried in decides nothing for that reason.
// The colon is named first because it is the shape that was measured here.
func hostAtPort(field, port string) (string, bool) {
	for _, separator := range []byte{':', '.'} {
		at := strings.LastIndexByte(field, separator)
		if at < 0 {
			continue
		}

		if field[at+1:] == port {
			return field[:at], true
		}
	}

	return "", false
}

// wildcardAddresses is what a "*" on this line stands for, read off the
// protocol column.
//
// The BSDs and macOS write the protocol as tcp4, tcp6 or tcp46, and that is
// the only thing on such a line that says which family the socket is of: the
// address column is a bare "*". tcp46 is a socket of both, so it stands for
// both addresses.
//
// A line with no such column gets nothing back, and the "*" on it is then
// passed over. That is the case for ss and for net-tools, neither of which
// writes a bare "*" for a listening address in the first place, and for any
// output this end has not seen: a wildcard whose family is not stated is not a
// wildcard this end may name.
func wildcardAddresses(fields []string) []string {
	for _, field := range fields {
		switch field {
		case "tcp4":
			return []string{"0.0.0.0"}
		case "tcp6":
			return []string{"::"}
		case "tcp46":
			return []string{"0.0.0.0", "::"}
		}
	}

	return nil
}

// listeningLine reports whether a line of output is about a socket that is
// listening. Both commands name that state the same way and in a column of
// their own, at the front for ss and at the back for netstat, so the word is
// looked for among the fields rather than at a place in the line.
func listeningLine(fields []string) bool {
	for _, field := range fields {
		if field == "LISTEN" {
			return true
		}
	}

	return false
}

// recordListenAddresses asks the Host what is listening on the forwarded port
// and writes what it said to the tunnel row.
//
// It runs beside the accept loop for the reason the reachability probe does:
// the ask waits out its whole bound against a Host that does not answer, and
// the loop it would hold is the one that carries the forwarded traffic.
//
// A reading taken on a connection that is no longer the current one is dropped,
// again for the reason the probe drops one. The tunnel reconnected while the
// ask was waiting, an ask of its own is running for the new connection, and
// writing here would put the addresses of a connection that is gone on the row
// of the one that replaced it.
//
// Nothing is logged about an ask that came back with nothing. An account with
// no shell is the configuration this program recommends, so the usual outcome
// here is no answer, and a line about it on every connection of every tunnel
// would be noise that says nothing is wrong.
func (t *SSHTunnel) recordListenAddresses(m *Manager, tunnel *models.Tunnel, measured *ssh.Client, port int) {
	addresses := askListenAddresses(measured, port, listenAskTimeout)

	t.clientMu.RLock()
	current := t.client
	t.clientMu.RUnlock()

	if current != measured {
		return
	}

	t.tunnelMu.Lock()
	tunnel.ListenAddresses = addresses
	t.saveTunnelStatus(m, tunnel)
	t.tunnelMu.Unlock()
}
