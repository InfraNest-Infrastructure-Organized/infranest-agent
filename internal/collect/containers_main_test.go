package collect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fibTrieFixture = `Main:
  +-- 0.0.0.0/0 3 0 5
     |-- 0.0.0.0
        /0 universe UNICAST
     +-- 127.0.0.0/8 2 0 2
        +-- 127.0.0.0/31 1 0 0
           |-- 127.0.0.0
              /8 host LOCAL
           |-- 127.0.0.1
              /32 host LOCAL
     +-- 172.18.0.0/16 2 0 2
        |-- 172.18.0.0
           /16 link UNICAST
        |-- 172.18.0.5
           /32 host LOCAL
Local:
  +-- 0.0.0.0/0 3 0 5
     +-- 172.18.0.0/16 2 0 2
        |-- 172.18.0.5
           /32 host LOCAL
`

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fake machine: two containers on the systemd driver, one started under tini, one Podman container whose
// processes sit one cgroup down, and a docker-proxy publishing two ports of the first.
func fakeContainerHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cg := filepath.Join(root, "sys/fs/cgroup")
	writeFile(t, filepath.Join(cg, "system.slice/docker-aaa.scope/cgroup.procs"), "2210\n2201\n2230\n")
	writeFile(t, filepath.Join(cg, "system.slice/docker-bbb.scope/cgroup.procs"), "3301\n3302\n")
	writeFile(t, filepath.Join(cg, "machine.slice/libpod-ccc.scope/cgroup.procs"), "")
	writeFile(t, filepath.Join(cg, "machine.slice/libpod-ccc.scope/container/cgroup.procs"), "4401\n")

	proc := func(pid, comm, cmdline string) {
		writeFile(t, filepath.Join(root, "proc", pid, "stat"), pid+" ("+comm+") S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 1000 10")
		writeFile(t, filepath.Join(root, "proc", pid, "comm"), comm+"\n")
		writeFile(t, filepath.Join(root, "proc", pid, "cmdline"), cmdline)
	}
	proc("2201", "postgres", "postgres\x00-c\x00password=hunter2\x00")
	proc("2210", "postgres", "postgres: checkpointer\x00")
	proc("2230", "postgres", "postgres: walwriter\x00")
	proc("3301", "tini", "/sbin/tini\x00--\x00redis-server\x00")
	proc("3302", "redis-server", "redis-server *:6379\x00")
	proc("4401", "node_exporter", "/bin/node_exporter\x00")
	writeFile(t, filepath.Join(root, "proc/2201/net/fib_trie"), fibTrieFixture)
	writeFile(t, filepath.Join(root, "proc/3302/net/fib_trie"), strings.ReplaceAll(fibTrieFixture, "172.18.0.5", "172.18.0.6"))

	proc("900", "docker-proxy", "/usr/bin/docker-proxy\x00-proto\x00tcp\x00-host-ip\x000.0.0.0\x00-host-port\x0015432\x00-container-ip\x00172.18.0.5\x00-container-port\x005432\x00-use-listen-fd\x00")
	proc("901", "docker-proxy", "/usr/bin/docker-proxy\x00-proto\x00tcp\x00-host-ip\x00::\x00-host-port\x0015432\x00-container-ip\x00172.18.0.5\x00-container-port\x005432\x00")
	proc("902", "docker-proxy", "/usr/bin/docker-proxy\x00--proto=udp\x00--host-ip=127.0.0.1\x00--host-port=5353\x00--container-ip=172.18.0.5\x00--container-port=53\x00")
	// A proxy for a container this host does not report.
	proc("903", "docker-proxy", "/usr/bin/docker-proxy\x00-proto\x00tcp\x00-host-port\x008080\x00-container-ip\x00172.18.0.99\x00-container-port\x0080\x00")
	// Not a proxy, though its command line looks like one.
	proc("904", "bash", "docker-proxy\x00-host-port\x001\x00-container-ip\x00172.18.0.6\x00-container-port\x001\x00")

	return root
}

func fakeContainers() []Container {
	return []Container{
		{ID: "aaa", cgroup: "/system.slice/docker-aaa.scope"},
		{ID: "bbb", cgroup: "/system.slice/docker-bbb.scope"},
		{ID: "ccc", cgroup: "/machine.slice/libpod-ccc.scope"},
		{ID: "ddd", cgroup: "/system.slice/docker-ddd.scope"},
	}
}

