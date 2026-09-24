//go:build windows

package main

import (
	"testing"
)

// requireReportKeptToTheOwner fails unless the DACL of the report of an
// install lets in nobody but the user of this process, SYSTEM and
// Administrators, which is what installReportMode says on Unix.
func requireReportKeptToTheOwner(t *testing.T, report string) {
	t.Helper()

	others := otherSIDs(t, report)
	if len(others) != 0 {
		t.Errorf("the DACL of the report file %s lets in %v as well", report, others)
	}
}
