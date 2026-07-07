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
	// AuditLog, if set, installs auditd with a baseline ruleset (identity files,
	// sshd config, privilege-escalation binaries) — a tamper-evident on-node
	// forensic trail for nodes left up after a crash for debugging (S-F).
	AuditLog bool
	// SysctlHardening, if set, applies a CIS-lite kernel network baseline (ignore
	// ICMP redirects / source-routed packets, SYN-cookies, log martians). Uses
	// LOOSE reverse-path filtering (rp_filter=2) — strict (1) breaks WireGuard's
	// policy routing.
	SysctlHardening bool
	// EncryptWorkspace, if set, mounts an encrypted (LUKS2) filesystem at the run
	// user's workspace, so synced code and build artifacts are encrypted at rest
	// (S-E). The key is generated into tmpfs (RAM) at boot and never touches the
	// persistent disk — so a stolen/imaged disk yields only ciphertext, and the
	// volume is intentionally unrecoverable after a reboot (fine for ephemeral nodes).
	EncryptWorkspace bool
	// L2Script, if set, is a shell script that brings up the Layer-2 VXLAN
	// interface (security.overlay: l2). It is written to disk and run at boot
	// AFTER wg-quick@wg0, because vxlan0 rides wg0. The peer FDB is injected later,
	// at the barrier (peers aren't known at boot).
	L2Script string
}

// DefaultToolchain is Pandion's C++ toolchain per the Execution Contract (§5):
// gcc/g++/make (build-essential), clang, cmake, gdb, gdbserver (for shared
// debug-attach, `debug share`), plus tmux for `attach`. gdbserver is installed
// here — in the build window, while egress is open — because a locked-down node
// cannot apt-install it later.
//
// NOTE (reproducibility, H2): these are unpinned for M1. Version pinning +
// lockfile recording is a later refinement; pass explicit "pkg=version" strings
// here to pin.
func DefaultToolchain() []string {
	return []string{"build-essential", "clang", "cmake", "gdb", "gdbserver", "tmux"}
}

