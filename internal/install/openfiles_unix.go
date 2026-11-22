//go:build !windows

package install

import (
	"sort"
	"strings"
)

// notAFilePrefixes are the things a descriptor points at that are not a file
// this installation owns.
//
// The first three are what Linux writes for a descriptor that has no path -
// they are not absolute paths at all, so they are already refused by the check
// below - and the rest are paths that do exist and still name nothing of ours:
// the terminal or /dev/null the service was started with, and the kernel's own
// trees, which every process has something open in.
var notAFilePrefixes = []string{
	"socket:",
	"anon_inode:",
	"pipe:",
	"/dev/",
	"/proc/",
	"/sys/",
}

// deletedSuffix is what Linux appends to the target of a descriptor whose file
// has already been unlinked. There is nothing at that path to remove, and the
// path itself may since have been taken by another file, so such a descriptor
// is dropped rather than read as the file it used to be.
const deletedSuffix = " (deleted)"

// openFilePath answers the path a descriptor names and whether it is a file
// that may be considered part of this installation.
func openFilePath(target string) (string, bool) {
	target = strings.TrimSpace(target)

	if target == "" || strings.HasSuffix(target, deletedSuffix) {
		return "", false
	}

	// Anything that is not an absolute path is not a file: a socket, a pipe and
	// an event descriptor are all written as a kind and a number.
	if !strings.HasPrefix(target, "/") {
		return "", false
	}

	for _, prefix := range notAFilePrefixes {
		if strings.HasPrefix(target, prefix) {
			return "", false
		}
	}

	return target, true
}

// sortedUnique is how a list of open files is answered: in one order, with each
// path once. A process has the same file open on several descriptors as a
// matter of course, and the order the descriptors are listed in is the order
// they were opened, which says nothing to anybody reading the list.
func sortedUnique(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	unique := make([]string, 0, len(paths))

	for _, path := range paths {
		if seen[path] {
			continue
		}

		seen[path] = true

		unique = append(unique, path)
	}

	sort.Strings(unique)

	return unique
}
