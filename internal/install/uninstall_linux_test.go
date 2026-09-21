//go:build linux

package install

import (
	"errors"
	"fmt"
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

// The units the installs below register.
//
// One name per test rather than one shared between them, for the reason
// flowRoundTripUnit has its own: a cleanup that runs while another test is
// halfway through its own registration would pull the unit out from under it.
// None of them is the name this program installs under, which refuseTestUnit
// holds them to before anything is written.
const (
	overInstallUnit = serviceName + "-overtest" + unitSuffix
	elsewhereUnit   = serviceName + "-elsewheretest" + unitSuffix
	spacedPathUnit  = serviceName + "-spacetest" + unitSuffix
)

// refuseTestUnit ends the test before it registers anything if the name it was
// given is the one the deployment runs under.
//
// The machine this is developed on runs tunnel-manager.service, and these tests
// stop services, write over executables and remove registrations. A name that
// drifted onto the production one would take the deployment down, and finding
// that out from the first systemctl call is too late.
func refuseTestUnit(t *testing.T, unit string) {
	t.Helper()

	if unit == serviceName+unitSuffix || !strings.HasPrefix(unit, serviceName+"-") {
		t.Fatalf("this test would register %q, which is not a unit of its own", unit)
	}
}

// roundTripGate is the agreement every test here runs behind: the environment
// variable that asks for it, and being the user that may register a unit.
func roundTripGate(t *testing.T, unit string) {
	t.Helper()

	if os.Getenv(roundTripEnv) != "1" {
		t.Skipf("set %s=1 to register a %s with systemd", roundTripEnv, unit)
	}

	if os.Geteuid() != 0 {
		t.Skipf("registering a unit needs root, this is uid %d", os.Geteuid())
	}

	refuseTestUnit(t, unit)
}

// TestInstallOverTheSameInstallation installs twice over the same paths,
// through the real systemd backend.
//
// The tests against the recording backend show that a second install is asked
// to stop the service and register again. What they cannot show is the part the
// operator is left with: that the file on disk is the new one, that the report
// names both the md5 that was there and the md5 that is there now, and that the
// service is up again afterwards rather than stopped and left that way.
func TestInstallOverTheSameInstallation(t *testing.T) {
	roundTripGate(t, overInstallUnit)
	allowPrivilege(t)

	svc := systemd{unit: overInstallUnit}
	unitPath := filepath.Join(unitDir, overInstallUnit)

	// Registered before anything is written, so that a run which fails halfway
	// leaves nothing of this behind either.
	t.Cleanup(func() {
		_, _ = svc.run(jobTimeout, "disable", "--now", svc.unit)
		_ = os.Remove(unitPath)
		_, _ = svc.run(commandTimeout, "daemon-reload")
	})

	dir := t.TempDir()
	plan := Plan{
		ExecutablePath: filepath.Join(dir, serviceName+"-overtest"),
		DataDir:        filepath.Join(dir, "data"),
		DatabaseFile:   filepath.Join(dir, "data", databaseFileName),
	}

	// Two programs that behave the same and are not the same file: what a
	// second install has to show is that the bytes on disk were replaced, and
	// two copies of one program would pass that whether or not anything was
	// written. Both only sleep, for the reason the round trip above does.
	first := writeFile(t, filepath.Join(t.TempDir(), "downloaded"),
		"#!/bin/sh\n# the first release\nexec sleep 600\n")
	second := writeFile(t, filepath.Join(t.TempDir(), "downloaded"),
		"#!/bin/sh\n# the second release, a different file\nexec sleep 600\n")

	_, err := svc.Current()
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("before installing, Current answered %v, want ErrNotInstalled. Is %s already on this machine?",
			err, overInstallUnit)
	}

	var firstOut strings.Builder

	before, err := install(svc, plan, first, "the test, first release", &firstOut)
	if err != nil {
		t.Fatalf("the first install failed: %v", err)
	}

	t.Logf("\n%s", firstOut.String())

	if before.Replaced {
		t.Error("the first install says it wrote over a registration that was already there")
	}

	if state := activeState(t, svc.unit); state != "active" {
		t.Fatalf("after the first install the service is %q, want \"active\"", state)
	}

	var secondOut strings.Builder

	after, err := install(svc, plan, second, "the test, second release", &secondOut)
	if err != nil {
		t.Fatalf("the second install failed: %v", err)
	}

	t.Logf("\n%s", secondOut.String())

	if !after.Replaced {
		t.Error("the second install says nothing was registered before")
	}

	// The md5 the second install found in place is the one the first install
	// left. That is the whole of what tells an operator the file they are
	// replacing is the one they think it is.
	if after.DigestBefore != before.DigestAfter {
		t.Errorf("the second install found md5 %q in place, want the one the first install left, %q",
			after.DigestBefore, before.DigestAfter)
	}

	if after.DigestAfter == after.DigestBefore {
		t.Errorf("the md5 before and after the second install are both %q, want the file to have changed",
			after.DigestAfter)
	}

	report := secondOut.String()

	for _, want := range []string{
		"md5 before",
		after.DigestBefore,
		"md5 after",
		after.DigestAfter,
		"written over the one that was already there",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report of the second install does not hold %q:\n%s", want, report)
		}
	}

	if strings.Contains(report, "no file was there") {
		t.Errorf("the report of the second install says no file was there:\n%s", report)
	}

	// Read off the disk rather than taken from the outcome, because the digests
	// above are worked out by the code under test.
	installed, err := os.ReadFile(plan.ExecutablePath)
	if err != nil {
		t.Fatalf("failed to read the installed executable: %v", err)
	}

	wanted, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("failed to read the second release: %v", err)
	}

	if string(installed) != string(wanted) {
		t.Errorf("the executable at %s is\n%s\nwant the second release\n%s",
			plan.ExecutablePath, installed, wanted)
	}

	// A second install stops the service to write over the executable it holds
	// open. Leaving it stopped would be an install that took the service down.
	if state := activeState(t, svc.unit); state != "active" {
		t.Errorf("after the second install the service is %q, want \"active\"", state)
	} else {
		t.Logf("after the second install, systemctl is-active %s says %q", svc.unit, state)
	}
}

