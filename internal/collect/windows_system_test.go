//go:build windows

package collect

import (
	"strings"
	"testing"
)

// These run on a real Windows machine — CI's windows-latest — because a registry layout or a struct offset
// that is wrong compiles fine and reads garbage. What is asserted is what every Windows Server has.

func TestWindowsDescribesItself(t *testing.T) {
	s := systemWindows()
	if !strings.Contains(s.OS, "Windows") {
		t.Fatalf("OS = %q", s.OS)
	}
	if !strings.HasPrefix(s.Kernel, "10.0.") {
		t.Fatalf("kernel = %q", s.Kernel)
	}
	if s.CPUs < 1 {
		t.Fatalf("cpus = %d", s.CPUs)
	}
	t.Logf("os=%q kernel=%q reboot_required=%v reason=%q", s.OS, s.Kernel, s.RebootRequired, s.RebootReason)
}

func TestTheAutomaticServicesAreListedWithTheirState(t *testing.T) {
	services, hosted, err := servicesWindows()
	if err != nil {
		t.Fatal(err)
	}

	// The event log is automatic-start and running on every Windows there is.
	var eventLog *Service
	for i := range services {
		if strings.EqualFold(services[i].Unit, "EventLog") {
			eventLog = &services[i]
		}
	}
	if eventLog == nil || eventLog.ActiveState != "active" || eventLog.SubState != "running" {
		t.Fatalf("EventLog missing or not running among %d services: %+v", len(services), eventLog)
	}
	// A running service says which process it lives in (InfraNest #5670), and that process hosts it.
	if eventLog.PID <= 0 {
		t.Fatalf("EventLog is running and names no process: %+v", eventLog)
	}
	found := false
	for _, name := range hosted[eventLog.PID] {
		found = found || name == eventLog.Description
	}
	if !found {
		t.Fatalf("process %d is not said to host %q: %v", eventLog.PID, eventLog.Description, hosted[eventLog.PID])
	}
	t.Logf("%d automatic services", len(services))
}

func TestAStoppedServiceIsFailedOnlyWithAnErrorExit(t *testing.T) {
	clean := windowsService("a", "A", serviceStatusProcess{CurrentState: serviceStopped})
	never := windowsService("b", "B", serviceStatusProcess{CurrentState: serviceStopped, Win32ExitCode: errorServiceNeverStarted})
	broken := windowsService("c", "C", serviceStatusProcess{CurrentState: serviceStopped, Win32ExitCode: 1066})

	if clean.ActiveState != "inactive" || never.ActiveState != "inactive" {
		t.Fatalf("a service that stopped on purpose is not failed: %+v %+v", clean, never)
	}
	if broken.ActiveState != "failed" || broken.ExecMainStatus == nil || *broken.ExecMainStatus != 1066 || broken.ExecMainCode == nil || *broken.ExecMainCode != 1 {
		t.Fatalf("an error exit is failed: %+v", broken)
	}
}

func TestARunningServiceNamesItsProcessAndAStoppedOneDoesNot(t *testing.T) {
	running := windowsService("w", "W", serviceStatusProcess{CurrentState: serviceRunning, ProcessID: 1234})
	stopped := windowsService("s", "S", serviceStatusProcess{CurrentState: serviceStopped, ProcessID: 0})

	if running.PID != 1234 {
		t.Fatalf("a running service names its process: %+v", running)
	}
	if stopped.PID != 0 {
		t.Fatalf("a stopped service names none: %+v", stopped)
	}
}
