//go:build windows

package crypto

import "os"

// checkKeyFilePermission checks nothing on Windows.
//
// os.Stat there reports no Unix mode: every file reads as 0666, or 0444 when
// the read-only attribute is set, whatever its ACL allows. The check the Unix
// side makes would refuse every key file, including the one this package has
// just created, so it is not made here.
func checkKeyFilePermission(path string, info os.FileInfo) error {
	return nil
}
