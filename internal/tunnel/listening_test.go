package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"golang.org/x/crypto/ssh"
)

// ssOutput is what ss printed on a Linux Host asked about one port, kept
// exactly as it arrived. It is the answer to "ss -ltnH sport = :19601" from an
// OpenSSH server that had been asked to forward that port with GatewayPorts
// left at clientspecified, so both families are in it.
//
// It is pasted rather than written out by hand because what is being tested is
// whether this end reads what that end prints. A sample somebody typed here
// would only say that the parser reads what its author expected.
const ssOutput = "LISTEN 0      128            0.0.0.0:19601 0.0.0.0:*\n" +
	"LISTEN 0      128               [::]:19601    [::]:*\n"

// netstatOutput is the same machine answering "netstat -an", cut down to the
// lines about that port and two others. net-tools writes the IPv6 wildcard as
// :::19601, with no brackets round it, which is the one form here that does not
// come apart at a colon the way an address with a port usually does.
const netstatOutput = "Active Internet connections (servers and established)\n" +
	"Proto Recv-Q Send-Q Local Address           Foreign Address         State\n" +
	"tcp        0      0 0.0.0.0:19601           0.0.0.0:*               LISTEN\n" +
	"tcp        0      0 127.0.0.1:30000         0.0.0.0:*               LISTEN\n" +
	"tcp6       0      0 :::19601                :::*                    LISTEN\n" +
	"tcp6       0      0 :::53895                :::*                    LISTEN\n" +
	"tcp        0      0 127.0.0.1:2222          127.0.0.1:41246         ESTABLISHED\n"

// TestListeningAddressesReadsWhatSsPrinted holds the parser to the output of
// the first command the Host is asked.
func TestListeningAddressesReadsWhatSsPrinted(t *testing.T) {
	got := listeningAddresses(ssOutput, 19601)
	want := []string{"0.0.0.0", "::"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v, out of what ss printed", got, want)
	}
}

// TestListeningAddressesReadsWhatNetstatPrinted holds it to the output of the
// second, which names the state in the last column instead of the first and
// writes the IPv6 wildcard without brackets.
func TestListeningAddressesReadsWhatNetstatPrinted(t *testing.T) {
	got := listeningAddresses(netstatOutput, 19601)
	want := []string{"0.0.0.0", "::"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v, out of what netstat printed", got, want)
	}
}

// TestListeningAddressesLeavesEveryOtherPortAlone is what keeps the row from
// filling with the rest of the machine. netstat is asked plainly, so what comes
// back is every socket there is, and the one this tunnel is about is the one
// that may be written down.
func TestListeningAddressesLeavesEveryOtherPortAlone(t *testing.T) {
	got := listeningAddresses(netstatOutput, 30000)
	want := []string{"127.0.0.1"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v, the port asked about is the only one that counts", got, want)
	}

	if established := listeningAddresses(netstatOutput, 41246); len(established) != 0 {
		t.Fatalf("addresses = %v, want none: that port is on a connection that is established "+
			"and nothing is listening on it", established)
	}
}

// TestListeningAddressesTakesNothingOutOfWhatIsNotAnAddress is the guard on
// what the far side gets to put in this row.
//
// The output comes off another machine, and the ways of getting something other
// than a table back are ordinary: an account with no shell prints a sentence, a
// system with no ss prints a refusal, and either can be read as a table by a
// parser that trusts what it is given. Nothing that fails to parse as an IP at
// the port being asked about may be written down.
func TestListeningAddressesTakesNothingOutOfWhatIsNotAnAddress(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{"an account with no shell", "This account is currently not available.\n"},
		{"a system with no ss", "bash: line 1: ss: command not found\n"},
		{"a shell that said the word itself", "LISTEN is not a command 19601\n"},
		{"a word ending in the port", "LISTEN nonsense:19601\n"},
		{"nothing at all", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := listeningAddresses(tt.output, 19601); len(got) != 0 {
				t.Fatalf("addresses = %v, want none: %q is not an answer about what is listening",
					got, tt.output)
			}
		})
	}
}

