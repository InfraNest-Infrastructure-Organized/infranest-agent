package collect

import (
	"bufio"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// What runs in each container and which ports Docker publishes for it (InfraNest #5710), read without the Docker
// socket and without any capability:
//
//   - the main process from the scope's `cgroup.procs` and that process's /proc/<pid>/stat — both world-readable;
//   - published ports from the command lines of Docker's `docker-proxy` processes, which consist of nothing but
//     flags naming addresses and ports. Only those five flags are read from them, in memory; the command line
//     itself is never sent, so this needs no ProcessArgs. Each proxy names the container by its address, which
//     is matched to a container by the addresses its own network namespace lists as local
//     (/proc/<pid>/net/fib_trie and if_inet6, world-readable like /proc/net/tcp).
//
// Everything here takes `root` — "/" on a real machine — so the tests can lay out a fake /proc and /sys.

const (
	maxPublishedPorts = 32
	maxCgroupProcs    = 64 << 10
	maxFibTrie        = 256 << 10
	maxProcEntries    = 200000
	maxShimSkips      = 64
)

// Init shims a container may run as its first process, which say nothing about what the container is.
var initShims = map[string]bool{"tini": true, "docker-init": true, "dumb-init": true, "catatonit": true}

func describeContainers(containers []Container, root string, args bool) {
	if len(containers) == 0 {
		return
	}

	byAddress := map[netip.Addr]int{}
	for i := range containers {
		c := &containers[i]
		pid, comm := mainProcess(root, c.cgroup)
		if pid <= 0 {
			continue
		}
		c.MainPID, c.MainProcess = pid, clip(comm, maxCommand)
		if args {
			if body, err := os.ReadFile(filepath.Join(root, "proc", strconv.Itoa(pid), "cmdline")); err == nil {
				if full := strings.TrimSpace(strings.ReplaceAll(string(body), "\x00", " ")); full != "" {
					c.MainCommand = clip(full, maxCommand)
				}
			}
		}
		for _, a := range namespaceAddresses(root, pid) {
			byAddress[a] = i
		}
	}

	if len(byAddress) == 0 {
		return
	}
	for _, p := range dockerProxies(root) {
		i, ok := byAddress[p.containerIP]
		if !ok || len(containers[i].Ports) >= maxPublishedPorts {
			continue
		}
		containers[i].Ports = append(containers[i].Ports, p.port)
	}
	for i := range containers {
		ports := containers[i].Ports
		sort.Slice(ports, func(a, b int) bool {
			if ports[a].HostPort != ports[b].HostPort {
				return ports[a].HostPort < ports[b].HostPort
			}
			if ports[a].Protocol != ports[b].Protocol {
				return ports[a].Protocol < ports[b].Protocol
			}
			return ports[a].HostIP < ports[b].HostIP
		})
	}
}

// mainProcess is the lowest process ID in a cgroup — the container's first process — past an init shim, with
// its name. A scope whose own list is empty (Podman puts the processes in a child cgroup) is looked into one
// level down.
func mainProcess(root, cgroup string) (int, string) {
	if cgroup == "" {
		return 0, ""
	}
	var pids []int
	for _, base := range []string{"sys/fs/cgroup", "sys/fs/cgroup/unified", "sys/fs/cgroup/systemd", "sys/fs/cgroup/pids"} {
		dir := filepath.Join(root, base, cgroup)
		if pids = readCgroupProcs(filepath.Join(dir, "cgroup.procs")); len(pids) > 0 {
			break
		}
		if entries, err := os.ReadDir(dir); err == nil {
			for n, e := range entries {
				if n >= 16 {
					break
				}
				if e.IsDir() {
					pids = append(pids, readCgroupProcs(filepath.Join(dir, e.Name(), "cgroup.procs"))...)
				}
			}
			if len(pids) > 0 {
				break
			}
		}
	}
	if len(pids) == 0 {
		return 0, ""
	}
	sort.Ints(pids)

	firstPID, firstComm := 0, ""
	for n, pid := range pids {
		if n >= maxShimSkips {
			break
		}
		stat, err := os.ReadFile(filepath.Join(root, "proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			continue
		}
		comm, err := parseProcComm(string(stat))
		if err != nil {
			continue
		}
		if firstPID == 0 {
			firstPID, firstComm = pid, comm
		}
		if !initShims[comm] {
			return pid, comm
		}
	}

	return firstPID, firstComm
}

func readCgroupProcs(path string) []int {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var pids []int
	scanner := bufio.NewScanner(io.LimitReader(f, maxCgroupProcs))
	for scanner.Scan() {
		if pid, err := atoiStrict(strings.TrimSpace(scanner.Text())); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}

	return pids
}

// namespaceAddresses lists the addresses a process's network namespace holds as its own, loopback and
// link-local left out. Read only to match a docker-proxy's container address; never sent.
func namespaceAddresses(root string, pid int) []netip.Addr {
	dir := filepath.Join(root, "proc", strconv.Itoa(pid), "net")
	var out []netip.Addr

	if f, err := os.Open(filepath.Join(dir, "fib_trie")); err == nil {
		out = append(out, parseFibTrieLocal(io.LimitReader(f, maxFibTrie))...)
		f.Close()
	}
	if f, err := os.Open(filepath.Join(dir, "if_inet6")); err == nil {
		out = append(out, parseIfInet6(io.LimitReader(f, maxFibTrie))...)
		f.Close()
	}

	return out
}

// parseFibTrieLocal reads the addresses a namespace's routing trie marks "/32 host LOCAL": its own IPv4
// addresses. The trie names an address on one line and what it is on the next.
func parseFibTrieLocal(r io.Reader) []netip.Addr {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	var last netip.Addr
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if rest, ok := strings.CutPrefix(line, "|-- "); ok {
			last, _ = netip.ParseAddr(strings.TrimSpace(rest))

			continue
		}
		if strings.HasPrefix(line, "/32 host LOCAL") && last.IsValid() && !last.IsLoopback() && !seen[last] {
			seen[last] = true
			out = append(out, last)
		}
	}

	return out
}

// parseIfInet6 reads a namespace's IPv6 addresses: 32 hex digits, then index, prefix, scope, flags and name.
func parseIfInet6(r io.Reader) []netip.Addr {
	var out []netip.Addr
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || len(fields[0]) != 32 {
			continue
		}
		var b [16]byte
		ok := true
		for i := 0; i < 16; i++ {
			v, err := strconv.ParseUint(fields[0][2*i:2*i+2], 16, 8)
			if err != nil {
				ok = false

				break
			}
			b[i] = byte(v)
		}
		a := netip.AddrFrom16(b)
		if ok && !a.IsLoopback() && !a.IsLinkLocalUnicast() {
			out = append(out, a)
		}
	}

	return out
}

