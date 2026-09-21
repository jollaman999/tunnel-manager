//go:build darwin

package install

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// defaultsPlist is the LaunchDaemon this install writes on a Mac where the
// operator named nothing.
//
// It is written out here whole rather than built from the constants, because
// this file is the whole of the check there is: the backend was never run on a
// Mac. Built from the same pieces the code builds it from, this would pass for
// any plist at all. Written out, a change to the label, to the flags or to the
// keys is a line of a diff, and this is also the text that goes into the
// design document for somebody with a Mac to hold the real thing against.
const defaultsPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.github.jollaman999.tunnel-manager</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/tunnel-manager</string>
		<string>-db</string>
		<string>/Library/Application Support/tunnel-manager/tunnel-manager.db</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`

func TestLaunchdPlistOfTheDefaults(t *testing.T) {
	written := string(launchdPlist(plannedFor("darwin")))

	if written != defaultsPlist {
		t.Errorf("the plist of the default paths is\n%s\nwant\n%s", written, defaultsPlist)
	}

	// Logged so that go test -v shows the file itself, which is what somebody
	// with a Mac has to hold /Library/LaunchDaemons against.
	t.Logf("%s\n%s", launchdPlistPath, written)
}

// TestLaunchdPlistIsReadBackWhole covers the pair that has to agree: what
// Register writes and what Current reads. An install reads its own
// registration back before it reports, so a writer and a reader that drifted
// apart would fail every install rather than only an uninstall.
func TestLaunchdPlistIsReadBackWhole(t *testing.T) {
	plan := plannedFor("darwin")
	path := filepath.Join(t.TempDir(), filepath.Base(launchdPlistPath))

	err := writePlist(path, launchdPlist(plan))
	if err != nil {
		t.Fatalf("failed to write the plist: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the plist: %v", err)
	}

	// launchd refuses a job whose plist is writable by anybody else.
	if info.Mode().Perm() != plistMode {
		t.Errorf("the plist is %v, want %v", info.Mode().Perm(), plistMode)
	}

	installed, err := currentFrom(path)
	if err != nil {
		t.Fatalf("failed to read the plist back: %v", err)
	}

	if installed.ExecutablePath != plan.ExecutablePath {
		t.Errorf("the executable read back is %q, want %q", installed.ExecutablePath, plan.ExecutablePath)
	}

	// The plist carries -db and the read does not answer with it. Which
	// database an installation is using is asked of the running process, which
	// is the one place it is certain: a plist need not carry -db at all, and
	// then the arguments say nothing about the file that was opened.
	if installed.DatabaseFile != "" {
		t.Errorf("the database read back is %q, want it empty and asked of the process", installed.DatabaseFile)
	}

	if installed.DefinitionPath != path {
		t.Errorf("the definition read back is %q, want %q", installed.DefinitionPath, path)
	}
}

// TestLaunchdPlistEscapesPaths covers a path a filesystem takes and XML does
// not. Unescaped, it would write a plist launchd cannot parse, which is an
// installation that is registered and never comes up.
func TestLaunchdPlistEscapesPaths(t *testing.T) {
	plan := Plan{
		ExecutablePath: "/usr/local/bin/tunnel & manager",
		DataDir:        "/Library/Application Support/<data>",
		DatabaseFile:   "/Library/Application Support/<data>/tunnel-manager.db",
	}

	document := string(launchdPlist(plan))

	if strings.Contains(document, "tunnel & manager") || strings.Contains(document, "<data>") {
		t.Errorf("the paths went in unescaped:\n%s", document)
	}

	arguments, err := programArguments([]byte(document))
	if err != nil {
		t.Fatalf("the plist of those paths could not be parsed back: %v\n%s", err, document)
	}

	want := []string{plan.ExecutablePath, "-db", plan.DatabaseFile}

	if !reflect.DeepEqual(arguments, want) {
		t.Errorf("the arguments read back are %q, want %q", arguments, want)
	}
}

// TestProgramArgumentsOfAHandWrittenPlist covers a file this package did not
// write: other keys around the one that matters, a dictionary in a different
// order, and whitespace of somebody else's editor. The plist is a text file on
// the operator's machine, and one they edited is still the registration an
// uninstall has to be able to read.
func TestProgramArgumentsOfAHandWrittenPlist(t *testing.T) {
	document := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>KeepAlive</key>
    <true/>
    <key>StandardErrorPath</key>
    <string>/var/log/tunnel-manager.err</string>
    <key>ProgramArguments</key>
    <array>
      <string>/opt/tm/tunnel-manager</string>
      <string>-db</string>
      <string>/opt/tm/tm.db</string>
    </array>
    <key>Label</key>
    <string>io.github.jollaman999.tunnel-manager</string>
  </dict>
</plist>
`

	arguments, err := programArguments([]byte(document))
	if err != nil {
		t.Fatalf("failed to read the hand written plist: %v", err)
	}

	want := []string{"/opt/tm/tunnel-manager", "-db", "/opt/tm/tm.db"}

	if !reflect.DeepEqual(arguments, want) {
		t.Errorf("the arguments read back are %q, want %q", arguments, want)
	}
}

