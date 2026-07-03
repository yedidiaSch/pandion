// Package harden builds the provision-time hardening artifacts. M1 ships the
// cloud-init builder for SSH host-key injection and C++ toolchain install;
// firewall/overlay/LUKS land in M2.
package harden

import (
	"fmt"
	"strings"
	"time"
)

// DefaultIdleTTL is the default abandoned-node poweroff window (P2b, security).
const DefaultIdleTTL = 60 * time.Minute

// DefaultRunUser is the unprivileged user that runs workloads (S-C).
const DefaultRunUser = "pandion-run"

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
	// IdleTTL, if > 0, installs an on-node dead-man's-switch: the node powers
	// itself off after this long with no active SSH (P2b, security). 0 disables.
	IdleTTL time.Duration
	// Fail2ban, if set, installs fail2ban and enables the sshd jail (systemd
	// backend) — defense-in-depth against SSH brute-force/scanning (P1). Bans on
	// FAILED auth only, so the key-holding operator is never affected.
	Fail2ban bool
}

// DefaultToolchain is Pandion's C++ toolchain per the Execution Contract (§5):
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

	// effective package list: the requested toolchain, plus fail2ban when enabled.
	pkgs := ci.Packages
	if ci.Fail2ban {
		pkgs = append(append([]string{}, pkgs...), "fail2ban")
	}
	if len(pkgs) > 0 {
		b.WriteString("package_update: true\n")
		b.WriteString("packages:\n")
		for _, p := range pkgs {
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

	// Accumulate write_files + runcmd entries, then emit them once.
	type wf struct{ path, perms, content string }
	var files []wf
	var runcmds []string

	if ci.WGConfig != "" {
		files = append(files, wf{"/etc/wireguard/wg0.conf", "0600", ci.WGConfig})
	}

	// fail2ban: enable the sshd jail using the systemd journal backend (Ubuntu 24.04
	// has no /var/log/auth.log by default), then enable the service (P1, defense-in-depth).
	if ci.Fail2ban {
		files = append(files, wf{"/etc/fail2ban/jail.local", "0644", fail2banJail()})
		runcmds = append(runcmds, "[ systemctl, enable, --now, fail2ban ]")
	}
	if u := strings.TrimSpace(ci.RunUser); u != "" && u != "root" {
		runcmds = append(runcmds,
			fmt.Sprintf("[ bash, -c, \"id -u %s >/dev/null 2>&1 || useradd -m -s /bin/bash %s\" ]", u, u))
	}

	// Idle dead-man's-switch (P2b, security): a node with no active SSH for IdleTTL
	// powers itself off — an abandoned/runaway node stops executing. NOTE: this is
	// a SECURITY control, not cost control (a stopped Hetzner server still bills;
	// `pandion reap` handles deletion). 0 disables.
	if ci.IdleTTL > 0 {
		ttlSec := int(ci.IdleTTL.Seconds())
		files = append(files,
			wf{"/usr/local/bin/pandion-deadman", "0755", deadmanScript(ttlSec)},
			wf{"/etc/systemd/system/pandion-deadman.service", "0644", deadmanService()},
			wf{"/etc/systemd/system/pandion-deadman.timer", "0644", deadmanTimer()})
		runcmds = append(runcmds,
			"[ bash, -c, \"mkdir -p /run/pandion && touch /run/pandion/heartbeat\" ]",
			"[ systemctl, enable, --now, pandion-deadman.timer ]")
	}

	if ci.WGConfig != "" {
		runcmds = append(runcmds, "[ systemctl, enable, --now, wg-quick@wg0 ]")
	}

	if len(files) > 0 {
		b.WriteString("write_files:\n")
		for _, f := range files {
			b.WriteString("  - path: " + f.path + "\n")
			b.WriteString("    permissions: '" + f.perms + "'\n")
			b.WriteString("    content: |\n")
			for _, line := range strings.Split(strings.TrimRight(f.content, "\n"), "\n") {
				b.WriteString("      ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
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

// fail2banJail enables the sshd jail on the systemd journal backend (no
// /var/log/auth.log on modern Ubuntu) with conservative ban settings.
func fail2banJail() string {
	return `[sshd]
enabled = true
backend = systemd
maxretry = 5
findtime = 10m
bantime = 1h
`
}

// deadmanScript touches the heartbeat while an SSH connection is established, and
// powers the node off once the heartbeat is older than ttlSec (idle).
func deadmanScript(ttlSec int) string {
	return fmt.Sprintf(`#!/bin/sh
TTL=%d
HB=/run/pandion/heartbeat
# fresh while any SSH connection is established (incl. a long streaming session)
if ss -Htn state established '( sport = :22 )' 2>/dev/null | grep -q .; then
  mkdir -p /run/pandion && touch "$HB"
fi
now=$(date +%%s)
last=$(stat -c %%Y "$HB" 2>/dev/null || echo "$now")
if [ $((now - last)) -gt "$TTL" ]; then
  logger "pandion dead-man: idle > ${TTL}s, powering off"
  systemctl poweroff
fi
`, ttlSec)
}

func deadmanService() string {
	return `[Unit]
Description=Pandion idle dead-man's-switch
[Service]
Type=oneshot
ExecStart=/usr/local/bin/pandion-deadman
`
}

func deadmanTimer() string {
	return `[Unit]
Description=Run the Pandion dead-man's-switch every minute
[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
[Install]
WantedBy=timers.target
`
}
