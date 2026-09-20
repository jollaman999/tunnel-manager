//go:build !windows

package main

import (
	"os"
	"syscall"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"go.uber.org/zap"
)

// warnIfNotPrivileged says so when the process is not root. What follows may
// need it: raising the hard limit on file descriptors, and writing to the
// directories the log file and the configuration live in.
func warnIfNotPrivileged(logger *zap.Logger) {
	if os.Geteuid() == 0 {
		return
	}

	logger.Warn("not running as root",
		logid.StartupNotRoot.Field(),
		zap.Int("euid", os.Geteuid()),
		zap.String("message", "operations that require root privileges may fail, such as raising the max ulimit or writing to system directories"))
}

// checkUlimit raises the number of file descriptors this process may open, as
// far as it is allowed to. Every tunnel holds several of them, so the default
// limit on many systems runs out long before the tunnels do.
//
// The fields of syscall.Rlimit are int64 on some Unix systems and uint64 on
// others, so every one of them is converted before it is used instead of being
// taken for one or the other.
func checkUlimit(logger *zap.Logger) {
	var rLimit syscall.Rlimit
	const desiredCur = uint64(65535)

	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		logger.Warn("error getting rlimit", logid.UlimitReadFailed.Field(), zap.Error(err))
		return
	}

	logger.Info("current ulimit before change",
		logid.UlimitCurrent.Field(),
		zap.Uint64("cur", uint64(rLimit.Cur)),
		zap.Uint64("max", uint64(rLimit.Max)))

	if uint64(rLimit.Max) < desiredCur {
		logger.Warn("max ulimit is low",
			logid.UlimitMaxLow.Field(),
			zap.Uint64("current", uint64(rLimit.Max)),
			zap.Uint64("desired", desiredCur),
			zap.String("message", "tunnel-manager recommends setting max ulimit to more than 65535 for reliable connection management. raising the max ulimit requires root privileges"))
	}

	if uint64(rLimit.Cur) >= desiredCur {
		logger.Info("no need to change ulimit", logid.UlimitChangeNotNeeded.Field())
		return
	}

	// Without root privileges the soft limit can only be raised up to the hard limit.
	newCur := rLimit.Max
	if newCur <= rLimit.Cur {
		logger.Warn("cannot raise the current ulimit any further",
			logid.UlimitRaiseNotPossible.Field(),
			zap.Uint64("current", uint64(rLimit.Cur)),
			zap.Uint64("max", uint64(rLimit.Max)),
			zap.Uint64("desired", desiredCur),
			zap.String("message", "the current ulimit already reached the max ulimit. raising the max ulimit requires root privileges"))
		return
	}

	newLimit := syscall.Rlimit{
		Cur: newCur,
		Max: rLimit.Max,
	}

	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &newLimit)
	if err != nil {
		logger.Warn("failed to change ulimit",
			logid.UlimitChangeFailed.Field(),
			zap.Error(err),
			zap.Uint64("current", uint64(rLimit.Cur)),
			zap.Uint64("max", uint64(rLimit.Max)),
			zap.Uint64("tried", uint64(newCur)))
		return
	}

	logger.Info("successfully changed ulimit",
		logid.UlimitChanged.Field(),
		zap.Uint64("old_limit", uint64(rLimit.Cur)),
		zap.Uint64("new_limit", uint64(newLimit.Cur)))

	if uint64(newLimit.Cur) < desiredCur {
		logger.Warn("ulimit is still lower than the desired value",
			logid.UlimitStillLow.Field(),
			zap.Uint64("current", uint64(newLimit.Cur)),
			zap.Uint64("desired", desiredCur))
	}
}
