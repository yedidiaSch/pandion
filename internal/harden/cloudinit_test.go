package harden

import (
	"strings"
	"testing"
	"time"
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

func TestBuild_CreatesRunUser(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
		RunUser:        "pandion-run",
	})
	if !strings.Contains(out, "runcmd:") {
		t.Fatalf("expected runcmd for user creation:\n%s", out)
	}
	if !strings.Contains(out, "useradd -m -s /bin/bash pandion-run") {
		t.Errorf("run user not created:\n%s", out)
	}
}

func TestBuild_RootRunUser_NoUseradd(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
		RunUser:        "root",
	})
	if strings.Contains(out, "useradd") {
		t.Errorf("root run user must not create a user:\n%s", out)
	}
}

func TestBuild_IdleDeadman(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
		IdleTTL:        30 * time.Minute,
	})
	for _, want := range []string{
		"/usr/local/bin/pandion-deadman",
		"pandion-deadman.timer",
		"TTL=1800", // 30m in seconds
		"systemctl poweroff",
		"[ systemctl, enable, --now, pandion-deadman.timer ]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deadman cloud-init missing %q\n%s", want, out)
		}
	}
}

func TestBuild_Fail2ban(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
		Packages:       []string{"cmake"},
		Fail2ban:       true,
	})
	for _, want := range []string{
		"- fail2ban",                             // added to the package list
		"/etc/fail2ban/jail.local",               // jail config written
		"backend = systemd",                      // journal backend (no auth.log)
		"[sshd]",                                 // sshd jail enabled
		"[ systemctl, enable, --now, fail2ban ]", // service enabled
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fail2ban cloud-init missing %q\n%s", want, out)
		}
	}
}

func TestBuild_NoFail2banByDefault(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
	})
	if strings.Contains(out, "fail2ban") {
		t.Errorf("fail2ban must be absent unless enabled\n%s", out)
	}
}

func TestBuild_AuditLog(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
		AuditLog:       true,
	})
	for _, want := range []string{
		"- auditd",                         // package
		"/etc/audit/rules.d/pandion.rules", // rules file
		"-w /etc/shadow -p wa",             // a baseline watch
		"pandion_identity",                 // rule key
		"augenrules --load",                // loaded without reboot
	} {
		if !strings.Contains(out, want) {
			t.Errorf("auditd cloud-init missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "-S execve") {
		t.Error("baseline must NOT log every execve (too noisy)")
	}
}

func TestBuild_NoAuditByDefault(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
	})
	if strings.Contains(out, "auditd") {
		t.Errorf("auditd must be absent unless enabled\n%s", out)
	}
}

func TestBuild_NoDeadmanWhenZero(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
	})
	if strings.Contains(out, "pandion-deadman") {
		t.Errorf("no deadman expected when IdleTTL=0:\n%s", out)
	}
}