// ResolveToolchain returns the apt packages to install on a node: the built-in
// C++ toolchain PLUS the caller's declared extras (libraries/tools), de-duplicated
// and order-preserving. When noDefault is set the built-in toolchain is omitted and
// only the declared extras are returned (a minimal node). This is the single place
// that decides "what libraries land on the node", shared by the single-node and
// cluster paths so `--packages` and `toolchain.packages` behave identically.
func ResolveToolchain(declared []string, noDefault bool) []string {
	var base []string
	if !noDefault {
		base = DefaultToolchain()
	}
	seen := make(map[string]bool, len(base)+len(declared))
	out := make([]string, 0, len(base)+len(declared))
	for _, p := range append(base, declared...) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Build produces cloud-init user-data.
//
// Host key injection uses the cloud-init `ssh_keys:` module — NOT `write_files`
// + `systemctl restart ssh`, which races with cloud-init and socket-activated
// sshd and hangs (spike S1 finding F1).
func Build(ci CloudInit) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")

	// effective package list: the requested toolchain, plus the hardening daemons.
	pkgs := append([]string{}, ci.Packages...)
	if ci.Fail2ban {
		pkgs = append(pkgs, "fail2ban")
	}
	if ci.AuditLog {
		pkgs = append(pkgs, "auditd")
	}
	if ci.EncryptWorkspace {
		pkgs = append(pkgs, "cryptsetup")
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

	// auditd: install a baseline ruleset, enable the service, and load the rules
	// without a reboot (S-F). Best-effort — augenrules failure never blocks boot.
	if ci.AuditLog {
		files = append(files, wf{"/etc/audit/rules.d/pandion.rules", "0640", auditRules()})
		runcmds = append(runcmds,
			"[ bash, -c, \"systemctl enable --now auditd 2>/dev/null; augenrules --load 2>/dev/null || true\" ]")
	}

	// sysctl network hardening: write the baseline and apply it now (no reboot).
	if ci.SysctlHardening {
		files = append(files, wf{"/etc/sysctl.d/99-pandion-hardening.conf", "0644", sysctlHardening()})
		runcmds = append(runcmds, "[ sysctl, --system ]")
	}

	// encrypted workspace (S-E): set up + mount a LUKS volume at the run user's
	// workspace. Placed AFTER useradd so the home directory exists.
	if ci.EncryptWorkspace {
		files = append(files, wf{"/usr/local/bin/pandion-encfs", "0755", encfsScript()})
		runcmds = append(runcmds,
			fmt.Sprintf("[ bash, /usr/local/bin/pandion-encfs, %s ]", workspacePath(ci.RunUser)))
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

	// Layer-2 overlay (security.overlay: l2): bring up vxlan0 AFTER wg0 is up, since
	// it rides wg0. Written as a script and invoked (like the encfs pattern).
	if ci.L2Script != "" {
		files = append(files, wf{"/usr/local/bin/pandion-l2-up", "0755", ci.L2Script})
		runcmds = append(runcmds, "[ bash, /usr/local/bin/pandion-l2-up ]")
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

// auditRules is a conservative baseline: watch the identity files, the sshd
// config, and privilege-escalation binaries. Deliberately NOT logging every
// execve (too noisy) — this is a targeted, low-overhead forensic trail (S-F).
func auditRules() string {
	return `## Pandion baseline audit policy (S-F)
-D
-b 8192
-w /etc/passwd -p wa -k pandion_identity
-w /etc/shadow -p wa -k pandion_identity
-w /etc/ssh/sshd_config -p wa -k pandion_sshd
-w /usr/bin/sudo -p x -k pandion_priv
-w /usr/bin/su -p x -k pandion_priv
`
}

// sysctlHardening is a CIS-lite kernel network baseline. rp_filter is LOOSE (2),
// NOT strict (1): strict reverse-path filtering drops WireGuard's asymmetrically
// routed packets and breaks the overlay. Nothing here touches ip_forward (the
// nodes are WG endpoints, not routers), so the mesh is unaffected.
func sysctlHardening() string {
	return `# Pandion network hardening baseline
net.ipv4.conf.all.rp_filter = 2
net.ipv4.conf.default.rp_filter = 2
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.default.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv6.conf.all.accept_source_route = 0
net.ipv4.tcp_syncookies = 1
net.ipv4.conf.all.log_martians = 1
`
}

// workspacePath mirrors the CLI's workspace-dir logic: the run user's home (or
// /root), so the encrypted mount lands where synced code + build artifacts live.
func workspacePath(runUser string) string {
	if runUser == "" || runUser == "root" {
		return "/root/workspace"
	}
	return "/home/" + runUser + "/workspace"
}

// encfsScript sets up a LUKS2 volume backed by a file and mounts it at $1. The
// key lives ONLY in tmpfs (/run, RAM) — never on the persistent disk — so the
// data is encrypted at rest and the volume is deliberately unrecoverable after a
// reboot. Idempotent. chowns the mount to the workspace's owner directory user.
func encfsScript() string {
	return `#!/bin/sh
set -e
WORKDIR="$1"
KEY=/run/pandion-luks.key   # tmpfs (RAM) — never persisted to disk
IMG=/var/lib/pandion-enc.img
[ -e /dev/mapper/pandion_enc ] && exit 0   # already set up
head -c 64 /dev/urandom > "$KEY"; chmod 600 "$KEY"
fallocate -l 2G "$IMG" 2>/dev/null || dd if=/dev/zero of="$IMG" bs=1M count=2048
cryptsetup luksFormat --batch-mode --type luks2 --key-file "$KEY" "$IMG"
cryptsetup luksOpen --key-file "$KEY" "$IMG" pandion_enc
mkfs.ext4 -q /dev/mapper/pandion_enc
mkdir -p "$WORKDIR"
mount /dev/mapper/pandion_enc "$WORKDIR"
# match ownership to the parent (home) so the run user can write to it
owner=$(stat -c '%U' "$(dirname "$WORKDIR")" 2>/dev/null || echo root)
chown "$owner:$owner" "$WORKDIR" 2>/dev/null || true
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
