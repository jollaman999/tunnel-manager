//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// portUnavailable says whether a listen failed because another socket holds the
// address, so that another port may be tried. EACCES is left out: here it means
// a port below 1024 without the privilege for it, which is a setting to fix.
func portUnavailable(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