// TestCappedWriterDropsTheLineItCutInHalf pins what a Host that prints more
// than the cap leaves behind. The bytes stop somewhere, and where they stop is
// not the end of a line: an address cut anywhere still reads as a field, and
// half of one is a different address rather than half an address.
func TestCappedWriterDropsTheLineItCutInHalf(t *testing.T) {
	out := &cappedWriter{}

	line := "LISTEN 0 128 192.0.2.10:19601 0.0.0.0:*\n"
	for out.kept.Len() < listenAskOutputLimit {
		_, _ = out.Write([]byte(line))
	}

	if !out.truncated {
		t.Fatalf("the writer did not stop, so nothing bounds what a Host may send")
	}

	kept := out.text()
	if !strings.HasSuffix(kept, "\n") {
		t.Fatalf("what was kept ends in %q, so a line that was cut in half is still in it",
			kept[max(0, len(kept)-40):])
	}

	for _, address := range listeningAddresses(kept, 19601) {
		if address != "192.0.2.10" {
			t.Fatalf("address = %q, want 192.0.2.10: an address was read out of a line that was cut",
				address)
		}
	}
}

// sessionAnswer is what the test SSH server does with one exec request: what it
// prints, and whether it answers at all.
type sessionAnswer struct {
	output string
	mute   bool
}

// startAskingSSHServer is startForwardingSSHServer with session channels
// answered instead of refused, which is a Host whose account has a shell.
//
// What it prints is the caller's to decide per command, so that a test can hand
// over output that was captured off a real system. A command the caller says
// nothing about is answered with nothing at all, which is what a Host that has
// not got that command comes to.
func startAskingSSHServer(t *testing.T, confirmed uint32, answers map[string]sessionAnswer) (string, func() []string) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate host key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	var mu sync.Mutex
	var asked []string
	var conns []net.Conn

	t.Cleanup(func() {
		_ = ln.Close()

		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()

			go func(conn net.Conn) {
				defer func() {
					_ = conn.Close()
				}()

				sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer func() {
					_ = sshConn.Close()
				}()

				go func() {
					for newChan := range chans {
						if newChan.ChannelType() != "session" {
							_ = newChan.Reject(ssh.UnknownChannelType, "not supported")

							continue
						}

						channel, requests, err := newChan.Accept()
						if err != nil {
							continue
						}

						go serveSession(channel, requests, answers, func(command string) {
							mu.Lock()
							asked = append(asked, command)
							mu.Unlock()
						})
					}
				}()

				for req := range reqs {
					if !req.WantReply {
						continue
					}
					if req.Type == "tcpip-forward" {
						_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{Port: confirmed}))

						continue
					}
					_ = req.Reply(false, nil)
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), asked...)
	}
}

// serveSession answers one session channel the way an sshd does: the command
// out of the exec request is run, what it printed goes back on the channel, and
// an exit status closes it.
func serveSession(channel ssh.Channel, requests <-chan *ssh.Request, answers map[string]sessionAnswer, note func(string)) {
	defer func() {
		_ = channel.Close()
	}()

	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}

			continue
		}

		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}

			continue
		}

		note(payload.Command)

		answer := answers[payload.Command]
		if answer.mute {
			// No reply, no output and no closing of the channel, which is the
			// Host that has taken the question and says nothing back. The loop
			// goes on waiting for a request that never comes, so what ends this
			// is the other end giving up.
			continue
		}

		if req.WantReply {
			_ = req.Reply(true, nil)
		}

		_, _ = channel.Write([]byte(answer.output))
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))

		return
	}
}

// TestAskListenAddressesTakesTheAddressesTheHostNamed is the whole ask against
// an SSH server, from the session channel to the value the row is stored with.
func TestAskListenAddressesTakesTheAddressesTheHostNamed(t *testing.T) {
	serverAddr, asked := startAskingSSHServer(t, 19601, map[string]sessionAnswer{
		"ss -ltnH sport = :19601": {output: ssOutput},
	})

	client, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	if got := askListenAddresses(client, 19601, listenAskTimeout); got != "0.0.0.0,::" {
		t.Fatalf("listen addresses = %q, want %q", got, "0.0.0.0,::")
	}

	if commands := asked(); len(commands) != 1 || commands[0] != "ss -ltnH sport = :19601" {
		t.Fatalf("the Host was asked %q, want the one command carrying the port and nothing after it",
			commands)
	}
}

