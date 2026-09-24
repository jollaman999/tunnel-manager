//go:build !windows

package main

import (
	"os"
	"testing"
)

// requireReportKeptToTheOwner fails unless the report of an install is at
// installReportMode.
func requireReportKeptToTheOwner(t *testing.T, report string) {
	t.Helper()

	info, err := os.Stat(report)
	if err != nil {
		t.Fatalf("the report file %s was not made: %v", report, err)
	}

	if info.Mode().Perm() != installReportMode {
		t.Errorf("the report file is %v, want %v", info.Mode().Perm(), os.FileMode(installReportMode))
	}
}
