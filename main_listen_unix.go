//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// addressInUse says whether a listen failed because another socket holds the
// address.
func addressInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
