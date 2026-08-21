package daemon

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordlessAccountWarningGivesExactSafeRemedies(t *testing.T) {
	got := passwordlessAccountWarning("192.0.2.1", "root")
	for _, want := range []string{
		"ssh -t 'root@192.0.2.1' passwd",
		"LuCI",
		"System → Administration → Router Password",
		"will not change /etc/shadow",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q: %q", want, got)
		}
	}
}

func TestSSHBootstrapFailureNamesTheAvailableActions(t *testing.T) {
	got := sshBootstrapFailure(errors.New("connection refused")).Error()
	for _, want := range []string{
		"Dropbear",
		"port 22",
		"SSH private key",
		"password is still required for the ubus sign-in",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SSH failure does not mention %q: %q", want, got)
		}
	}
}
