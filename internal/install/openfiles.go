package install

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrOpenFilesUnknown is what a backend answers when the files the running
// service has open cannot be read.
//
// It is a value and not a failure of the uninstall, because there are ordinary
// reasons for it: the service is not running, or this is a platform where the
// question cannot be asked at all (see the Windows backend). What the caller
// does with it depends on what it was going to do with the answer - a removal
// that keeps the data goes on without it, and a -purge stops, because the one
// thing that must not happen is guessing a directory and removing it.
var ErrOpenFilesUnknown = errors.New("the files the running service has open could not be read")

// The two files SQLite keeps beside the database in write ahead log mode. The
// database of this installation is opened in that mode
// (internal/database/database.go, sqliteDSN), so a running process holds all
// three of them open, and that is what tells the database apart from every
// other file it has open.
const (
	walSuffix = "-wal"
	shmSuffix = "-shm"
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

// databaseAmong picks the database of this installation out of the files the
// running service has open.
//
// It is the file that has its own write ahead log open beside it. Nothing else
// about the name is read: -db may point at a file called anything at all, and a
// rule that looked for an extension would answer with confidence for a path the
// operator chose to spell differently. The write ahead log, on the other hand,
// is SQLite's own doing and carries the name of the database with -wal after
// it, so the pair is evidence rather than a guess.
//
// More than one such pair is not answered either. Nothing in this installation
// opens a second database, so it would mean the process is not the one this
// removal is about, and picking one of them is exactly the guess that removes
// somebody else's data.
func databaseAmong(files []string) (string, error) {
	open := make(map[string]bool, len(files))
	for _, file := range files {
		open[file] = true
	}

	var found []string

	for _, file := range files {
		if strings.HasSuffix(file, walSuffix) || strings.HasSuffix(file, shmSuffix) {
			continue
		}

		if open[file+walSuffix] {
			found = append(found, file)
		}
	}

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("%w: none of the files it has open is a database with its "+
			"%s beside it. It has these open: %s",
			ErrOpenFilesUnknown, walSuffix, openFilesText(files))
	default:
		return "", fmt.Errorf("%w: it has more than one database open, so which one this "+
			"installation is cannot be said: %s",
			ErrOpenFilesUnknown, strings.Join(found, ", "))
	}
}

// openFilesText names the files for a message. An empty list is written out as
// such, or the message would end in a colon and nothing.
func openFilesText(files []string) string {
	if len(files) == 0 {
		return "no files at all"
	}

	return strings.Join(files, ", ")
}