func TestAContainerIsNamedByItsMainProcessPastAnInitShim(t *testing.T) {
	cs := fakeContainers()
	describeContainers(cs, fakeContainerHost(t), false)

	got := []string{}
	for _, c := range cs {
		got = append(got, c.MainProcess)
	}
	if strings.Join(got, ",") != "postgres,redis-server,node_exporter," {
		t.Fatalf("%v", got)
	}
	if cs[0].MainPID != 2201 || cs[1].MainPID != 3302 || cs[3].MainPID != 0 {
		t.Fatalf("pids %d %d %d", cs[0].MainPID, cs[1].MainPID, cs[3].MainPID)
	}
}

func TestAContainersCommandLineIsSentOnlyWithCommandLinesOn(t *testing.T) {
	off := fakeContainers()
	root := fakeContainerHost(t)
	describeContainers(off, root, false)
	body, _ := json.Marshal(off[0])
	if strings.Contains(string(body), "hunter2") || strings.Contains(string(body), "main_command") {
		t.Fatalf("%s", body)
	}

	on := fakeContainers()
	describeContainers(on, root, true)
	if on[0].MainCommand != "postgres -c password=hunter2" {
		t.Fatalf("%q", on[0].MainCommand)
	}
}

func TestPublishedPortsAreReadFromDockerProxies(t *testing.T) {
	cs := fakeContainers()
	describeContainers(cs, fakeContainerHost(t), false)

	body, _ := json.Marshal(cs[0].Ports)
	want := `[{"protocol":"udp","host_ip":"127.0.0.1","host_port":5353,"container_port":53},` +
		`{"protocol":"tcp","host_ip":"0.0.0.0","host_port":15432,"container_port":5432},` +
		`{"protocol":"tcp","host_ip":"::","host_port":15432,"container_port":5432}]`
	if string(body) != want {
		t.Fatalf("%s", body)
	}
	if cs[1].Ports != nil || cs[2].Ports != nil {
		t.Fatalf("ports on a container no proxy names: %v %v", cs[1].Ports, cs[2].Ports)
	}

	// The cgroup path is ours, not the wire's.
	whole, _ := json.Marshal(cs[0])
	if strings.Contains(string(whole), "system.slice") {
		t.Fatalf("%s", whole)
	}
}

func TestADockerProxyIsReadByItsFiveFlagsOnly(t *testing.T) {
	for _, bad := range [][]string{
		{"docker-proxy", "-proto", "tcp", "-host-port", "0", "-container-ip", "172.17.0.2", "-container-port", "80"},
		{"docker-proxy", "-proto", "icmp", "-host-port", "80", "-container-ip", "172.17.0.2", "-container-port", "80"},
		{"docker-proxy", "-host-port", "80", "-container-ip", "not-an-ip", "-container-port", "80"},
		{"docker-proxy", "-host-port", "80", "-container-port", "80"},
	} {
		if p, ok := parseDockerProxy(bad); ok {
			t.Errorf("%v read as %+v", bad, p)
		}
	}

	p, ok := parseDockerProxy([]string{"docker-proxy", "-container-ip", "fd00::5", "-container-port", "443", "-host-port", "443"})
	if !ok || p.port.Protocol != "tcp" || p.containerIP.String() != "fd00::5" || p.port.HostIP != "" {
		t.Fatalf("%+v %v", p, ok)
	}
}

func TestANamespacesOwnAddressesAreReadFromTheTrieAndIfInet6(t *testing.T) {
	v4 := parseFibTrieLocal(strings.NewReader(fibTrieFixture))
	if len(v4) != 1 || v4[0].String() != "172.18.0.5" {
		t.Fatalf("%v", v4)
	}
	v6 := parseIfInet6(strings.NewReader("00000000000000000000000000000001 01 80 10 80 lo\n" +
		"fe800000000000000042acfffe120005 0b 40 20 80 eth0\n" +
		"fd000000000000000000000000000005 0b 40 00 80 eth0\n"))
	if len(v6) != 1 || v6[0].String() != "fd00::5" {
		t.Fatalf("%v", v6)
	}
}