// TestAskListenAddressesAsksTheNextCommandWhenTheFirstNamesNothing pins the
// fallback. A Host with no ss prints nothing this can read, and that is not an
// answer that nothing is listening.
func TestAskListenAddressesAsksTheNextCommandWhenTheFirstNamesNothing(t *testing.T) {
	serverAddr, asked := startAskingSSHServer(t, 19601, map[string]sessionAnswer{
		"netstat -an": {output: netstatOutput},
	})

	client, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	if got := askListenAddresses(client, 19601, listenAskTimeout); got != "0.0.0.0,::" {
		t.Fatalf("listen addresses = %q, want %q, out of the second command", got, "0.0.0.0,::")
	}

	if commands := asked(); len(commands) != 2 {
		t.Fatalf("the Host was asked %q, want both commands: the first named no address", commands)
	}
}

// TestAskListenAddressesIsEmptyWhenTheHostRefusesTheSession is the case this is
// built around: the account has no shell, so there is no answer, and no answer
// is not an answer that nothing is open.
//
// What matters as much is the second half. The SSH connection carries the
// forwarded port, and a refused session must cost it nothing at all: the
// refusal is the configuration this program recommends, not a fault to recover
// from.
func TestAskListenAddressesIsEmptyWhenTheHostRefusesTheSession(t *testing.T) {
	// This server refuses every channel, which is what an account with
	// ForceCommand or a shell of /sbin/nologin comes to on the wire.
	serverAddr, _ := startForwardingSSHServer(t)

	client, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	if got := askListenAddresses(client, 19601, listenAskTimeout); got != "" {
		t.Fatalf("listen addresses = %q, want empty: the Host never answered, so nothing is known", got)
	}

	// A global request the server answers at all is proof the connection is
	// still up. It replies with a failure to everything but a forward request,
	// and a failure that arrives is a connection that carried it.
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("the SSH connection did not carry a request after the session was refused: %v", err)
	}
}

// TestAskListenAddressesGivesUpWithinItsBound pins the bound on a Host that
// takes the question and says nothing back. Without one the goroutine and the
// session would be held for as long as the connection stands, and a new pair
// would be taken on every reconnect.
func TestAskListenAddressesGivesUpWithinItsBound(t *testing.T) {
	serverAddr, _ := startAskingSSHServer(t, 19601, map[string]sessionAnswer{
		"ss -ltnH sport = :19601": {mute: true},
		"netstat -an":             {mute: true},
	})

	client, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	started := time.Now()
	got := askListenAddresses(client, 19601, 300*time.Millisecond)
	took := time.Since(started)

	if got != "" {
		t.Fatalf("listen addresses = %q, want empty: the Host answered nothing", got)
	}

	// The bound is over the whole ask and not over each command, so two mute
	// commands still end inside the one bound with room for the wait to be
	// noticed.
	if took > 3*time.Second {
		t.Fatalf("the ask took %v against a bound of 300ms, so the bound is not over the whole of it", took)
	}
}

// TestListenAskTimeoutIsTheSameBoundAsTheProbe keeps the two numbers one
// number. Both are a wait on the far side of the same connection, and an
// operator who has learned what one of them costs has learned the other.
func TestListenAskTimeoutIsTheSameBoundAsTheProbe(t *testing.T) {
	if listenAskTimeout != forwardProbeTimeout {
		t.Fatalf("listenAskTimeout = %v, forwardProbeTimeout = %v, want the same bound",
			listenAskTimeout, forwardProbeTimeout)
	}

	if listenAskTimeout <= 0 {
		t.Fatalf("listenAskTimeout = %v, so the ask is made with no bound at all", listenAskTimeout)
	}
}

// TestListenCommandsCarryThePortAndNothingElse holds what is put on a command
// line to the one value that may be there.
//
// The port is an int, so what is written is digits. Nothing the far side said
// goes anywhere near here: the banner is a string the Host chose, and the Host
// does not get to choose what runs on it.
func TestListenCommandsCarryThePortAndNothingElse(t *testing.T) {
	commands := listenCommands(19601)
	if len(commands) == 0 {
		t.Fatal("no command is sent, so the Host is never asked anything")
	}

	for _, command := range commands {
		for _, char := range command {
			if strings.ContainsRune(";&|$`\n\r<>(){}'\"\\*?", char) {
				t.Fatalf("command %q carries %q, which a shell reads as something other than a word",
					command, string(char))
			}
		}
	}

	if !strings.Contains(commands[0], "19601") {
		t.Fatalf("the first command is %q and does not name the port, so the Host is asked about "+
			"every socket it has", commands[0])
	}
}

