// Package harden builds the provision-time hardening artifacts. M1 ships the
// cloud-init builder for SSH host-key injection and C++ toolchain install;
// firewall/overlay/LUKS land in M2.
package harden

import (
	"fmt"
	"strings"
)

// DefaultRunUser is the unprivileged user that runs workloads (S-C).
const DefaultRunUser = "envcore-run"

// CloudInit is the input to Build. It grows as later milestones add hardening
// (egress rules, firewall, LUKS) without changing call sites.
type CloudInit struct {
	// HostPrivKeyPEM is the OpenSSH-format ed25519 host private key, injected via
	// the cloud-init ssh_keys module (race-free — spike S1 finding F1).
	HostPrivKeyPEM string
	// HostPubKey is "ssh-ed25519 AAAA... [comment]".
	HostPubKey string
	// LoginPubKey, if set, is added to ssh_authorized_keys as a belt-and-suspenders
	// (the primary path registers it with the provider so it lands on root).
	LoginPubKey string
	// Packages are installed at first boot (e.g. the C++ toolchain). Empty = none.
	Packages []string
	// WGConfig, if set, is written to /etc/wireguard/wg0.conf and brought up at
	// boot via wg-quick@wg0. Rendered via write_files (finding F11), not inline
	// shell, which is fragile.
	WGConfig string
	// RunUser, if set and not "root", is created as an unprivileged login user
	// (no sudo) at boot. User workloads run as this user, not root (S-C).
	RunUser string
}

// DefaultToolchain is EnvCore's C++ toolchain per the Execution Contract (§5):
// gcc/g++/make (build-essential), clang, cmake, gdb, plus tmux for `attach`.
//
// NOTE (reproducibility, H2): these are unpinned for M1. Version pinning +
// lockfile recording is a later refinement; pass explicit "pkg=version" strings
// here to pin.
func DefaultToolchain() []string {
	return []string{"build-essential", "clang", "cmake", "gdb", "tmux"}
}

// Build produces cloud-init user-data.
//
// Host key injection uses the cloud-init `ssh_keys:` module — NOT `write_files`
// + `systemctl restart ssh`, which races with cloud-init and socket-activated
// sshd and hangs (spike S1 finding F1).
func Build(ci CloudInit) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")

	if len(ci.Packages) > 0 {
		b.WriteString("package_update: true\n")
		b.WriteString("packages:\n")
		for _, p := range ci.Packages {
			b.WriteString("  - ")
			b.WriteString(strings.TrimSpace(p))
			b.WriteString("\n")
		}
	}

	b.WriteString("ssh_keys:\n")
	b.WriteString("  ed25519_private: |\n")
	for _, line := range strings.Split(strings.TrimRight(ci.HostPrivKeyPEM, "\n"), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("  ed25519_public: ")
	b.WriteString(strings.TrimSpace(ci.HostPubKey))
	b.WriteString("\n")

	if ci.LoginPubKey != "" {
		b.WriteString("ssh_authorized_keys:\n")
		b.WriteString("  - ")
		b.WriteString(strings.TrimSpace(ci.LoginPubKey))
		b.WriteString("\n")
	}

	if ci.WGConfig != "" {
		// write_files with a block scalar (F11): render the wg0.conf indented.
		b.WriteString("write_files:\n")
		b.WriteString("  - path: /etc/wireguard/wg0.conf\n")
		b.WriteString("    permissions: '0600'\n")
		b.WriteString("    content: |\n")
		for _, line := range strings.Split(strings.TrimRight(ci.WGConfig, "\n"), "\n") {
			b.WriteString("      ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	// Accumulate runcmd entries in order: create the unprivileged run user first
	// (workloads run as it, not root — S-C), then bring the overlay up.
	var runcmds []string
	if u := strings.TrimSpace(ci.RunUser); u != "" && u != "root" {
		runcmds = append(runcmds,
			fmt.Sprintf("[ bash, -c, \"id -u %s >/dev/null 2>&1 || useradd -m -s /bin/bash %s\" ]", u, u))
	}
	if ci.WGConfig != "" {
		runcmds = append(runcmds, "[ systemctl, enable, --now, wg-quick@wg0 ]")
	}
	if len(runcmds) > 0 {
		b.WriteString("runcmd:\n")
		for _, rc := range runcmds {
			b.WriteString("  - ")
			b.WriteString(rc)
			b.WriteString("\n")
		}
	}
	return b.String()
}