// TestInstallAtAnotherPlaceIsRefused drives the refusal against a registration
// that really exists.
//
// The tests against the recording backend show that install answers with an
// error. What they cannot show is the half that matters on a machine: that the
// refusal came before anything was touched, so the unit still holds the paths
// it held, the installed executable is the one that was there, and nothing was
// put at the paths that were asked for.
func TestInstallAtAnotherPlaceIsRefused(t *testing.T) {
	roundTripGate(t, elsewhereUnit)
	allowPrivilege(t)

	svc := systemd{unit: elsewhereUnit}
	unitPath := filepath.Join(unitDir, elsewhereUnit)

	t.Cleanup(func() {
		_, _ = svc.run(jobTimeout, "disable", "--now", svc.unit)
		_ = os.Remove(unitPath)
		_, _ = svc.run(commandTimeout, "daemon-reload")
	})

	here := t.TempDir()
	installed := Plan{
		ExecutablePath: filepath.Join(here, serviceName+"-elsewheretest"),
		DataDir:        filepath.Join(here, "data"),
		DatabaseFile:   filepath.Join(here, "data", databaseFileName),
	}

	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "#!/bin/sh\nexec sleep 600\n")

	_, err := install(svc, installed, source, "the test", &strings.Builder{})
	if err != nil {
		t.Fatalf("the install failed: %v", err)
	}

	if state := activeState(t, svc.unit); state != "active" {
		t.Fatalf("after the install the service is %q, want \"active\"", state)
	}

	unitBefore, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("failed to read the unit that was written: %v", err)
	}

	executableBefore, err := os.ReadFile(installed.ExecutablePath)
	if err != nil {
		t.Fatalf("failed to read the installed executable: %v", err)
	}

	elsewhere := t.TempDir()
	asked := Plan{
		ExecutablePath: filepath.Join(elsewhere, "bin", serviceName+"-elsewheretest"),
		DataDir:        filepath.Join(elsewhere, "data"),
		DatabaseFile:   filepath.Join(elsewhere, "data", databaseFileName),
	}

	var out strings.Builder

	_, err = install(svc, asked, source, "the test, somewhere else", &out)
	if err == nil {
		t.Fatalf("installing at %s over a service registered at %s was allowed",
			asked.ExecutablePath, installed.ExecutablePath)
	}

	t.Logf("refused as it should: %v", err)

	if out.Len() != 0 {
		t.Errorf("the refused install wrote a report:\n%s", out.String())
	}

	unitAfter, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("after the refusal the unit could not be read: %v", err)
	}

	if string(unitAfter) != string(unitBefore) {
		t.Errorf("after the refusal the unit is\n%s\nwant it as it was\n%s", unitAfter, unitBefore)
	}

	executableAfter, err := os.ReadFile(installed.ExecutablePath)
	if err != nil {
		t.Fatalf("after the refusal the installed executable could not be read: %v", err)
	}

	if string(executableAfter) != string(executableBefore) {
		t.Errorf("after the refusal the executable at %s has changed", installed.ExecutablePath)
	}

	// Nothing was put where the refused install was pointed, which is what
	// makes the refusal something the operator can act on: there is one
	// installation on this machine and not one and a half.
	for _, path := range []string{asked.ExecutablePath, asked.DataDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused install left %s behind: %v", path, err)
		}
	}

	// Asked of systemd rather than of the unit file, because what a start would
	// run is what systemd loaded and not what is on disk.
	execStart := systemctlShow(t, svc.unit, "ExecStart")
	if !strings.Contains(execStart, installed.ExecutablePath) {
		t.Errorf("after the refusal systemctl says the service starts\n%s\nwant %s",
			execStart, installed.ExecutablePath)
	}

	t.Logf("systemctl show %s -p ExecStart:\n%s", svc.unit, execStart)

	if state := activeState(t, svc.unit); state != "active" {
		t.Errorf("after the refusal the service is %q, want it left running", state)
	}
}

