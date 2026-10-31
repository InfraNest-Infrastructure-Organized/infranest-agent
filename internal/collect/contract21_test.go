package collect

import (
	"encoding/json"
	"strings"
	"testing"
)

// PAYLOAD.md states what contract 21 adds to the one sample that carries snapshots, at its caps. The receiver's
// caps are used — 32 processes, 100 containers — although the agent itself reports ten processes.
func TestWhatContract21AddsToTheSnapshotSampleAtItsCaps(t *testing.T) {
	without := Sample{Processes: make([]Process, 32)}
	containers := make([]Container, maxContainers)
	withoutContainers := make([]Container, maxContainers)
	without.Containers = &withoutContainers

	with := Sample{Processes: make([]Process, 32), Containers: &containers}
	for i := range with.Processes {
		with.Processes[i].Package = &Package{
			Name: strings.Repeat("n", maxPackageName), Version: strings.Repeat("v", maxPackageVersion),
			Description: strings.Repeat("d", maxPackageDescription), Source: "dpkg", Match: "executable",
		}
		for j := 0; j < maxHostedServices; j++ {
			with.Processes[i].Services = append(with.Processes[i].Services, strings.Repeat("s", maxHostedName))
		}
	}
	for i := range containers {
		containers[i].MainPID = 4_194_304
		containers[i].MainProcess = strings.Repeat("p", 15)
		containers[i].MainCommand = strings.Repeat("c", maxCommand)
		for j := 0; j < maxPublishedPorts; j++ {
			containers[i].Ports = append(containers[i].Ports, PublishedPort{Protocol: "tcp", HostIP: "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", HostPort: 65535, ContainerPort: 65535})
		}
	}

	a, _ := json.Marshal(without)
	b, _ := json.Marshal(with)
	added := len(b) - len(a)
	if added > 478_588 {
		t.Fatalf("contract 21 adds %d bytes at its caps; PAYLOAD.md says 478,588", added)
	}
	t.Logf("contract 21 adds %d bytes to the snapshot sample at its caps", added)
}
