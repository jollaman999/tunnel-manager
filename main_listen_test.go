package main

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// freeLoopbackPort returns a port nothing listens on at the moment it returns.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}

	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	return port
}

// holdLoopbackPort opens a port for the length of the test, the way another
// program holding the stored port would.
func holdLoopbackPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to open a port to hold: %v", err)
	}

	t.Cleanup(func() {
		_ = l.Close()
	})

	return l.Addr().(*net.TCPAddr).Port
}

func avoidNothing(int) bool {
	return false
}

func TestListenAPIOpensTheStoredPortWhenItIsFree(t *testing.T) {
	stored := freeLoopbackPort(t)

	l, port, reason, err := listenAPI("127.0.0.1", stored, 0, func(int) bool {
		t.Fatalf("avoid was asked while the stored port was free")
		return false
	})
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port != stored || l.Addr().(*net.TCPAddr).Port != stored {
		t.Fatalf("opened %s and reported %d, want the stored port %d", l.Addr(), port, stored)
	}

	if reason != "" {
		t.Fatalf("gave the reason %q for a stored port that was opened", reason)
	}
}

func TestListenAPIOpensAnotherPortWhenTheStoredOneIsTaken(t *testing.T) {
	stored := holdLoopbackPort(t)

	l, port, reason, err := listenAPI("127.0.0.1", stored, 0, avoidNothing)
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port == stored {
		t.Fatalf("reported the taken port %d", stored)
	}

	if reason != apiPortInUse {
		t.Fatalf("gave the reason %q for a port another socket holds, want %q", reason, apiPortInUse)
	}

	if l.Addr().(*net.TCPAddr).Port != port {
		t.Fatalf("opened %s and reported %d", l.Addr(), port)
	}
}

// The API listens on the wildcard of both families. Another program that holds
// the stored port on an IPv4 address alone takes the IPv4 clients of it, so
// the port counts as taken. Windows would let a dual-stack socket bind over
// such a holder, which is what SO_EXCLUSIVEADDRUSE is there to refuse.
func TestListenAPIOnTheWildcardMovesAwayFromAPortHeldOnIPv4Alone(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "127.0.0.1:0"} {
		held, err := net.Listen("tcp4", address)
		if err != nil {
			t.Fatalf("failed to hold %s: %v", address, err)
		}

		stored := held.Addr().(*net.TCPAddr).Port

		l, port, reason, err := listenAPI("", stored, 0, avoidNothing)
		if err != nil {
			_ = held.Close()
			t.Fatalf("listenAPI with %s held: %v", held.Addr(), err)
		}

		_ = l.Close()
		_ = held.Close()

		if port == stored {
			t.Errorf("opened the stored port %d while another socket held it on %s", stored, address)
		}

		if reason != apiPortInUse {
			t.Errorf("gave the reason %q with %s held, want %q", reason, address, apiPortInUse)
		}
	}
}

func TestListenAPIPicksAgainWhileAvoidRefuses(t *testing.T) {
	stored := holdLoopbackPort(t)

	refused := map[int]bool{}

	l, port, _, err := listenAPI("127.0.0.1", stored, 0, func(p int) bool {
		if len(refused) < 3 {
			refused[p] = true
			return true
		}

		return false
	})
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if len(refused) != 3 {
		t.Fatalf("avoid refused %d distinct ports, want 3: %v", len(refused), refused)
	}

	if refused[port] || port == stored {
		t.Fatalf("settled on %d, which was refused or is the stored %d", port, stored)
	}

	// The refused ones are let go once a port is settled on.
	for p := range refused {
		again, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			t.Fatalf("refused port %d is still held: %v", p, err)
		}
		_ = again.Close()
	}
}

func TestListenAPIGivesUpWithTheStoredPortError(t *testing.T) {
	stored := holdLoopbackPort(t)

	asked := 0

	l, port, _, err := listenAPI("127.0.0.1", stored, 0, func(int) bool {
		asked++
		return true
	})
	if err == nil {
		_ = l.Close()
		t.Fatalf("opened port %d while avoid refused every one", port)
	}

	if !portUnavailable(err) {
		t.Fatalf("the error is not the one of the taken stored port: %v", err)
	}

	if asked != apiListenAttempts {
		t.Fatalf("avoid was asked %d times, want %d", asked, apiListenAttempts)
	}
}

// A port below 1024 is refused to a process without the privilege for it,
// which is a failure another port must not hide.
func TestListenAPILeavesOtherFailuresAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows lets any process open a port below 1024")
	}

	l, err := net.Listen("tcp", "127.0.0.1:1")
	if err == nil {
		_ = l.Close()
		t.Skip("this process may open port 1, so there is no refusal to observe")
	}

	if portUnavailable(err) {
		t.Skip("port 1 is taken here rather than refused")
	}

	l, port, _, err := listenAPI("127.0.0.1", 1, 0, func(int) bool {
		t.Fatalf("avoid was asked for a failure that was not a taken port")
		return false
	})
	if err == nil {
		_ = l.Close()
		t.Fatalf("opened port %d in place of a refused one", port)
	}

	if port != 1 {
		t.Fatalf("reported port %d, want the stored 1", port)
	}
}

func TestListenAPIOpensThePreviousPortWhenTheStoredOneIsTaken(t *testing.T) {
	stored := holdLoopbackPort(t)
	previous := freeLoopbackPort(t)

	l, port, _, err := listenAPI("127.0.0.1", stored, previous, avoidNothing)
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port != previous || l.Addr().(*net.TCPAddr).Port != previous {
		t.Fatalf("opened %s and reported %d, want the previous port %d", l.Addr(), port, previous)
	}
}

