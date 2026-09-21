//go:build darwin

package install

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// lsofCommand is what is run to read the open files of a process.
//
// It is a bare name and not a path on purpose, so it is found on PATH wherever
// this Mac keeps it. macOS ships it in /usr/sbin, which is on the PATH of root,
// but that is not a thing to nail down from here: a machine that keeps it
// elsewhere would then have no way of answering at all.
const lsofCommand = "lsof"

// lsofFieldArguments ask lsof for its machine readable output.
//
// Without them the answer is a table whose last column is the name, and a name
// may hold spaces - /Library/Application Support/tunnel-manager is the default
// data directory on this platform - so the table cannot be split back into
// fields with any confidence. With -F the answer is one field per line, each
// line starting with the letter of the field, and "n" is the name. The output
// still carries the process and descriptor lines, which are ignored here.
//
// None of this was run on a Mac. What is held down instead is the parsing, in
// openFilesFromLsof below, against output of the shape lsof documents; what
// lsof actually prints on macOS is the part that has to be checked there.
var lsofFieldArguments = []string{"-F", "n", "-p"}

// lsofNameField is the letter lsof puts in front of a name.
const lsofNameField = "n"

// openFilesOfPID answers the files process pid holds open.
//
// There is no /proc on this platform, so lsof is asked. It is the same question
// the Linux backend answers by reading /proc/<pid>/fd, and it is asked for the
// same reason: a registration that passes no -db says nothing about which
// database the process opened.
func openFilesOfPID(pid int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()

	arguments := append(append([]string{}, lsofFieldArguments...), fmt.Sprint(pid))

	// Standard error comes back with standard output, because what lsof says
	// about a process it would not look at goes on either one depending on the
	// reason, and what this does with the text is put it in front of a person.
	raw, err := exec.CommandContext(ctx, lsofCommand, arguments...).CombinedOutput()
	output := string(raw)

	if err != nil {
		return nil, fmt.Errorf("%w: %s %s failed: %w: %s",
			ErrOpenFilesUnknown, lsofCommand, strings.Join(arguments, " "), err,
			strings.TrimSpace(output))
	}

	return openFilesFromLsof(output), nil
}

// openFilesFromLsof reads the paths out of what lsof -F n wrote.
func openFilesFromLsof(output string) []string {
	var paths []string

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")

		if !strings.HasPrefix(line, lsofNameField) {
			continue
		}

		path, ok := openFilePath(strings.TrimPrefix(line, lsofNameField))
		if !ok {
			continue
		}

		paths = append(paths, path)
	}

	return sortedUnique(paths)
}

// OpenFiles answers the files the running service has open.
//
// The process id comes from launchd, through the same print this backend waits
// on a start with, so the files read are the ones of the job this uninstall is
// about rather than of another copy of the program.
func (launchdService) OpenFiles() ([]string, error) {
	up, output, err := loaded()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOpenFilesUnknown, err)
	}

	if !up {
		return nil, fmt.Errorf("%w: launchd holds no job under %s, so there is nothing to ask which "+
			"files it has open", ErrOpenFilesUnknown, launchdTarget)
	}

	pid, running := servicePID(output)
	if !running {
		return nil, fmt.Errorf("%w: %s is loaded but no process of it is running. The last launchctl "+
			"print of it was:\n%s", ErrOpenFilesUnknown, launchdTarget, strings.TrimSpace(output))
	}

	return openFilesOfPID(pid)
}
