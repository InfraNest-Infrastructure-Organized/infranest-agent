package journal

import (
	"encoding/json"
	"strings"
	"testing"
)

// The fixture (testdata/authfail.py) holds 9 refusals in 14:00-14:59, each line commented with whether it counts.
func TestAnHourCountsItsRefusedLoginsOnce(t *testing.T) {
	h := countHour(openFixture(t, "authfail"), at(14, 0))
	if h.AuthFailures != 9 {
		t.Fatalf("auth_failures = %d, want 9", h.AuthFailures)
	}
	if next := countHour(openFixture(t, "authfail"), at(15, 0)); next.AuthFailures != 1 {
		t.Fatalf("the next hour counted %d, want its own 1", next.AuthFailures)
	}
	if quiet := countHour(openFixture(t, "authfail"), at(9, 0)); quiet.AuthFailures != 0 {
		t.Fatalf("an hour with no refusals counted %d", quiet.AuthFailures)
	}
}

// The count is a number: nothing from the lines that were read to make it reaches the payload.
func TestTheCountCarriesNoNameOrAddress(t *testing.T) {
	b, err := json.Marshal(countHour(openFixture(t, "authfail"), at(14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"root", "jaspar", "admin", "203.0.113", "198.51.100", "192.0.2", "Failed", "pam_"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the hour's counts contain %q: %s", leak, b)
		}
	}
	if !strings.Contains(string(b), `"auth_failures":9`) {
		t.Errorf("auth_failures missing from %s", b)
	}
}

// Zero is sent, not omitted: it is an answer, and an absent field means an agent that does not count.
func TestZeroRefusalsIsSent(t *testing.T) {
	b, _ := json.Marshal(Hour{})
	if !strings.Contains(string(b), `"auth_failures":0`) {
		t.Fatalf("zero omitted: %s", b)
	}
}

func TestWhichLinesAreRefusals(t *testing.T) {
	cases := []struct {
		identifier, message string
		want                bool
	}{
		{"sshd", "Failed password for root from 203.0.113.7 port 1 ssh2", true},
		{"sshd-session", "Failed password for root from 203.0.113.7 port 1 ssh2", true},
		{"sshd-auth", "Failed keyboard-interactive/pam for root from 203.0.113.7 port 1 ssh2", true},
		{"sshd", "Invalid user admin from 203.0.113.7 port 1", true},
		{"sshd", "Failed password for invalid user admin from 203.0.113.7 port 1 ssh2", false},
		{"sshd", "Failed none for invalid user admin from 203.0.113.7 port 1 ssh2", false},
		{"sshd", "Failed publickey for root from 203.0.113.7 port 1 ssh2", false},
		{"sshd", "Accepted password for root from 203.0.113.7 port 1 ssh2", false},
		{"sshd", "pam_unix(sshd:auth): authentication failure; user=root", false},
		{"sshd", "error: PAM: Authentication failure for root from 203.0.113.7", false},
		{"app", "Failed password for root from 203.0.113.7 port 1 ssh2", false},
		{"dropbear", "Bad password attempt for 'root' from 192.0.2.1:1", true},
		{"dropbear", "Login attempt for nonexistent user from 192.0.2.1:1", true},
		{"dropbear", "Exit before auth from <192.0.2.1:1>: Exited normally", false},
		{"sudo", "pam_unix(sudo:auth): authentication failure; logname=jaspar", true},
		{"su", "pam_unix(su-l:auth): authentication failure; logname=jaspar", true},
		{"cockpit-session", "pam_sss(cockpit:auth): authentication failure; logname=", true},
		{"sudo", "jaspar : 3 incorrect password attempts ; TTY=pts/0", false},
		{"CRON", "pam_unix(cron:session): session opened for user root", false},
		{"sudo", "pam_unix(sudo:auth): conversation failed", false},
		{"x", "pam_broken: authentication failure", false},
		{"x", "pam_x(:auth): authentication failure", true},
		{"sshd", "Invalid user \nsecond line", true},
		{"sshd", "", false},
	}
	for _, c := range cases {
		if got := authFailure(c.identifier, c.message); got != c.want {
			t.Errorf("authFailure(%q, %q) = %v, want %v", c.identifier, c.message, got, c.want)
		}
	}
}
