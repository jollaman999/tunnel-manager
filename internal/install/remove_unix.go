//go:build !windows

package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// removeExecutable removes the installed executable and says whether it had to
// be left for the next reboot, which here it never does.
//
// Unix removes the name and not the file: a process running from it - including
// this one, when an uninstall removes the binary it is running - keeps the file
// alive through its own open handle until it exits, and nothing about the
// removal waits on that. The answer is therefore always false, and the reason
// this reports it at all is Windows, where it is not: see remove_windows.go.
func removeExecutable(path string) (bool, error) {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("failed to remove the executable %s: %w", path, err)
	}

	return false, nil
}

// RemoveFile removes a file of the installation and says whether it had to be
// left for the next reboot, which here it never does, for the reason
// removeExecutable gives: a file another process holds open is removed all the
// same. The error of os.Remove is answered as it is, so that the caller tells a
// file that was not there from one that could not go.
func RemoveFile(path string) (bool, error) {
	return false, os.Remove(path)
}