// TestInstallToAPathWithASpaceLeavesNothing installs to a path a systemd unit
// cannot carry.
//
// checkUnitPaths refuses that path, and the unit test beside it covers the
// refusal itself. What is being asked here is what the machine is left with,
// which the refusal alone does not say: an install that copied the executable
// before finding out has left a file at a path nothing will ever name again,
// and no uninstall can find it.
func TestInstallToAPathWithASpaceLeavesNothing(t *testing.T) {
	roundTripGate(t, spacedPathUnit)
	allowPrivilege(t)

	svc := systemd{unit: spacedPathUnit}
	unitPath := filepath.Join(unitDir, spacedPathUnit)

	t.Cleanup(func() {
		_, _ = svc.run(jobTimeout, "disable", "--now", svc.unit)
		_ = os.Remove(unitPath)
		_, _ = svc.run(commandTimeout, "daemon-reload")
	})

	dir := t.TempDir()
	plan := Plan{
		ExecutablePath: filepath.Join(dir, "tunnel manager", serviceName+"-spacetest"),
		DataDir:        filepath.Join(dir, "data"),
		DatabaseFile:   filepath.Join(dir, "data", databaseFileName),
	}

	source := writeFile(t, filepath.Join(t.TempDir(), "downloaded"), "#!/bin/sh\nexec sleep 600\n")

	var out strings.Builder

	_, err := install(svc, plan, source, "the test", &out)
	if err == nil {
		t.Fatalf("installing to %q was allowed", plan.ExecutablePath)
	}

	t.Logf("refused as it should: %v", err)

	if _, err := os.Stat(plan.ExecutablePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused install left an executable at %q: %v", plan.ExecutablePath, err)
	}

	// Said out loud whichever way it went, because this is the whole question:
	// the path is one no uninstall could read back out of a unit, so a file
	// there is a file that stays there.
	t.Logf("after the refusal, is there a file at %q: %v", plan.ExecutablePath, statText(plan.ExecutablePath))

	if _, err := svc.Current(); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("after the refusal, Current answered %v, want ErrNotInstalled", err)
	}

	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused install wrote a unit at %s: %v", unitPath, err)
	}
}

// statText says whether a path is there, for a log line rather than a check.
func statText(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}

	return fmt.Sprintf("yes, %d bytes, mode %s", info.Size(), info.Mode())
}