func TestListenAPIOpensTheStoredPortOverThePreviousOne(t *testing.T) {
	stored := freeLoopbackPort(t)
	previous := freeLoopbackPort(t)

	l, port, _, err := listenAPI("127.0.0.1", stored, previous, avoidNothing)
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port != stored {
		t.Fatalf("reported %d, want the stored port %d", port, stored)
	}
}

func TestListenAPIPicksAnotherPortWhenThePreviousOneIsTakenToo(t *testing.T) {
	stored := holdLoopbackPort(t)
	previous := holdLoopbackPort(t)

	l, port, _, err := listenAPI("127.0.0.1", stored, previous, avoidNothing)
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port == stored || port == previous {
		t.Fatalf("reported %d, which is the taken stored %d or previous %d", port, stored, previous)
	}

	if l.Addr().(*net.TCPAddr).Port != port {
		t.Fatalf("opened %s and reported %d", l.Addr(), port)
	}
}

func TestListenAPISkipsAPreviousPortThatAvoidRefuses(t *testing.T) {
	stored := holdLoopbackPort(t)
	previous := freeLoopbackPort(t)

	l, port, _, err := listenAPI("127.0.0.1", stored, previous, func(p int) bool {
		return p == previous
	})
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer l.Close()

	if port == previous || port == stored {
		t.Fatalf("reported %d, which is the refused previous %d or the taken stored %d", port, previous, stored)
	}

	// The previous port was not opened on the way.
	again, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(previous)))
	if err != nil {
		t.Fatalf("the refused previous port %d is held: %v", previous, err)
	}
	_ = again.Close()
}

func TestTakePreviousAPIPortReadsAndClearsIt(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"35351", 35351},
		{" 8888 ", 8888},
		{"1", 1},
		{"65535", 65535},
		{"0", 0},
		{"65536", 0},
		{"-1", 0},
		{"port", 0},
		{"", 0},
	}

	for _, c := range cases {
		t.Setenv(previousAPIPortEnv, c.value)

		got := takePreviousAPIPort()
		if got != c.want {
			t.Errorf("%s=%q read as %d, want %d", previousAPIPortEnv, c.value, got, c.want)
		}

		if _, ok := os.LookupEnv(previousAPIPortEnv); ok {
			t.Errorf("%s=%q is still in the environment after it was read", previousAPIPortEnv, c.value)
		}

		if again := takePreviousAPIPort(); again != 0 {
			t.Errorf("a second read gave %d, want 0", again)
		}
	}
}

func TestRestartEnvironHandsOverOnlyThePortInUse(t *testing.T) {
	t.Setenv(previousAPIPortEnv, "1111")

	env := restartEnviron(2222)

	var found []string
	for _, kv := range env {
		if strings.HasPrefix(kv, previousAPIPortEnv+"=") {
			found = append(found, kv)
		}
	}

	if len(found) != 1 || found[0] != previousAPIPortEnv+"=2222" {
		t.Fatalf("handed over %v, want only %s=2222", found, previousAPIPortEnv)
	}

	for _, kv := range restartEnviron(0) {
		if strings.HasPrefix(kv, previousAPIPortEnv+"=") {
			t.Fatalf("a port of 0 handed over %s", kv)
		}
	}
}

// TestTheFallbackPortStaysClearOfTheSocksProxies covers what the port the API
// falls back to is kept off: every local port of a local forward, and the port
// of every SOCKS5 proxy that is switched on, whether its Host is enabled or
// not. A proxy that is switched off opens nothing and holds nothing.
func TestTheFallbackPortStaysClearOfTheSocksProxies(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tunnel-manager.db")), &gorm.Config{
		Logger: gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&models.Host{}, &models.LocalForward{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	rows := []interface{}{
		&models.Host{ID: 1, IP: "192.0.2.1", Port: 22, User: "root", Enabled: true,
			SocksEnabled: true, SocksPort: 1080},
		&models.Host{ID: 2, IP: "192.0.2.2", Port: 22, User: "root", Enabled: false,
			SocksEnabled: true, SocksPort: 1081},
		&models.Host{ID: 3, IP: "192.0.2.3", Port: 22, User: "root", Enabled: true,
			SocksEnabled: false, SocksPort: 1082},
		&models.LocalForward{HostID: 1, LocalPort: 15432, TargetIP: "127.0.0.1", TargetPort: 5432},
	}

	for _, row := range rows {
		err = db.Create(row).Error
		if err != nil {
			t.Fatalf("failed to store %+v: %v", row, err)
		}
	}

	held, err := heldLocalPorts(db)
	if err != nil {
		t.Fatalf("heldLocalPorts: %v", err)
	}

	for port, want := range map[int]bool{1080: true, 1081: true, 1082: false, 15432: true, 8443: false} {
		if held[port] != want {
			t.Errorf("port %d is held = %v, want %v", port, held[port], want)
		}
	}

	// The same function is what the startup hands listenAPI as avoid, so a
	// proxy on the port the system would pick is passed over there as well.
	stored := holdLoopbackPort(t)
	socks := freeLoopbackPort(t)

	err = db.Model(&models.Host{}).Where("id = ?", 1).Update("socks_port", socks).Error
	if err != nil {
		t.Fatalf("failed to move the proxy: %v", err)
	}

	held, err = heldLocalPorts(db)
	if err != nil {
		t.Fatalf("heldLocalPorts: %v", err)
	}

	listener, port, _, err := listenAPI("127.0.0.1", stored, socks, func(port int) bool {
		return held[port]
	})
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer listener.Close()

	if port == socks {
		t.Fatalf("the API fell back to %d, the port of a SOCKS5 proxy", port)
	}
}
