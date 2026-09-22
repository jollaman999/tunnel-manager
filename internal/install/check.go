package install

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// checkTimeout bounds a check of the latest release.
//
// It is far shorter than fetchTimeout because nothing is downloaded: what is
// read is one JSON answer of a few kilobytes. A check runs on a timer with
// nobody watching it, so one that hangs must give up while the next one is
// still an hour away rather than pile up behind it.
const checkTimeout = 30 * time.Second

// Latest is what the newest release says about itself, without anything being
// downloaded.
type Latest struct {
	// Tag is the release tag as GitHub gave it, "v3.7.4" and not "3.7.4". It is
	// kept as it was written so that what is shown is what the release page
	// shows.
	Tag string
	// Newer says the tag names a version after the one asked about. It is
	// false where the versions compare the other way and false where they
	// cannot be compared at all: see Comparable.
	Newer bool
	// Comparable says the two versions were both read as numbers. Where it is
	// false, Newer says nothing, and what a screen shows is the two versions
	// side by side rather than an answer about them.
	//
	// It is a field of its own because "not newer" and "cannot tell" lead to
	// different things: an install that runs on the first would run on the
	// second too, and the second is exactly where it must not.
	Comparable bool
}

// Check reads the newest release and compares it with the version handed in.
//
// Nothing is downloaded and nothing is written. The binary of the release is
// left alone: what an update needs before it starts is whether to start at all,
// and that is one small answer rather than fifteen megabytes.
//
// A failure is returned as an error rather than being hidden behind a fallback,
// which is where this differs from Fetch. Fetch falls back because an operator
// asked to install and a binary that works is at hand; here there is nothing to
// fall back to, and a check that could not read the release must say so instead
// of reading as a version that is up to date.
func Check(ctx context.Context, version string) (Latest, error) {
	return checkFrom(ctx, version, latestReleaseURL)
}

// checkFrom is Check with the API address handed in, so the tests run against
// an httptest server. Nothing else may point this anywhere but the constant.
func checkFrom(ctx context.Context, version string, latestURL string) (Latest, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	latest, err := latestRelease(ctx, newFetchClient(), latestURL)
	if err != nil {
		return Latest{}, fmt.Errorf("the latest release could not be read: %w", err)
	}

	newer, comparable := isNewer(version, latest.TagName)

	return Latest{Tag: latest.TagName, Newer: newer, Comparable: comparable}, nil
}

// isNewer compares two versions and says whether the second is after the first,
// and whether the question could be answered at all.
//
// Only three numbers separated by dots are read, with a leading v allowed on
// either side because the tag carries one and the version constant does not.
// Anything else is not compared: a tag with a suffix, a tag with four parts, a
// tag that is a word. Those answer false for both, which reads as "cannot tell"
// and never as "there is something newer".
//
// That asymmetry is the point. An answer of "newer" is what starts an install,
// and a version this does not understand is not grounds for replacing the
// binary of a running service.
func isNewer(current string, tag string) (newer bool, comparable bool) {
	running, ok := versionParts(current)
	if !ok {
		return false, false
	}

	released, ok := versionParts(tag)
	if !ok {
		return false, false
	}

	for i := range running {
		if released[i] != running[i] {
			return released[i] > running[i], true
		}
	}

	return false, true
}

// versionParts reads "3.7.4" or "v3.7.4" as its three numbers.
func versionParts(value string) ([3]int, bool) {
	var parts [3]int

	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "v")

	fields := strings.Split(trimmed, ".")
	if len(fields) != 3 {
		return parts, false
	}

	for i, field := range fields {
		// A field has to be digits and nothing else. Atoi takes a leading sign,
		// which would read "-1" as a number and make a version compare as
		// older than every other, so the sign is turned away here.
		if field == "" || strings.IndexFunc(field, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
			return parts, false
		}

		number, err := strconv.Atoi(field)
		if err != nil {
			return parts, false
		}

		parts[i] = number
	}

	return parts, true
}

// RegisteredFor reports whether this platform has a service registration that
// starts the executable handed in.
//
// It is what decides whether an update may be installed from inside the running
// program. The install ends by restarting the service, and on a machine where
// no service is registered there is nothing to restart: an install started
// there would register one, which is a different thing from updating and not
// what a press on an update screen is asking for.
//
// A platform with no backend, and a registration that names some other
// executable, both answer false. Only a failure of the ask itself is an error,
// so that a machine which could not be asked is told apart from one that
// answered no.
func RegisteredFor(executable string) (bool, error) {
	backend, err := newService()
	if err != nil {
		// No backend for this platform. It is not a failure to report: this
		// build simply cannot register a service, so it certainly is not
		// running as one.
		return false, nil
	}

	current, err := backend.Current()
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return false, nil
		}

		return false, err
	}

	// The comparison is of the paths as they are. A registration that starts
	// some other file is not this process's registration, and installing over
	// it would move an installation that nobody asked to move.
	return current.ExecutablePath != "" && current.ExecutablePath == executable, nil
}
