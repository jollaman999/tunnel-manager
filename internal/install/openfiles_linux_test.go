//go:build linux

package install

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// measuredOpenFiles is what the deployed service on this machine actually had
// open, read off it while it was running:
//
//	$ PID=$(systemctl show tunnel-manager -p MainPID --value)
//	$ sudo ls -l /proc/$PID/fd | awk '{print $NF}'
//
// The four paths are the answer this whole mechanism exists to produce, and the
// rest of what that listing held - sockets, the epoll descriptor, the pipe of
// the log, /dev/null - is what has to be left out of it.
var measuredOpenFiles = []string{
	"/var/lib/tunnel-manager/logs/tunnel-manager.log",
	"/var/lib/tunnel-manager/tunnel-manager.db",
	"/var/lib/tunnel-manager/tunnel-manager.db-shm",
	"/var/lib/tunnel-manager/tunnel-manager.db-wal",
}

// fakeProc plants a process directory of symbolic links and points the reader
// at it.
//
// The links are made for real and read back with readlink, which is the whole
// of what openFilesOfPID does on a live process: what /proc hands out is
// symbolic links whose target is a path that need not exist, and a link to
// socket:[...] made here reads back exactly as the kernel writes it.
func fakeProc(t *testing.T, pid int, targets []string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid), "fd")

	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatalf("failed to make %s: %v", dir, err)
	}

	for descriptor, target := range targets {
		err = os.Symlink(target, filepath.Join(dir, strconv.Itoa(descriptor)))
		if err != nil {
			t.Fatalf("failed to plant the descriptor %d of the process: %v", descriptor, err)
		}
	}

	was := procRoot
	procRoot = root

	t.Cleanup(func() {
		procRoot = was
	})
}

// TestOpenFilesOfPID reads a process directory of the shape /proc has and holds
// what comes out of it to the four files of the deployed service.
func TestOpenFilesOfPID(t *testing.T) {
	const pid = 219825

	fakeProc(t, pid, []string{
		// Everything a process of this program has open that is not a file of
		// the installation. The API listener, the connections to the hosts, the
		// epoll descriptor of the runtime, the pipe the logger writes through,
		// and the standard input of a service, which systemd gives /dev/null.
		"socket:[24680]",
		"socket:[24681]",
		"anon_inode:[eventpoll]",
		"pipe:[13579]",
		"/dev/null",
		// A file that was removed while it was still open, which is what a log
		// looks like after somebody deleted it by hand. There is nothing at
		// that path to remove, and the path may since have been taken by
		// another file.
		"/var/lib/tunnel-manager/logs/tunnel-manager.log.1 (deleted)",
		// The four that are the answer, with the database open twice over, the
		// way a second connection to it shows up.
		"/var/lib/tunnel-manager/tunnel-manager.db",
		"/var/lib/tunnel-manager/tunnel-manager.db",
		"/var/lib/tunnel-manager/tunnel-manager.db-wal",
		"/var/lib/tunnel-manager/tunnel-manager.db-shm",
		"/var/lib/tunnel-manager/logs/tunnel-manager.log",
	})

	files, err := openFilesOfPID(pid)
	if err != nil {
		t.Fatalf("reading the open files of %d failed: %v", pid, err)
	}

	if strings.Join(files, "\n") != strings.Join(measuredOpenFiles, "\n") {
		t.Errorf("the open files are\n%s\nwant\n%s",
			strings.Join(files, "\n"), strings.Join(measuredOpenFiles, "\n"))
	}

	t.Logf("read out of the process:\n%s", strings.Join(files, "\n"))

	database, err := databaseAmong(files)
	if err != nil {
		t.Fatalf("the database could not be picked out of them: %v", err)
	}

	if database != "/var/lib/tunnel-manager/tunnel-manager.db" {
		t.Errorf("the database is %q, want /var/lib/tunnel-manager/tunnel-manager.db", database)
	}
}

// TestOpenFilesOfAProcessThatIsNotThere covers the answer for a process id
// nothing is running under. It has to be ErrOpenFilesUnknown and not a list,
// because an empty list would read as a service with nothing of ours open,
// which is a different thing from one that could not be asked.
func TestOpenFilesOfAProcessThatIsNotThere(t *testing.T) {
	fakeProc(t, 1, nil)

	_, err := openFilesOfPID(4242)
	if !errors.Is(err, ErrOpenFilesUnknown) {
		t.Errorf("reading the open files of a process that is not there answered %v, want %v",
			err, ErrOpenFilesUnknown)
	}

	t.Logf("answered as it should: %v", err)
}

// TestDatabaseAmong covers the rule that picks the database out: the file that
// has its own write ahead log open beside it.
func TestDatabaseAmong(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
		fails bool
	}{
		{
			name:  "the deployed service",
			files: measuredOpenFiles,
			want:  "/var/lib/tunnel-manager/tunnel-manager.db",
		},
		{
			// -db may name a file called anything at all, which is why the
			// name is not what this reads.
			name:  "a database named anything",
			files: []string{"/srv/tm/data", "/srv/tm/data-wal", "/srv/tm/data-shm"},
			want:  "/srv/tm/data",
		},
		{
			name:  "nothing with a write ahead log beside it",
			files: []string{"/var/lib/tunnel-manager/logs/tunnel-manager.log"},
			fails: true,
		},
		{
			name:  "no files at all",
			files: nil,
			fails: true,
		},
		{
			// Two of them is not a database to pick from, it is a process that
			// is not the one this removal is about.
			name: "two databases",
			files: []string{
				"/var/lib/tunnel-manager/tunnel-manager.db", "/var/lib/tunnel-manager/tunnel-manager.db-wal",
				"/srv/other.db", "/srv/other.db-wal",
			},
			fails: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := databaseAmong(c.files)

			if c.fails {
				if err == nil {
					t.Fatalf("databaseAmong(%v) answered %q, want a failure", c.files, got)
				}

				if !errors.Is(err, ErrOpenFilesUnknown) {
					t.Errorf("the failure is %v, want it to be %v", err, ErrOpenFilesUnknown)
				}

				t.Logf("refused as it should: %v", err)

				return
			}

			if err != nil {
				t.Fatalf("databaseAmong(%v) failed: %v", c.files, err)
			}

			if got != c.want {
				t.Errorf("the database is %q, want %q", got, c.want)
			}
		})
	}
}
