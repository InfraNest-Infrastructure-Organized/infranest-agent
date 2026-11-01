package loginunit

import "testing"

func TestALoginServicesInstanceIsReportedAsItsTemplate(t *testing.T) {
	cases := map[string]string{
		"sshd@3-10.0.0.1:22-203.0.113.7:51234.service":      "sshd@.service",
		"ssh@0-[2001:db8::1]:22-[2001:db8::7]:4242.service": "ssh@.service",
		// A name without its type suffix: the address's dots are not one.
		"sshd@3-10.0.0.1:22-203.0.113.7:51234": "sshd@.service",
		"getty@tty1.service":                   "getty@.service",
		"sshd@.service":                        "sshd@.service",
		"ssh.service":                          "ssh.service",
		"nginx@site-a.service":                 "nginx@site-a.service",
		"postgresql@16-main.service":           "postgresql@16-main.service",
	}
	for unit, want := range cases {
		if got := Reported(unit); got != want {
			t.Errorf("%s is reported as %q, want %q", unit, got, want)
		}
	}
}

func TestOnlyALoginServicesInstanceIsAnInstance(t *testing.T) {
	cases := map[string]bool{
		"sshd@3-10.0.0.1:22-203.0.113.7:51234.service": true,
		"getty@tty1.service":                           true,
		"sshd@.service":                                false,
		"sshd.service":                                 false,
		"postgresql@16-main.service":                   false,
	}
	for unit, want := range cases {
		if got := Instance(unit); got != want {
			t.Errorf("Instance(%s) = %v, want %v", unit, got, want)
		}
	}
}
