package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// reexecRoundEnv is how the helper below knows which time around it is running.
// It is an environment variable because the environment is what reexec hands
// over, so a value set before the call is read by the image that replaces this
// one. The round number is the whole of what tells the two apart: the program,
// the arguments and the PID are the same by design.
const reexecRoundEnv = "TUNNEL_MANAGER_TEST_REEXEC_ROUND"

// reexecRoundLine is what the helper prints on each round. The PID is read out
// of it by the test, which is the check: exec replaces the image of a process
// rather than starting another one, so both rounds have to report the same PID.
var reexecRoundLine = regexp.MustCompile(`(?m)^round (\d+): pid=(\d+)$`)

// TestReexecRunsThisProgramAgainAsTheSameProcess is the property the restart
// rests on. A child process would be started outside the cgroup of the unit and
// taken down with the one that started it, so what is checked here is not that
// the program ran again but that it ran as this very process.
func TestReexecRunsThisProgramAgainAsTheSameProcess(t *testing.T) {
	if !canReexec() {
		t.Skip("this platform replaces no process image, so there is nothing to run again")
	}

	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("failed to find the test binary: %v", err)
	}

	// The test binary is run again as the helper below. It is this program as
	// far as exec is concerned: an executable that is started with arguments
	// and an environment and that runs itself again with both unchanged.
	cmd := exec.Command(binary, "-test.run=TestReexecHelperProcess", "-test.v")
	cmd.Env = append(os.Environ(), reexecRoundEnv+"=1")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the helper failed: %v\n%s", err, out)
	}

	rounds := reexecRoundLine.FindAllStringSubmatch(string(out), -1)
	if len(rounds) != 2 {
		t.Fatalf("the helper reported %d rounds, want 2\n%s", len(rounds), out)
	}

	if rounds[0][1] != "1" || rounds[1][1] != "2" {
		t.Fatalf("the helper reported rounds %s and %s, want 1 and 2\n%s",
			rounds[0][1], rounds[1][1], out)
	}

	if rounds[0][2] != rounds[1][2] {
		t.Fatalf("the program ran again as PID %s while the first round was PID %s, "+
			"so it was started as another process instead of replacing this one\n%s",
			rounds[1][2], rounds[0][2], out)
	}
}

// TestReexecReportsThatItCannotRunAgainWhereItCannot covers the other build.
// The restart has to be able to tell an operator beforehand that the service
// does not come back on its own, and that answer is canReexec: a build that
// says it cannot and then quietly does nothing would leave the process ended
// with the screen still waiting for it.
func TestReexecReportsThatItCannotRunAgainWhereItCannot(t *testing.T) {
	if canReexec() {
		t.Skip("this platform replaces the process image, which the test above covers")
	}

	err := reexec()
	if err == nil {
		t.Fatal("reexec reported no failure on a platform that cannot run this program again")
	}
}

// TestReexecHelperProcess is not a test. It is the program the test above runs:
// it says which round it is on and under which PID, and the first round runs
// itself again.
func TestReexecHelperProcess(t *testing.T) {
	round := os.Getenv(reexecRoundEnv)
	if round == "" {
		t.Skip("this is the helper of TestReexecRunsThisProgramAgainAsTheSameProcess")
	}

	fmt.Printf("round %s: pid=%d\n", round, os.Getpid())

	if round != "1" {
		return
	}

	// The round is raised before the call, because reexec hands over the
	// environment of this process and the image that replaces it reads the
	// value from there. Left as it was, the two rounds would go on replacing
	// each other.
	err := os.Setenv(reexecRoundEnv, "2")
	if err != nil {
		t.Fatalf("failed to set %s: %v", reexecRoundEnv, err)
	}

	err = reexec()

	// Only a failure comes back from that call. The message is printed rather
	// than reported through t, so that the test that reads this output says
	// what happened even when the helper is a build that cannot run again.
	fmt.Printf("running this program again failed: %v\n", err)

	// A test binary that reports nothing else would exit 0 and leave the test
	// above reading one round out of the output rather than a failure.
	t.Fatalf("running this program again returned: %v", err)
}
