package harden

import (
	"strings"
	"testing"
)

// Encodes spike S1 finding F1 as a regression guard: host key MUST be injected
// via ssh_keys, and we must NEVER fall back to the racy write_files approach.
func TestBuildCloudInit_UsesSshKeysNotWriteFiles(t *testing.T) {
	priv := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNz\nQUJD\n-----END OPENSSH PRIVATE KEY-----"
	out := BuildCloudInit(priv, "ssh-ed25519 AAAAHOST host", "ssh-ed25519 BBBBLOGIN login")

	for _, want := range []string{
		"#cloud-config",
		"ssh_keys:",
		"  ed25519_private: |",
		"    -----BEGIN OPENSSH PRIVATE KEY-----", // 4-space indented under the block scalar
		"    QUJD",
		"  ed25519_public: ssh-ed25519 AAAAHOST host",
		"ssh_authorized_keys:",
		"  - ssh-ed25519 BBBBLOGIN login",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init missing %q\n--- got ---\n%s", want, out)
		}
	}

	if strings.Contains(out, "write_files") {
		t.Error("cloud-init must NOT use write_files for host keys (S1 finding F1)")
	}
}
