//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestCheckUlimitNeverLeavesThisProcessWithFewerDescriptors runs the check
// against the limits this process was started with, whatever they are. The
// check is allowed to raise the soft limit and nothing else: a run that lowered
// it, or that left it below what it could have reached, would cost the process
// descriptors that every tunnel needs.
func TestCheckUlimitNeverLeavesThisProcessWithFewerDescriptors(t *testing.T) {
	var before syscall.Rlimit

	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &before)
	if err != nil {
		t.Fatalf("failed to read the limits of this process: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)

	checkUlimit(zap.New(core))

	var after syscall.Rlimit

	err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &after)
	if err != nil {
		t.Fatalf("failed to read the limits back: %v", err)
	}

	if uint64(after.Cur) < uint64(before.Cur) {
		t.Fatalf("the soft limit was lowered from %d to %d", uint64(before.Cur), uint64(after.Cur))
	}
	if after.Max != before.Max {
		t.Fatalf("the hard limit was changed from %d to %d", uint64(before.Max), uint64(after.Max))
	}

	// The fields of syscall.Rlimit are int64 on some Unix systems and uint64
	// on others, so they are converted rather than compared as they come.
	reachable := uint64(after.Max)
	if reachable > 65535 {
		reachable = 65535
	}
	if uint64(after.Cur) < reachable {
		t.Fatalf("the soft limit was left at %d while %d was within reach", uint64(after.Cur), reachable)
	}

	// What the process started with is reported whatever is done about it, so
	// that a deployment running out of descriptors can be read back from the log.
	found := false

	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, "current ulimit before change") {
			found = true
		}
	}

	if !found {
		t.Fatalf("the limits this process started with were not reported, logged: %v", logs.AllUntimed())
	}
}

// The soft and the hard limit of a process cannot be moved around inside a test
// binary without every other test having to live with the result, and the hard
// limit cannot be raised back at all without privileges. checkUlimit is
// therefore run in a child process that sets the limits it is meant to see.
const (
	ulimitHelperEnv = "TUNNEL_MANAGER_ULIMIT_HELPER"
	ulimitLogPrefix = "ULIMIT-LOG "
	ulimitNowPrefix = "ULIMIT-NOW "
)

// TestCheckUlimitHelperProcess is the child of the checkUlimit tests. It is a
// test only so that it can be reached through the test binary, and it does
// nothing when it is run as part of the normal suite.
func TestCheckUlimitHelperProcess(t *testing.T) {
	limits := os.Getenv(ulimitHelperEnv)
	if limits == "" {
		t.Skip("this test is the child process of the checkUlimit tests")
	}

	// The numbers are read straight into the fields of syscall.Rlimit. Those
	// are int64 on some Unix systems and uint64 on others, and Sscanf takes
	// either, while a variable declared here would have to pick one.
	var limit syscall.Rlimit

	_, err := fmt.Sscanf(limits, "%d,%d", &limit.Cur, &limit.Max)
	if err != nil {
		t.Fatalf("failed to read the limits to set from %q: %v", limits, err)
	}

	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit)
	if err != nil {
		t.Skipf("failed to set the limits the case needs: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)

	checkUlimit(zap.New(core))

	for _, entry := range logs.All() {
		line := entry.Message
		for key, value := range entry.ContextMap() {
			line += fmt.Sprintf(" %s=%v", key, value)
		}
		fmt.Println(ulimitLogPrefix + line)
	}

	var reached syscall.Rlimit

	err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &reached)
	if err != nil {
		t.Fatalf("failed to read back the limits: %v", err)
	}

	fmt.Printf("%s%d,%d\n", ulimitNowPrefix, uint64(reached.Cur), uint64(reached.Max))
}

// ulimitOutcome holds what checkUlimit logged in the child process and the
// limits the child was left with.
type ulimitOutcome struct {
	lines []string
	cur   uint64
	max   uint64
}