// TestProgramArgumentsRefusesWhatItCannotRead holds the rule of the interface:
// a plist that is there and cannot be read is a failure and never an empty
// answer. An empty answer would travel up as an installation with no paths,
// and an uninstall would take the registration away and leave the executable
// and the database behind with nothing naming them.
func TestProgramArgumentsRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name     string
		document string
	}{
		{
			name:     "not xml at all",
			document: "this is not a plist\n",
		},
		{
			name: "no ProgramArguments",
			document: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.github.jollaman999.tunnel-manager</string>
</dict>
</plist>
`,
		},
		{
			name: "an empty ProgramArguments",
			document: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>ProgramArguments</key>
	<array>
	</array>
</dict>
</plist>
`,
		},
		{
			name: "ProgramArguments that is not an array",
			document: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>ProgramArguments</key>
	<string>/usr/local/bin/tunnel-manager</string>
</dict>
</plist>
`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			arguments, err := programArguments([]byte(c.document))
			if err == nil {
				t.Fatalf("it was read as %q, want a failure", arguments)
			}

			t.Log(err)
		})
	}
}

// TestCurrentFromNothing covers the one case that is an answer and not a
// failure: there is no plist, so nothing is registered.
func TestCurrentFromNothing(t *testing.T) {
	_, err := currentFrom(filepath.Join(t.TempDir(), "nothing.plist"))

	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("a missing plist answered %v, want ErrNotInstalled", err)
	}
}

// TestCurrentFromAPlistThatCannotBeRead is the other side of it. The file is
// there, so something is registered, and saying "nothing is installed" here
// would have an uninstall walk away from a service that is still running.
func TestCurrentFromAPlistThatCannotBeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.plist")

	err := os.WriteFile(path, []byte("<plist version=\"1.0\"><dict>\n"), plistMode)
	if err != nil {
		t.Fatalf("failed to write the broken plist: %v", err)
	}

	_, err = currentFrom(path)
	if err == nil {
		t.Fatal("a plist that could not be parsed was read without a failure")
	}

	if errors.Is(err, ErrNotInstalled) {
		t.Errorf("a plist that is there but unreadable answered ErrNotInstalled: %v", err)
	}

	t.Log(err)
}

// TestLaunchctlCommands freezes the commands this package runs as root on a
// Mac. They cannot be run from here, so the argv is what there is to check,
// and it is written out rather than built from the constants for the same
// reason the plist above is.
func TestLaunchctlCommands(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "Start bootstraps the plist into the system domain",
			got:  bootstrapArgs(launchdPlistPath),
			want: []string{
				"bootstrap",
				"system",
				"/Library/LaunchDaemons/io.github.jollaman999.tunnel-manager.plist",
			},
		},
		{
			name: "Stop and Unregister boot the job out of it",
			got:  bootoutArgs(),
			want: []string{"bootout", "system/io.github.jollaman999.tunnel-manager"},
		},
		{
			name: "the wait after either one asks what the state is",
			got:  printArgs(),
			want: []string{"print", "system/io.github.jollaman999.tunnel-manager"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !reflect.DeepEqual(c.got, c.want) {
				t.Errorf("the command is launchctl %s, want launchctl %s",
					strings.Join(c.got, " "), strings.Join(c.want, " "))
			}

			t.Logf("launchctl %s", strings.Join(c.got, " "))
		})
	}
}

// TestServicePID covers the line Start waits for.
//
// The sample is written from launchctl's documented output and was not
// captured from a Mac, so what this holds down is that the reader takes a pid
// where there is one and takes none where there is not - not that macOS words
// it this way. A macOS that words it differently makes Start wait out its
// attempts and report the text, which is the failure that puts it in front of
// somebody who can see the real thing.
func TestServicePID(t *testing.T) {
	running := `system/io.github.jollaman999.tunnel-manager = {
	active count = 1
	path = /Library/LaunchDaemons/io.github.jollaman999.tunnel-manager.plist
	state = running
	program = /usr/local/bin/tunnel-manager
	pid = 4213
	immediate reason = speculative
}
`

	pid, ok := servicePID(running)
	if !ok || pid != 4213 {
		t.Errorf("the pid read out is %d (%t), want 4213", pid, ok)
	}

	notRunning := `system/io.github.jollaman999.tunnel-manager = {
	active count = 0
	path = /Library/LaunchDaemons/io.github.jollaman999.tunnel-manager.plist
	state = not running
	program = /usr/local/bin/tunnel-manager
	last exit code = 1
}
`

	pid, ok = servicePID(notRunning)
	if ok {
		t.Errorf("a job with no process was read as running with pid %d", pid)
	}
}

// TestPlistPathFor holds the rule the interface asks for: a registration that
// is already there is written over where it is, and only a system with none
// gets the default path.
func TestPlistPathFor(t *testing.T) {
	if path := plistPathFor(nil); path != launchdPlistPath {
		t.Errorf("with nothing registered the plist goes to %q, want %q", path, launchdPlistPath)
	}

	elsewhere := &Installed{DefinitionPath: "/Library/LaunchDaemons/somewhere.else.plist"}

	if path := plistPathFor(elsewhere); path != elsewhere.DefinitionPath {
		t.Errorf("over an existing registration the plist goes to %q, want %q", path, elsewhere.DefinitionPath)
	}

	// An Installed carrying no definition path is not a reason to write
	// nowhere: Windows leaves that field empty, and a field that is empty here
	// means the same as having read nothing.
	if path := plistPathFor(&Installed{}); path != launchdPlistPath {
		t.Errorf("with no definition path the plist goes to %q, want %q", path, launchdPlistPath)
	}
}

// TestNewServiceIsLaunchd checks that the init of this file is what the flow
// above ends up with. Without it the package builds for darwin and every
// install on it answers ErrNoBackend.
func TestNewServiceIsLaunchd(t *testing.T) {
	if newService == nil {
		t.Fatal("no backend is registered for darwin")
	}

	svc, err := newService()
	if err != nil {
		t.Fatalf("building the darwin backend failed: %v", err)
	}

	if _, ok := svc.(launchdService); !ok {
		t.Errorf("the darwin backend is %T, want launchdService", svc)
	}
}
