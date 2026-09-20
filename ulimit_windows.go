//go:build windows

package main

import (
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"go.uber.org/zap"
)

// warnIfNotPrivileged says nothing on Windows. The Unix build warns when the
// process is not root, which is about what root may do to the descriptor limit
// and to the system directories. Neither has a counterpart here, so the warning
// would name a privilege that does not decide anything.
func warnIfNotPrivileged(_ *zap.Logger) {}

// checkUlimit does nothing on Windows. It exists so that the startup reads the
// same on every platform.
//
// A Unix process may open only as many file descriptors as RLIMIT_NOFILE
// allows, and every tunnel holds several, so the Unix build raises that limit
// as far as it is permitted to. Windows has no such per process limit to raise:
// handles are bounded by the memory the kernel will spend on them, and there is
// nothing for the process to ask for at startup.
func checkUlimit(logger *zap.Logger) {
	logger.Debug("there is no descriptor limit to raise on this platform", logid.UlimitNotApplicable.Field())
}
