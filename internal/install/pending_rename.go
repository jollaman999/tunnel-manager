package install

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// The deletions Windows carries out at the next boot, read and filtered here
// apart from the registry so that the reading is held by tests on any
// platform. pending_rename_windows.go is what reads and writes the value.
//
// The value is PendingFileRenameOperations under
// HKLM\SYSTEM\CurrentControlSet\Control\Session Manager, a REG_MULTI_SZ that
// MoveFileEx with MOVEFILE_DELAY_UNTIL_REBOOT appends to and the session
// manager carries out and clears while the next boot comes up. Its strings go
// in pairs: the file, then where it goes, and an empty second string is a
// deletion. Both are NT paths, \??\C:\..., and the first one may carry a mark
// in front of that: on Windows 10.0.26100 the executable an uninstall handed
// over read *1\??\C:\Program Files\tunnel-manager\tunnel-manager.exe. What the
// mark means is not documented, so it is only read past and never written.
//
// Other programs keep their own entries in the same value - an update of Edge
// or of Copilot, the print spooler - and they are carried over untouched. That
// is why the strings are kept as the UTF-16 they were read as rather than
// turned into Go strings and back: a name that does not decode cleanly would
// otherwise come back as another name.

// pendingRenameKey and pendingRenameValue name where the deletions are.
const (
	pendingRenameKey   = `SYSTEM\CurrentControlSet\Control\Session Manager`
	pendingRenameValue = "PendingFileRenameOperations"
)

// splitMultiSZ splits the data of a REG_MULTI_SZ into its strings.
//
// Every string ends in a NUL and the list ends in one more, so a well formed
// value is the strings followed by an empty one. The empty strings between
// them are kept: in this value they are the destinations of the deletions, and
// dropping them would pair every entry after the first deletion with the wrong
// partner. A value whose writer left off the last NUL is read the same way,
// except where its last string is empty: that one cannot be told from the end
// of the list, and the count of strings then comes out odd.
func splitMultiSZ(data []uint16) [][]uint16 {
	if len(data) > 0 && data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}

	var values [][]uint16

	start := 0

	for i, unit := range data {
		if unit == 0 {
			values = append(values, data[start:i])
			start = i + 1
		}
	}

	if start < len(data) {
		values = append(values, data[start:])
	}

	return values
}

// joinMultiSZ is splitMultiSZ the other way round: every string with its NUL,
// and the NUL that ends the list.
func joinMultiSZ(values [][]uint16) []uint16 {
	var data []uint16

	for _, value := range values {
		data = append(data, value...)
		data = append(data, 0)
	}

	return append(data, 0)
}

// pendingTarget is the path the first string of a pair names, as a Win32 path
// a path of this program can be compared with.
func pendingTarget(source []uint16) string {
	path := string(utf16.Decode(source))

	// The mark in front: a star and the digits after it.
	if strings.HasPrefix(path, "*") {
		end := 1

		for end < len(path) && path[end] >= '0' && path[end] <= '9' {
			end++
		}

		path = path[end:]
	}

	path = strings.TrimPrefix(path, `\??\`)

	if rest, found := strings.CutPrefix(path, `UNC\`); found {
		path = `\\` + rest
	}

	return path
}

// comparablePath spells a Windows path the one way two spellings of it are
// compared in: backslashes, no trailing one, and the case of every letter
// folded, since NTFS names that differ in case alone are the same file.
func comparablePath(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)

	if len(path) > 3 {
		path = strings.TrimRight(path, `\`)
	}

	return strings.ToLower(path)
}

// dropDeletionsOf takes out of the value the deletions of the executable, of
// the data directory and of everything under it, and keeps every other pair
// as it was read. It answers the value to write back and the paths whose
// deletions were taken out.
//
// Those are what an uninstall hands to the next boot on this platform: the
// executable the process was running from, and what was left of a data
// directory some other handle still held. An install that puts files at those
// paths before that boot would have them deleted by it, which is how a
// reinstall before a restart ended in a service that could not start
// ("The system cannot find the file specified", event 7000).
//
// Only deletions are taken out, a pair whose second string is empty. A rename
// onto or away from one of these paths is somebody else's and is kept. A value
// with an odd number of strings cannot be read as pairs at all, and is refused
// rather than written back with the pairing guessed at.
func dropDeletionsOf(values [][]uint16, executable string, dataDir string) ([][]uint16, []string, error) {
	if len(values)%2 != 0 {
		return nil, nil, fmt.Errorf("%s holds %d strings, which are not pairs, so it is left as it is",
			pendingRenameValue, len(values))
	}

	exe := comparablePath(executable)
	dir := comparablePath(dataDir)

	ours := func(target string) bool {
		path := comparablePath(target)

		switch {
		case exe != "" && path == exe:
			return true
		case dir != "" && (path == dir || strings.HasPrefix(path, dir+`\`)):
			return true
		}

		return false
	}

	kept := make([][]uint16, 0, len(values))

	var dropped []string

	for i := 0; i < len(values); i += 2 {
		source, destination := values[i], values[i+1]
		target := pendingTarget(source)

		if len(destination) == 0 && ours(target) {
			dropped = append(dropped, target)

			continue
		}

		kept = append(kept, source, destination)
	}

	return kept, dropped, nil
}