// readTunnelListenAddresses reads the field under the lock the tunnel is
// written under, which is what the ask writes it under.
func readTunnelListenAddresses(tun *SSHTunnel, tunnel *models.Tunnel) string {
	tun.tunnelMu.Lock()
	defer tun.tunnelMu.Unlock()

	return tunnel.ListenAddresses
}

// TestEstablishConnectionKeepsTheForwardWhenTheHostRefusesTheSession is the
// rule of this whole thing held at the level it matters at.
//
// The forwarded port is what the operator asked for. Asking what is listening
// on it is worth something, and it is worth nothing next to the port carrying
// traffic, so a Host that refuses the session has to come out of this with a
// tunnel that is up, a row that says connected, and nothing written down about
// addresses.
func TestEstablishConnectionKeepsTheForwardWhenTheHostRefusesTheSession(t *testing.T) {
	m := newSSHTestManager(t, 1)
	// Every channel refused, which is an account with no shell.
	serverAddr, _ := startForwardingSSHServer(t)
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	errc := make(chan error, 1)
	go func() {
		errc <- tun.establishConnection(m, tunnel)
	}()

	waitTunnelClient(t, tun, 10*time.Second)

	// Long enough for the ask to have been refused and come back. It is
	// refused on the spot rather than waited out, so this is not the bound
	// being waited for.
	time.Sleep(time.Second)

	tun.tunnelMu.Lock()
	status := tunnel.Status
	listening := tunnel.ListenAddresses
	openReach := tunnel.OpenReach
	tun.tunnelMu.Unlock()

	if status != "connected" {
		t.Fatalf("tunnel status = %q, want connected: a refused session is not a failure of the tunnel",
			status)
	}

	if listening != "" {
		t.Fatalf("listen addresses = %q, want empty: the Host refused the question, and what it "+
			"never answered may not be written down as an answer", listening)
	}

	if openReach != openReachBoth {
		t.Fatalf("open reach = %q, want %q: the forward request was answered before the session was "+
			"ever asked for", openReach, openReachBoth)
	}

	select {
	case err := <-errc:
		t.Fatalf("establishConnection returned %v while the connection was up, so the refused session "+
			"took the tunnel down with it", err)
	default:
	}

	closeTunnelClient(t, tun)

	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("establishConnection did not return after the connection was closed")
	}
}

// TestRecordListenAddressesDropsTheAnswerOfAnOlderConnection is what keeps a
// slow ask from writing over a tunnel that reconnected under it, the way the
// reachability probe is kept from it.
func TestRecordListenAddressesDropsTheAnswerOfAnOlderConnection(t *testing.T) {
	m := newSSHTestManager(t, 1)
	serverAddr, _ := startAskingSSHServer(t, 19601, map[string]sessionAnswer{
		"ss -ltnH sport = :19601": {output: ssOutput},
	})
	tun, tunnel := newSSHTestTunnel(t, serverAddr)

	current, _, cleanup := dialTestSSHClient(t, serverAddr)
	defer cleanup()

	tun.clientMu.Lock()
	tun.client = current
	tun.clientMu.Unlock()

	// A client that is not the one the tunnel holds, which is what an older
	// connection is by the time its ask comes back.
	older, _, olderCleanup := dialTestSSHClient(t, serverAddr)
	defer olderCleanup()

	tun.recordListenAddresses(m, tunnel, older, 19601)

	if got := readTunnelListenAddresses(tun, tunnel); got != "" {
		t.Fatalf("listen addresses = %q, want empty: the answer of a connection that is gone was "+
			"written to the row of the one that replaced it", got)
	}

	tun.recordListenAddresses(m, tunnel, current, 19601)

	if got := readTunnelListenAddresses(tun, tunnel); got != "0.0.0.0,::" {
		t.Fatalf("listen addresses = %q, want %q: the answer of the current connection was dropped",
			got, "0.0.0.0,::")
	}
}