type proxyPort struct {
	containerIP netip.Addr
	port        PublishedPort
}

// dockerProxies finds Docker's userland proxies and the port each forwards.
func dockerProxies(root string) []proxyPort {
	entries, err := os.ReadDir(filepath.Join(root, "proc"))
	if err != nil {
		return nil
	}
	var out []proxyPort
	for n, e := range entries {
		if n >= maxProcEntries {
			break
		}
		if _, err := atoiStrict(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(root, "proc", e.Name())
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "docker-proxy" {
			continue
		}
		f, err := os.Open(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(f, 4096))
		f.Close()
		if p, ok := parseDockerProxy(strings.Split(strings.TrimRight(string(body), "\x00"), "\x00")); ok {
			out = append(out, p)
		}
	}

	return out
}

// parseDockerProxy reads the five flags a docker-proxy is started with — `-proto tcp -host-ip 0.0.0.0
// -host-port 8080 -container-ip 172.17.0.2 -container-port 80` — and nothing else. Both `-flag value` and
// `-flag=value`, with one dash or two, as Go's flag package accepts them.
func parseDockerProxy(args []string) (proxyPort, bool) {
	flags := map[string]string{}
	for i := 1; i < len(args); i++ {
		name := strings.TrimLeft(args[i], "-")
		if name == args[i] {
			continue
		}
		if k, v, ok := strings.Cut(name, "="); ok {
			flags[k] = v

			continue
		}
		switch name {
		case "proto", "host-ip", "host-port", "container-ip", "container-port":
			if i+1 < len(args) {
				flags[name] = args[i+1]
				i++
			}
		}
	}

	proto := flags["proto"]
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" && proto != "sctp" {
		return proxyPort{}, false
	}
	hostPort, err1 := strconv.Atoi(flags["host-port"])
	containerPort, err2 := strconv.Atoi(flags["container-port"])
	containerIP, err3 := netip.ParseAddr(flags["container-ip"])
	if err1 != nil || err2 != nil || err3 != nil || hostPort < 1 || hostPort > 65535 || containerPort < 1 || containerPort > 65535 {
		return proxyPort{}, false
	}
	hostIP := ""
	if a, err := netip.ParseAddr(flags["host-ip"]); err == nil {
		hostIP = a.Unmap().String()
	}

	return proxyPort{
		containerIP: containerIP.Unmap(),
		port:        PublishedPort{Protocol: proto, HostIP: hostIP, HostPort: hostPort, ContainerPort: containerPort},
	}, true
}
