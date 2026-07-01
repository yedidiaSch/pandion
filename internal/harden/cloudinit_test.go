package harden

import (
	"strings"
	"testing"
)

// Encodes spike S1 finding F1 as a regression guard: host key MUST be injected
// via ssh_keys, and we must NEVER fall back to the racy write_files approach.
func TestBuild_UsesSshKeysNotWriteFiles(t *testing.T) {
	priv := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNz\nQUJD\n-----END OPENSSH PRIVATE KEY-----"
	out := Build(CloudInit{
		HostPrivKeyPEM: priv,
		HostPubKey:     "ssh-ed25519 AAAAHOST host",
		LoginPubKey:    "ssh-ed25519 BBBBLOGIN login",
	})

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

func TestBuild_InstallsToolchainPackages(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
		Packages:       DefaultToolchain(),
	})
	if !strings.Contains(out, "packages:") || !strings.Contains(out, "package_update: true") {
		t.Fatalf("expected package install directives:\n%s", out)
	}
	for _, p := range []string{"build-essential", "clang", "cmake", "gdb", "tmux"} {
		if !strings.Contains(out, "  - "+p) {
			t.Errorf("toolchain missing package %q", p)
		}
	}
}

func TestBuild_NoPackagesSectionWhenEmpty(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
	})
	if strings.Contains(out, "packages:") {
		t.Errorf("no packages should be emitted when list is empty:\n%s", out)
	}
}
