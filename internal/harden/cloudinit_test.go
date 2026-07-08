// SPDX-License-Identifier: AGPL-3.0-or-later

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

func TestBuild_SysctlHardening(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM:  "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:      "ssh-ed25519 AAAA host",
		SysctlHardening: true,
	})
	for _, want := range []string{
		"/etc/sysctl.d/99-pandion-hardening.conf",
		"net.ipv4.tcp_syncookies = 1",
		"net.ipv4.conf.all.accept_redirects = 0",
		"[ sysctl, --system ]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sysctl cloud-init missing %q\n%s", want, out)
		}
	}
	// CRITICAL: rp_filter must be LOOSE (2), never strict (1) — strict breaks WireGuard.
	if strings.Contains(out, "rp_filter = 1") {
		t.Error("rp_filter must be LOOSE (2); strict (1) breaks the WireGuard overlay")
	}
	if !strings.Contains(out, "rp_filter = 2") {
		t.Error("expected loose rp_filter (2)")
	}
	// must not touch ip_forward (nodes are WG endpoints, not routers)
	if strings.Contains(out, "ip_forward") {
		t.Error("must not set ip_forward (would affect WG routing)")
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

func TestBuild_EncryptWorkspace(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM:   "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:       "ssh-ed25519 AAAA host",
		RunUser:          "pandion-run",
		EncryptWorkspace: true,
	})
	for _, want := range []string{
		"- cryptsetup",                               // package
		"/usr/local/bin/pandion-encfs",               // setup script written
		"cryptsetup luksFormat",                      // LUKS format
		"KEY=/run/pandion-luks.key",                  // key in tmpfs (RAM), not disk
		"pandion-encfs, /home/pandion-run/workspace", // mounted at the run user's workspace
	} {
		if !strings.Contains(out, want) {
			t.Errorf("encrypt-workspace cloud-init missing %q\n%s", want, out)
		}
	}
	// the LUKS key must NEVER be written to the persistent disk
	if strings.Contains(out, "KEY=/var") || strings.Contains(out, "KEY=/etc") {
		t.Error("LUKS key must live in tmpfs (/run), never on the persistent disk")
	}
}

func TestBuild_NoEncryptByDefault(t *testing.T) {
	out := Build(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----",
		HostPubKey:     "ssh-ed25519 AAAA host",
	})
	if strings.Contains(out, "cryptsetup") || strings.Contains(out, "pandion-encfs") {
		t.Errorf("encryption must be absent unless enabled (opt-in)\n%s", out)
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

func TestResolveToolchain_AddsToDefaultAndDedups(t *testing.T) {
	got := ResolveToolchain([]string{"libzmq3-dev", "cmake", " libboost-dev "}, false)
	// built-in toolchain present...
	for _, p := range DefaultToolchain() {
		if !containsStr(got, p) {
			t.Errorf("built-in package %q dropped: %v", p, got)
		}
	}
	// ...plus the declared libraries (trimmed)...
	for _, p := range []string{"libzmq3-dev", "libboost-dev"} {
		if !containsStr(got, p) {
			t.Errorf("declared package %q missing: %v", p, got)
		}
	}
	// ...and "cmake" (already in the default) appears exactly once.
	n := 0
	for _, p := range got {
		if p == "cmake" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("cmake should be de-duplicated, appears %d times: %v", n, got)
	}
}

func TestResolveToolchain_NoDefaultInstallsOnlyDeclared(t *testing.T) {
	got := ResolveToolchain([]string{"libzmq3-dev"}, true)
	if len(got) != 1 || got[0] != "libzmq3-dev" {
		t.Fatalf("no_default should install only declared packages, got: %v", got)
	}
	if containsStr(got, "build-essential") {
		t.Error("no_default must omit the built-in toolchain")
	}
}

func TestResolveToolchain_EmptyCases(t *testing.T) {
	if got := ResolveToolchain(nil, false); len(got) != len(DefaultToolchain()) {
		t.Errorf("no extras ⇒ just the default toolchain, got: %v", got)
	}
	if got := ResolveToolchain(nil, true); len(got) != 0 {
		t.Errorf("no_default + no extras ⇒ empty, got: %v", got)
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