// bsdNetstatOutput is what the netstat of the BSDs and of macOS prints. It is
// written from their manual pages, which have "Address formats are of the form
// 'host.port' or 'network.port'" and "Unspecified, or 'wildcard', addresses
// and ports appear as '*'", and from output published for macOS.
//
// It has not been taken off a BSD or a Mac, because there is none here. What
// the tests over it hold is that output of the documented shape is read, not
// that a given system prints that shape.
const bsdNetstatOutput = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  *.19601                *.*                    LISTEN
tcp6       0      0  *.19601                *.*                    LISTEN
tcp4       0      0  127.0.0.1.19603        *.*                    LISTEN
tcp4       0      0  192.0.2.10.22          198.51.100.5.52310     ESTABLISHED
tcp46      0      0  *.19604                *.*                    LISTEN
`

// TestListeningAddressesReadsTheBsdShape holds the parser to the other of the
// two shapes: the port after a dot rather than after a colon, and a wildcard
// written as a star whose family is in the protocol column.
func TestListeningAddressesReadsTheBsdShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		port int
		want []string
	}{
		{"a star on one line per family", 19601, []string{"0.0.0.0", "::"}},
		{"an address written out", 19603, []string{"127.0.0.1"}},
		{"tcp46 is a socket of both", 19604, []string{"0.0.0.0", "::"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listeningAddresses(bsdNetstatOutput, tc.port)

			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("addresses = %v, want %v, out of what a BSD netstat prints", got, tc.want)
			}
		})
	}
}

// TestListeningAddressesLeavesTheBsdOutputAloneBelowThePort is the same rule as
// for the other shape, checked here because the dot is also what an IPv4
// address is written with: a line about another port must not be read through
// a split that lands somewhere else.
func TestListeningAddressesLeavesTheBsdOutputAloneBelowThePort(t *testing.T) {
	if got := listeningAddresses(bsdNetstatOutput, 22); len(got) != 0 {
		t.Fatalf("addresses = %v, want none: port 22 is on a line that is not listening", got)
	}

	if got := listeningAddresses(bsdNetstatOutput, 1); len(got) != 0 {
		t.Fatalf("addresses = %v, want none: 1 is a piece of an address and not a port here", got)
	}
}

// TestAStarWithNoFamilyNamedIsPassedOver is what keeps a guess out of the row.
// A wildcard says nothing on its own about which family it stands for, so a
// line that does not carry the protocol column leaves it unread.
func TestAStarWithNoFamilyNamedIsPassedOver(t *testing.T) {
	output := "unknown    0      0  *.19601                *.*                    LISTEN\n"

	if got := listeningAddresses(output, 19601); len(got) != 0 {
		t.Fatalf("addresses = %v, want none: the line never says which family the star is",
			got)
	}
}

// TestAFieldIsOnlyReadWhereThePortMatches is what keeps an address from being
// cut in the wrong place. An IPv4 address carries dots of its own and a socket
// carries a port after one separator or the other, so the only thing that says
// where a field comes apart is which cut leaves the port being asked about.
func TestAFieldIsOnlyReadWhereThePortMatches(t *testing.T) {
	for _, tc := range []struct {
		field string
		port  string
		want  string
		ok    bool
	}{
		{"0.0.0.0:19601", "19601", "0.0.0.0", true},
		{"127.0.0.1.19601", "19601", "127.0.0.1", true},
		{"[::]:19601", "19601", "[::]", true},
		{":::19601", "19601", "::", true},
		{"*.19601", "19601", "*", true},
		// The last dot of the address happens to leave 0:19601, and the last
		// colon leaves 19601. Neither is the port 0, so nothing is read.
		{"0.0.0.0:19601", "0", "", false},
		{"0.0.0.0:19601", "19602", "", false},
		{"nonsense", "19601", "", false},
	} {
		got, ok := hostAtPort(tc.field, tc.port)

		if ok != tc.ok || got != tc.want {
			t.Errorf("hostAtPort(%q, %q) = %q, %v, want %q, %v",
				tc.field, tc.port, got, ok, tc.want, tc.ok)
		}
	}
}
