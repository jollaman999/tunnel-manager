package install

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// atATerminal makes the question think there is somebody to answer it.
//
// A test has no terminal - go test runs with its standard input on /dev/null or
// on a pipe - so the one part of this that cannot be driven as it is in the
// field is the check itself, which is why it is a variable. What is driven
// through it is everything after: the question, the answer and what is made of
// it.
func atATerminal(t *testing.T) {
	t.Helper()

	was := inputIsTerminal
	inputIsTerminal = func(io.Reader) bool { return true }

	t.Cleanup(func() {
		inputIsTerminal = was
	})
}

// refusingReader is an input that fails if anything is read from it. It stands
// in for the standard input of a run that must not be read at all.
type refusingReader struct {
	t *testing.T
}

func (r refusingReader) Read([]byte) (int, error) {
	r.t.Error("the question was read from the input although it was not asked")

	return 0, errors.New("this input must not be read")
}

// TestAskToRemoveTakesYesAndNothingElse covers the answers. Anything that is
// not yes is a no, including the empty line that pressing enter sends and the
// end of the input, because what follows a yes cannot be taken back.
func TestAskToRemoveTakesYesAndNothingElse(t *testing.T) {
	cases := []struct {
		answer string
		want   bool
	}{
		{answer: "y\n", want: true},
		{answer: "Y\n", want: true},
		{answer: "yes\n", want: true},
		{answer: " yes \n", want: true},
		{answer: "YES\n", want: true},
		// No newline at all: the answer typed and the input closed.
		{answer: "y", want: true},
		{answer: "n\n", want: false},
		{answer: "no\n", want: false},
		{answer: "N\n", want: false},
		// Enter on its own, which is the whole reason the question reads [y/N].
		{answer: "\n", want: false},
		// Nothing at all, which is the input ending before anything was typed.
		{answer: "", want: false},
		{answer: "ye\n", want: false},
		{answer: "yes please\n", want: false},
	}

	for _, c := range cases {
		t.Run(strings.TrimSpace(c.answer), func(t *testing.T) {
			atATerminal(t)

			var out strings.Builder

			goAhead, err := askToRemove("the plan\n", false, strings.NewReader(c.answer), &out)
			if err != nil {
				t.Fatalf("asking failed: %v", err)
			}

			if goAhead != c.want {
				t.Errorf("the answer %q was read as %v, want %v", c.answer, goAhead, c.want)
			}

			if !strings.Contains(out.String(), "[y/N]") {
				t.Errorf("the question does not say what the default is:\n%s", out.String())
			}

			t.Logf("%q answered:\n%s", c.answer, out.String())
		})
	}
}

// TestAskToRemoveWithAssumeYes covers -y: the list is still written out, and
// nothing is read from the input.
//
// The list is written because it is the record of what a run that removed an
// installation actually removed, and a script that passes -y is exactly the run
// nobody was watching.
func TestAskToRemoveWithAssumeYes(t *testing.T) {
	var out strings.Builder

	goAhead, err := askToRemove("the plan\n", true, refusingReader{t: t}, &out)
	if err != nil {
		t.Fatalf("asking with -y failed: %v", err)
	}

	if !goAhead {
		t.Error("-y did not go ahead")
	}

	if !strings.Contains(out.String(), "the plan") {
		t.Errorf("-y did not write what was about to be removed:\n%s", out.String())
	}

	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("-y asked the question anyway:\n%s", out.String())
	}

	t.Logf("with -y:\n%s", out.String())
}

// TestAskToRemoveWithoutATerminalIsRefused covers the run nobody is watching: a
// pipe, a cron job, a script.
//
// It is refused rather than answered. Taking the end of the input for a no
// would be a removal that quietly does nothing, and taking it for a yes would
// remove an installation nobody agreed to remove. This is the one case that
// uses the real check rather than the one the other tests put in its place: an
// os.Pipe is a file and not a character device, which is what a run from a
// script has.
func TestAskToRemoveWithoutATerminalIsRefused(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to make a pipe: %v", err)
	}

	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	// There is even an answer waiting on it. It still must not be read.
	_, err = writer.WriteString("y\n")
	if err != nil {
		t.Fatalf("failed to write the answer into the pipe: %v", err)
	}

	var out strings.Builder

	goAhead, err := askToRemove("the plan\n", false, reader, &out)
	if err == nil {
		t.Fatal("the uninstall went ahead with nobody to ask")
	}

	if goAhead {
		t.Error("the uninstall was allowed to go ahead")
	}

	// The list is written all the same, so that a script's output says what the
	// run would have removed.
	if !strings.Contains(out.String(), "the plan") {
		t.Errorf("what would have been removed was not written:\n%s", out.String())
	}

	if !strings.Contains(err.Error(), "-y") {
		t.Errorf("the refusal does not say how to run it anyway: %v", err)
	}

	t.Logf("refused as it should: %v", err)
}

// TestInputIsTerminal covers the check itself against the two kinds of input a
// test can make. A terminal cannot be made here, which is why the answers above
// are driven through the variable; what can be held down is that the things
// that are not terminals are not taken for one.
func TestInputIsTerminal(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to make a pipe: %v", err)
	}

	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("failed to open %s: %v", os.DevNull, err)
	}

	t.Cleanup(func() {
		_ = file.Close()
	})

	cases := []struct {
		name string
		in   io.Reader
	}{
		{name: "a pipe, which is what a script gives", in: reader},
		{name: "a reader that is not a file at all", in: strings.NewReader("y\n")},
		{name: "nothing", in: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if inputIsTerminal(c.in) {
				t.Errorf("%T was taken for a terminal", c.in)
			}
		})
	}

	// /dev/null is a character device and so passes this check, which is what
	// the check costs: it says there is something that could be typed at, not
	// that anybody is typing. What follows it is a read that ends at once and
	// is taken as a no, which is the safe end of that mistake.
	t.Logf("%s is a character device: %v", os.DevNull, inputIsTerminal(file))
}
