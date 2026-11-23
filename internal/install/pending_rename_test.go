package install

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// These hold the reading and the filtering of PendingFileRenameOperations,
// which run on any platform. The value is built the way the registry hands it
// over: UTF-16 strings, each ended by a NUL, and one more NUL at the end.

func multiSZ(values ...string) []uint16 {
	var data []uint16

	for _, value := range values {
		data = append(data, utf16.Encode([]rune(value))...)
		data = append(data, 0)
	}

	return append(data, 0)
}

func decoded(values [][]uint16) []string {
	out := make([]string, 0, len(values))

	for _, value := range values {
		out = append(out, string(utf16.Decode(value)))
	}

	return out
}

const (
	testExecutable = `C:\Program Files\tunnel-manager\tunnel-manager.exe`
	testDataDir    = `C:\ProgramData\tunnel-manager`
)

// observedEntries is shaped after the value on a Windows 10.0.26100 machine
// after an uninstall. Its last pair is what was read there; the three before
// it stand for the entries of Edge, Copilot and the print spooler that were
// there beside it, with paths made up here. One of them renames a file into
// place, which is a pair whose second string is not empty.
var observedEntries = []string{
	`\??\C:\Program Files (x86)\Microsoft\EdgeUpdate\Install\{A1B2}\MicrosoftEdgeUpdate.exe`, "",
	`\??\C:\Windows\SystemTemp\CopilotUpdate\setup.tmp`, `!\??\C:\Program Files\Copilot\setup.exe`,
	`\??\C:\Windows\System32\spool\drivers\x64\3\Old\1\unidrv.dll`, "",
	`*1\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`, "",
}

// TestSplitMultiSZKeepsTheEmptyDestinations is the reading the pairing rests
// on: an empty string in the middle of the value is the destination of a
// deletion, and a reader that dropped it would pair every entry after the
// first deletion with the wrong partner.
func TestSplitMultiSZKeepsTheEmptyDestinations(t *testing.T) {
	values := decoded(splitMultiSZ(multiSZ(observedEntries...)))

	if !slices.Equal(values, observedEntries) {
		t.Fatalf("read back %q, want %q", values, observedEntries)
	}

	if got := splitMultiSZ(multiSZ()); len(got) != 0 {
		t.Errorf("an empty list read back as %q", decoded(got))
	}

	if got := splitMultiSZ(nil); len(got) != 0 {
		t.Errorf("no data read back as %q", decoded(got))
	}

	// A writer that left off the last NUL: the last string is still read.
	data := multiSZ(`\??\C:\a`, "", `\??\C:\b`, `\??\C:\c`)
	data = data[:len(data)-2]

	if got := decoded(splitMultiSZ(data)); !slices.Equal(got, []string{`\??\C:\a`, "", `\??\C:\b`, `\??\C:\c`}) {
		t.Errorf("a value without its last NUL read back as %q", got)
	}
}

// TestJoinMultiSZIsWhatWasRead holds the writing: what is written back for
// the entries that are kept is the very data they were read as.
func TestJoinMultiSZIsWhatWasRead(t *testing.T) {
	data := multiSZ(observedEntries...)

	if got := joinMultiSZ(splitMultiSZ(data)); !slices.Equal(got, data) {
		t.Fatalf("written back as %v, want %v", got, data)
	}
}

// TestDropDeletionsOfTakesOutOnlyThisInstallation is the case that failed on
// Windows: an uninstall handed the executable to the next boot, -install put a
// new one at the same path, and the boot deleted it. The deletion of the
// executable goes, with the mark in front of it, and the entries of the other
// programs stay as they were, in the order they were in.
func TestDropDeletionsOfTakesOutOnlyThisInstallation(t *testing.T) {
	kept, dropped, err := dropDeletionsOf(splitMultiSZ(multiSZ(observedEntries...)), testExecutable, testDataDir)
	if err != nil {
		t.Fatalf("filtering failed: %v", err)
	}

	if !slices.Equal(dropped, []string{testExecutable}) {
		t.Errorf("took out %q, want the executable alone", dropped)
	}

	if got, want := decoded(kept), observedEntries[:6]; !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
}

