//go:build linux

package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// flowRoundTripUnit is the unit the round trip below registers.
//
// It is a name of its own and not the one this program installs under, for the
// reason the backend round trip has one: the machine this is developed on runs
// tunnel-manager.service, and a test that drove the install and the uninstall
// flows against that name would take the deployment down and remove it. It is
// also not the backend round trip's name, so that one run cleaning up after
// itself cannot pull the unit out from under the other.
const flowRoundTripUnit = serviceName + "-uninstalltest" + unitSuffix

// TestUninstallRoundTrip installs and then removes a service on this machine,
// through the real systemd backend.
//
// The tests beside this one drive the flow against a backend that records what
// it was asked and does nothing. What they cannot show is the part an uninstall
// is judged by: that the registration is gone from systemd afterwards, that the
// unit file is off the disk, that the executable the registration named was the
// one removed, and that the data is still there when -purge was not asked for.
func TestUninstallRoundTrip(t *testing.T) {
	if os.Getenv(roundTripEnv) != "1" {
		t.Skipf("set %s=1 to register a %s with systemd", roundTripEnv, flowRoundTripUnit)
	}

	if os.Geteuid() != 0 {
		t.Skipf("registering a unit needs root, this is uid %d", os.Geteuid())
	}

	allowPrivilege(t)

	svc := systemd{unit: flowRoundTripUnit}
	unitPath := filepath.Join(unitDir, flowRoundTripUnit)

	// Registered before anything is written, so that a run which fails halfway
	// leaves nothing of this behind either.
	t.Cleanup(func() {
		_, _ = svc.run(jobTimeout, "disable", "--now", svc.unit)
		_ = os.Remove(unitPath)
		_, _ = svc.run(commandTimeout, "daemon-reload")
	})

	dir := t.TempDir()
	plan := Plan{
		ExecutablePath: filepath.Join(dir, serviceName+"-uninstalltest"),
		DataDir:        filepath.Join(dir, "data"),
		DatabaseFile:   filepath.Join(dir, "data", databaseFileName),
	}

	// A program that stays up and ignores what it is handed, the same as the
	// backend round trip uses: what is under test is the flow, and the real
	// binary would want a database and a port to itself, which is a second
	// thing that can fail and says nothing about an uninstall.
	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "#!/bin/sh\nexec sleep 600\n")

	_, err := svc.Current()
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("before installing, Current answered %v, want ErrNotInstalled. Is %s already on this machine?",
			err, flowRoundTripUnit)
	}

	var installOut strings.Builder

	outcome, err := install(svc, plan, source, "the test", &installOut)
	if err != nil {
		t.Fatalf("the install failed: %v", err)
	}

	if !outcome.Started {
		t.Fatal("the install did not start the service")
	}

	t.Logf("\n%s", installOut.String())

	if state := activeState(t, svc.unit); state != "active" {
		t.Fatalf("after the install the service is %q, want \"active\"", state)
	}

	// Written by the test and not by the service: what the unit starts is a
	// sleep, so nothing else puts a file in the data directory. These are what
	// an uninstall without -purge has to leave alone, and an empty directory
	// would not show that it did.
	writeFile(t, plan.DatabaseFile, "the database")
	keyFile := writeFile(t, filepath.Join(plan.DataDir, "key"), "the key the passwords are sealed with")

	var out strings.Builder

	removed, err := uninstall(svc, Removal{}, &out)
	if err != nil {
		t.Fatalf("the uninstall failed: %v", err)
	}

	t.Logf("\n%s", out.String())

	if !removed.Registered {
		t.Error("the uninstall does not say it took a registration out")
	}

	// The paths came out of systemd and not out of the plan this test built.
	// That round trip through the unit is the whole of how an uninstall knows
	// what to remove.
	if removed.ExecutablePath != plan.ExecutablePath {
		t.Errorf("the uninstall removed %q, want the executable the registration named, %q",
			removed.ExecutablePath, plan.ExecutablePath)
	}

	if removed.DataDir != plan.DataDir {
		t.Errorf("the uninstall reports the data at %q, want %q", removed.DataDir, plan.DataDir)
	}

	if state := activeState(t, svc.unit); state == "active" {
		t.Errorf("after the uninstall the service is still %q", state)
	}

	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after the uninstall the unit %s is still there: %v", unitPath, err)
	}

	if shown := systemctlShow(t, svc.unit, "FragmentPath"); strings.TrimSpace(shown) != "FragmentPath=" {
		t.Errorf("after the uninstall systemctl still answers %q", shown)
	}

	if _, err := svc.Current(); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("after the uninstall, Current answered %v, want ErrNotInstalled", err)
	}

	if _, err := os.Stat(plan.ExecutablePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after the uninstall the executable is still at %s: %v", plan.ExecutablePath, err)
	}

	for _, path := range []string{plan.DataDir, plan.DatabaseFile, keyFile} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed although -purge was not asked for: %v", path, err)
		}
	}

	// With the registration gone there is nothing left on the machine saying
	// where the data is, which is the case the design hands to -db.
	var purgeOut strings.Builder

	purged, err := uninstall(svc, Removal{DatabaseFile: plan.DatabaseFile, Purge: true}, &purgeOut)
	if err != nil {
		t.Fatalf("the purge failed: %v", err)
	}

	t.Logf("\n%s", purgeOut.String())

	if !purged.Purged {
		t.Error("the removal does not say the data was purged")
	}

	if _, err := os.Stat(plan.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after -purge the data directory %s is still there: %v", plan.DataDir, err)
	}

	// Nothing registered and nothing named: the machine is already in the state
	// that was asked for, and an uninstall has to say so rather than report a
	// removal it did not do.
	_, err = uninstall(svc, Removal{}, &strings.Builder{})
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("an uninstall with nothing left answered %v, want ErrNotInstalled", err)
	} else {
		t.Logf("with nothing left: %v", err)
	}
}

// TestProductionServiceIsUntouched is the check that the round trip above stays
// off the service this machine actually runs.
//
// It is here rather than in the shell around the run because the name is what
// decides it: a change that made the flow above use serviceName would take the
// deployment down, and this fails in the same run rather than after somebody
// notices the tunnels are gone.
func TestProductionServiceIsUntouched(t *testing.T) {
	if !strings.HasPrefix(flowRoundTripUnit, serviceName+"-") || flowRoundTripUnit == serviceName+unitSuffix {
		t.Fatalf("the round trip unit is %q, which is the unit this program installs", flowRoundTripUnit)
	}

	if os.Getenv(roundTripEnv) != "1" || os.Geteuid() != 0 {
		return
	}

	// Asked of systemd directly, so that it is the machine answering and not
	// this package. A production service that was not installed here answers
	// with an empty FragmentPath, which is as valid a state to be left in.
	t.Logf("systemctl show %s:\n%s", serviceName+unitSuffix,
		systemctlShow(t, serviceName+unitSuffix, "FragmentPath", "ActiveState", "MainPID"))
}
