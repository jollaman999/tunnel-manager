package install

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// answerYes are the answers that mean yes. Everything else means no, including
// the empty line, which is what pressing enter sends: the question is asked
// before steps that cannot be taken back, so the answer that does nothing is
// the one that is easy to give by accident.
var answerYes = map[string]bool{"y": true, "yes": true}

// inputIsTerminal says whether the answer can be typed at all.
//
// It is a variable so that a test can drive the question with a reader of its
// own, which is the only way to cover the answers: a test has no terminal.
//
// A character device is what a terminal is and a pipe or a file is not, which
// is the same rule the shell uses to tell an interactive run from one inside a
// script. Anything that is not a file at all - a strings.Reader, a pipe the
// test made - is not a terminal either.
var inputIsTerminal = func(in io.Reader) bool {
	file, ok := in.(*os.File)
	if !ok {
		return false
	}

	info, err := file.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// askToRemove writes what the uninstall is about to remove and answers whether
// it may go ahead.
//
// It is asked before anything is stopped. The list is read off a service that
// is still running - which files the process has open is where the database
// comes from - so the order is read, ask, and only then stop and remove.
func askToRemove(plan string, assumeYes bool, in io.Reader, out io.Writer) (bool, error) {
	_, err := io.WriteString(out, plan)
	if err != nil {
		return false, fmt.Errorf("failed to write what the uninstall was about to remove: %w", err)
	}

	if assumeYes {
		_, err = io.WriteString(out, "-y was given, so this was not asked.\n")
		if err != nil {
			return false, fmt.Errorf("failed to write what the uninstall was about to remove: %w", err)
		}

		return true, nil
	}

	if !inputIsTerminal(in) {
		// Refused rather than taken for a yes or for a no. A run from a script
		// or over a pipe has nobody at the keyboard, and an uninstall that
		// removed an installation because nothing answered would be doing the
		// one thing this question is here to prevent. -y is how a script says
		// it has already decided.
		return false, errors.New("the answer to this cannot be read: the standard input of this process is " +
			"not a terminal, so there is nobody to ask. Run it again with -y if this is what you want")
	}

	_, err = io.WriteString(out, "Remove all of this? [y/N]: ")
	if err != nil {
		return false, fmt.Errorf("failed to ask whether to go ahead: %w", err)
	}

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("failed to read the answer: %w", err)
	}

	answer := strings.ToLower(strings.TrimSpace(line))

	return answerYes[answer], nil
}

// removalPlanText is what the question above is asked about: every path this
// uninstall would remove, spelled out.
//
// The paths are written out rather than summed up as "the installation",
// because what an operator has to be able to see is whether the installation
// about to go is the one they meant. The whole point of asking is that a -db or
// a registration pointing somewhere unexpected is caught by the person reading,
// and a path they cannot see cannot be caught.
func removalPlanText(removed Removed, files []installedFile, registered bool, purge bool) string {
	lines := planLines(removed, files, registered, purge)
	width := 0

	for _, line := range lines {
		if len(line.Path) > width {
			width = len(line.Path)
		}
	}

	b := &strings.Builder{}

	b.WriteString(serviceName + " uninstall will remove:\n")

	for _, line := range lines {
		if line.Path == "" {
			fmt.Fprintf(b, "  %s\n", line.What)
			continue
		}

		fmt.Fprintf(b, "  %-*s  %s\n", width, line.Path, line.What)
	}

	if purge {
		// Said apart from the list and in full. Everything else an uninstall
		// removes can be put back by installing again or restoring a backup;
		// the key cannot, and the database backup that is left is of no use
		// without it.
		fmt.Fprintf(b, "\n-purge removes the key the stored SSH passwords are sealed with. Without that key\n"+
			"the passwords cannot be read again, from a backup of the database or from anywhere\n"+
			"else. Every host would have to be given its password again.\n")
	} else if removed.DataDir != "" {
		fmt.Fprintf(b, "\nThe data in %s is kept. -purge is what removes it.\n", removed.DataDir)
	}

	return b.String()
}

// planLines are the entries of that list, in the order they are shown: what
// holds the registration, then the executable, then the data.
func planLines(removed Removed, files []installedFile, registered bool, purge bool) []installedFile {
	var lines []installedFile

	switch {
	case removed.Definition != "":
		lines = append(lines, installedFile{Path: removed.Definition, What: "the service registration"})
	case registered:
		// Windows keeps the registration in the registry under the service
		// name, so there is no path to show for it.
		lines = append(lines, installedFile{What: "the registration of " + serviceName + " with the service manager"})
	}

	if removed.ExecutablePath != "" {
		lines = append(lines, installedFile{Path: removed.ExecutablePath, What: "the executable"})
	}

	if !purge {
		return lines
	}

	if removed.DataDir != "" {
		lines = append(lines, installedFile{Path: removed.DataDir, What: "the data directory, with everything under it:"})
	}

	return append(lines, files...)
}
