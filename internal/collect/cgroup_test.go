package collect

import "testing"

func TestACgroupNamesTheServiceOrContainerAProcessBelongsTo(t *testing.T) {
	id := "4f1c8a9b2d3e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8"
	cases := []struct {
		name, body, unit, container string
	}{
		{"systemd service, cgroup v2", "0::/system.slice/php8.3-fpm.service\n", "php8.3-fpm.service", ""},
		{"docker under systemd", "0::/system.slice/docker-" + id + ".scope\n", "", "4f1c8a9b2d3e"},
		{"podman", "0::/machine.slice/libpod-" + id + ".scope/container\n", "", "4f1c8a9b2d3e"},
		{"docker, cgroupfs driver, v1", "12:pids:/docker/" + id + "\n11:memory:/docker/" + id + "\n", "", "4f1c8a9b2d3e"},
		{"a login session is neither", "0::/user.slice/user-1000.slice/session-3.scope\n", "", ""},
		{"a kernel thread has the root cgroup", "0::/\n", "", ""},
	}
	for _, c := range cases {
		unit, container := parseCgroupOwner(c.body)
		if unit != c.unit || container != c.container {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, unit, container, c.unit, c.container)
		}
	}
}