func (o ulimitOutcome) logged(text string) bool {
	for _, line := range o.lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// runCheckUlimitWith runs checkUlimit in a child process whose descriptor
// limits are the given ones.
func runCheckUlimitWith(t *testing.T, cur, max uint64) ulimitOutcome {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestCheckUlimitHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d,%d", ulimitHelperEnv, cur, max))

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the child process failed: %v, output:\n%s", err, output)
	}

	if strings.Contains(string(output), "failed to set the limits the case needs") {
		t.Skipf("this process may not set the limits the case needs, output:\n%s", output)
	}

	outcome := ulimitOutcome{}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, ulimitLogPrefix):
			outcome.lines = append(outcome.lines, strings.TrimPrefix(line, ulimitLogPrefix))
		case strings.HasPrefix(line, ulimitNowPrefix):
			_, err := fmt.Sscanf(strings.TrimPrefix(line, ulimitNowPrefix), "%d,%d", &outcome.cur, &outcome.max)
			if err != nil {
				t.Fatalf("failed to read the limits the child was left with from %q: %v", line, err)
			}
		}
	}

	if len(outcome.lines) == 0 {
		t.Fatalf("the child process logged nothing, output:\n%s", output)
	}

	return outcome
}

// TestCheckUlimitRaisesTheSoftLimitUpToTheHardLimit pins that a process that
// starts with room to grow ends up with more descriptors than it was given.
// Every tunnel holds descriptors, so a soft limit left where it was caps how
// many tunnels can be up at once.
func TestCheckUlimitRaisesTheSoftLimitUpToTheHardLimit(t *testing.T) {
	outcome := runCheckUlimitWith(t, 256, 1024)

	if outcome.cur != 1024 {
		t.Fatalf("the soft limit was left at %d, want it raised to the hard limit of 1024", outcome.cur)
	}
	if !outcome.logged("successfully changed ulimit") {
		t.Fatalf("the change was not reported, logged:\n%s", strings.Join(outcome.lines, "\n"))
	}

	// The hard limit is below what the service wants, and raising it needs
	// privileges, so both facts have to reach the operator.
	if !outcome.logged("max ulimit is low") {
		t.Fatalf("a hard limit below the desired one was not reported, logged:\n%s", strings.Join(outcome.lines, "\n"))
	}
	if !outcome.logged("ulimit is still lower than the desired value") {
		t.Fatalf("a soft limit that is still too low was not reported, logged:\n%s", strings.Join(outcome.lines, "\n"))
	}
}

// TestCheckUlimitLeavesTheLimitAloneWhenItIsAlreadyAtTheHardLimit pins that a
// process that cannot raise anything says so and goes on. Raising the hard
// limit needs privileges, and 9322d13 stopped the startup from requiring them.
func TestCheckUlimitLeavesTheLimitAloneWhenItIsAlreadyAtTheHardLimit(t *testing.T) {
	outcome := runCheckUlimitWith(t, 512, 512)

	if outcome.cur != 512 || outcome.max != 512 {
		t.Fatalf("the limits were left at %d/%d, want 512/512", outcome.cur, outcome.max)
	}
	if !outcome.logged("cannot raise the current ulimit any further") {
		t.Fatalf("a limit that cannot be raised was not reported, logged:\n%s", strings.Join(outcome.lines, "\n"))
	}
	if outcome.logged("successfully changed ulimit") {
		t.Fatalf("a change was reported although nothing could be changed, logged:\n%s",
			strings.Join(outcome.lines, "\n"))
	}
}

func TestCheckUlimitChangesNothingWhenTheSoftLimitIsAlreadyEnough(t *testing.T) {
	outcome := runCheckUlimitWith(t, 70000, 70000)

	if outcome.cur != 70000 {
		t.Fatalf("the soft limit was moved to %d although it was already enough", outcome.cur)
	}
	if !outcome.logged("no need to change ulimit") {
		t.Fatalf("a limit that is already enough was not reported as such, logged:\n%s",
			strings.Join(outcome.lines, "\n"))
	}
	if outcome.logged("max ulimit is low") {
		t.Fatalf("a hard limit above the desired one was reported as low, logged:\n%s",
			strings.Join(outcome.lines, "\n"))
	}
}