// TestDropDeletionsOfReadsEverySpellingOfThePath covers the ways the same
// file is written into the value: any case, the mark or none, forward slashes,
// and a share.
func TestDropDeletionsOfReadsEverySpellingOfThePath(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		executable string
		dataDir    string
		dropped    bool
	}{
		{"the mark seen on Windows", `*1\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			testExecutable, testDataDir, true},
		{"a mark of more digits", `*12\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`,
			testExecutable, testDataDir, true},
		{"no mark", `\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`, testExecutable, testDataDir, true},
		{"another case", `\??\c:\PROGRAM FILES\Tunnel-Manager\TUNNEL-MANAGER.EXE`,
			testExecutable, testDataDir, true},
		{"forward slashes in the plan", `\??\C:\tm\tunnel-manager.exe`, `C:/tm/tunnel-manager.exe`, `C:/tm/data`, true},
		{"a share", `\??\UNC\build\tm\tunnel-manager.exe`, `\\build\tm\tunnel-manager.exe`, `\\build\tm\data`, true},
		{"the data directory itself", `\??\C:\ProgramData\tunnel-manager`, testExecutable, testDataDir, true},
		{"the data directory with a trailing backslash", `\??\C:\ProgramData\tunnel-manager`,
			testExecutable, testDataDir + `\`, true},
		{"a file in the data directory", `\??\C:\ProgramData\tunnel-manager\logs\tunnel-manager.log`,
			testExecutable, testDataDir, true},
		{"a directory whose name starts with the data directory's", `\??\C:\ProgramData\tunnel-manager-old\x.log`,
			testExecutable, testDataDir, false},
		{"another file beside the executable", `\??\C:\Program Files\tunnel-manager\readme.txt`,
			testExecutable, testDataDir, false},
		{"a path that ends where the executable's does", `\??\D:\Program Files\tunnel-manager\tunnel-manager.exe`,
			testExecutable, testDataDir, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values := splitMultiSZ(multiSZ(c.source, ""))

			kept, dropped, err := dropDeletionsOf(values, c.executable, c.dataDir)
			if err != nil {
				t.Fatalf("filtering failed: %v", err)
			}

			if (len(dropped) == 1) != c.dropped || len(kept) != map[bool]int{true: 0, false: 2}[c.dropped] {
				t.Fatalf("%s: took out %q and kept %q, want it taken out: %v",
					c.source, dropped, decoded(kept), c.dropped)
			}
		})
	}
}

// TestDropDeletionsOfLeavesARename keeps a pair that moves a file onto or away
// from the executable. Only a deletion is what an uninstall hands over, and a
// rename is somebody else's.
func TestDropDeletionsOfLeavesARename(t *testing.T) {
	entries := []string{
		`\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`, `\??\C:\elsewhere\tunnel-manager.exe`,
		`\??\C:\staging\tunnel-manager.exe`, `!\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`,
	}

	kept, dropped, err := dropDeletionsOf(splitMultiSZ(multiSZ(entries...)), testExecutable, testDataDir)
	if err != nil {
		t.Fatalf("filtering failed: %v", err)
	}

	if len(dropped) != 0 || !slices.Equal(decoded(kept), entries) {
		t.Fatalf("took out %q and kept %q, want every rename kept", dropped, decoded(kept))
	}
}

// TestDropDeletionsOfRefusesAValueThatIsNotPairs leaves a value it cannot
// read as pairs alone. Written back with the pairing guessed at, every entry
// after the odd one would turn into another operation.
func TestDropDeletionsOfRefusesAValueThatIsNotPairs(t *testing.T) {
	values := splitMultiSZ(multiSZ(`\??\C:\a`, "", `*1\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`))

	_, _, err := dropDeletionsOf(values, testExecutable, testDataDir)
	if err == nil || !strings.Contains(err.Error(), "not pairs") {
		t.Fatalf("a value of three strings was answered %v, want a refusal", err)
	}
}

// TestDropDeletionsOfCarriesAnUnreadableNameOverAsItWas holds the entries of
// other programs to the very units they were read as. A name with a lone
// surrogate in it does not survive a trip through a Go string, and it would be
// written back as a name of another file.
func TestDropDeletionsOfCarriesAnUnreadableNameOverAsItWas(t *testing.T) {
	lone := append(utf16.Encode([]rune(`\??\C:\odd\`)), 0xD800, 'x')
	ours := utf16.Encode([]rune(`*1\??\C:\Program Files\tunnel-manager\tunnel-manager.exe`))

	var data []uint16

	for _, value := range [][]uint16{lone, {}, ours, {}} {
		data = append(data, value...)
		data = append(data, 0)
	}

	data = append(data, 0)

	kept, dropped, err := dropDeletionsOf(splitMultiSZ(data), testExecutable, testDataDir)
	if err != nil {
		t.Fatalf("filtering failed: %v", err)
	}

	if len(dropped) != 1 {
		t.Fatalf("took out %q, want the executable", dropped)
	}

	want := append(append([]uint16{}, lone...), 0, 0, 0)

	if got := joinMultiSZ(kept); !slices.Equal(got, want) {
		t.Fatalf("written back as %v, want %v", got, want)
	}
}
