package collect

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAProcessIsNamedByTheServicesItHosts(t *testing.T) {
	procs := []Process{{Command: "svchost.exe", PID: 10}, {Command: "svchost.exe", PID: 11}, {Command: "sqlservr.exe", PID: 12}}
	hosted := map[int][]string{
		10: {"Windows Update", "Background Intelligent Transfer Service"},
		11: {"DNS Client"},
		99: {"Gone"},
	}

	attachHostedServices(procs, hosted)

	if got := strings.Join(procs[0].Services, "|"); got != "Background Intelligent Transfer Service|Windows Update" {
		t.Fatalf("sorted by name: %q", got)
	}
	if got := strings.Join(procs[1].Services, "|"); got != "DNS Client" {
		t.Fatalf("%q", got)
	}
	if procs[2].Services != nil {
		t.Fatalf("a process that hosts nothing carries no list: %v", procs[2].Services)
	}

	// Absent on the wire, not [], for a process that hosts nothing.
	body, _ := json.Marshal(procs[2])
	if strings.Contains(string(body), "services") {
		t.Fatalf("%s", body)
	}
}

func TestTheHostedListIsBounded(t *testing.T) {
	names := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		names = append(names, strings.Repeat("x", 300)+string(rune('a'+i%26)))
	}
	procs := []Process{{Command: "svchost.exe", PID: 4}}

	attachHostedServices(procs, map[int][]string{4: names})

	if len(procs[0].Services) != maxHostedServices {
		t.Fatalf("%d names", len(procs[0].Services))
	}
	for _, n := range procs[0].Services {
		if len(n) > maxHostedName {
			t.Fatalf("a name of %d bytes", len(n))
		}
	}
}
