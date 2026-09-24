//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// addressInUse says whether a listen failed because another socket holds the
// address.
//
// Winsock reports it as WSAEADDRINUSE. syscall.EADDRINUSE is a value the syscall
// package made up for Windows, which no socket call returns, so testing for it
// would never find a taken port here.
func addressInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE)
}
