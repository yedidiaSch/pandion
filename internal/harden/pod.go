// SPDX-License-Identifier: AGPL-3.0-or-later

package harden

import (
	"fmt"
	"strings"
)

// Pod bootstrap: the container-provider analog of Build's cloud-init.
//
// A container (RunPod pod) has no cloud-init and no systemd, so Pandion starts
// it with this shell script as the container's command. It installs sshd with the
// host key Pandion generated (so the connection is pinned from the first byte,
// exactly as on a VM), authorizes the login key, exports the image's CUDA
// environment to SSH sessions, installs the toolchain + --packages, creates the
// unprivileged run user, and marks readiness for PodReadyCmd.
//
// What a container CANNOT get (no CAP_NET_ADMIN / systemd / block devices) is
// left out on purpose and reported by `up`: the nftables lockdown, WireGuard
// overlay, idle poweroff, fail2ban, auditd, sysctl hardening and LUKS.

// Pod bootstrap paths on the node.
const (
	PodReadyFile  = "/run/pandion/ready"
	PodFailedFile = "/run/pandion/failed"
	PodBootLog    = "/var/log/pandion-boot.log"
)

// PodBootEnv is the container env var carrying the base64 bootstrap script.
const PodBootEnv = "PANDION_BOOT"

// PodEntrypoint is the container command that decodes and runs the bootstrap
// (from PodBootEnv). The script ends by sleeping forever as PID 1, so the pod
// stays up until `pandion down` deletes it.
func PodEntrypoint() []string {
	return []string{"bash", "-c",
		`umask 077; printf %s "$` + PodBootEnv + `" | base64 -d > /root/.pandion-boot.sh && unset ` + PodBootEnv +
			` && exec bash /root/.pandion-boot.sh`}
}

// PodBootstrap renders the bootstrap shell script for a container node. It reads
// the host key, login key, packages and run user from ci; the VM-only hardening
// fields are ignored (see the package note above).
func PodBootstrap(ci CloudInit) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("#!/bin/bash\n# Pandion container-node bootstrap (no cloud-init in a pod).\nset -u\n")
	w("mkdir -p /run/pandion /var/log/pandion\nexec >>%s 2>&1\n", PodBootLog)
	w("echo \"pandion bootstrap start $(date -u +%%FT%%TZ)\"\n")
	w("export DEBIAN_FRONTEND=noninteractive\n")
	w("fail() { echo \"pandion bootstrap FAILED: $*\"; touch %s; exec sleep infinity; }\n", PodFailedFile)
	w("apt_install() { apt-get install -y -q --no-install-recommends \"$@\"; }\n")

	// 1) sshd, pinned to OUR host key; key-only root login.
	w("apt-get update -q || fail 'apt-get update'\n")
	w("apt_install openssh-server ca-certificates || fail 'install openssh-server'\n")
	w("rm -f /etc/ssh/ssh_host_*\n")
	w("cat > /etc/ssh/ssh_host_ed25519_key <<'PANDION_HOSTKEY'\n%s\nPANDION_HOSTKEY\n", strings.TrimRight(ci.HostPrivKeyPEM, "\n"))
	w("chmod 600 /etc/ssh/ssh_host_ed25519_key\n")
	w("printf '%%s\\n' %s > /etc/ssh/ssh_host_ed25519_key.pub\n", shq(strings.TrimSpace(ci.HostPubKey)))
	w("mkdir -p /root/.ssh && chmod 700 /root/.ssh\n")
	if k := strings.TrimSpace(ci.LoginPubKey); k != "" {
		w("printf '%%s\\n' %s >> /root/.ssh/authorized_keys\n", shq(k))
	}
	w("chmod 600 /root/.ssh/authorized_keys 2>/dev/null\n")
	w("mkdir -p /etc/ssh/sshd_config.d /run/sshd\n")
	w("cat > /etc/ssh/sshd_config.d/00-pandion.conf <<'PANDION_SSHD'\n" +
		"HostKey /etc/ssh/ssh_host_ed25519_key\n" +
		"PermitRootLogin prohibit-password\n" +
		"PasswordAuthentication no\n" +
		"KbdInteractiveAuthentication no\n" +
		"PANDION_SSHD\n")

	// 2) the image's CUDA environment for SSH sessions: sshd builds a fresh env, so
	//    without this nvcc / compute-sanitizer are not on PATH over SSH. Only an
	//    allowlist is exported — never the RUNPOD_* vars (they can carry API keys).
	w("for v in PATH LD_LIBRARY_PATH LIBRARY_PATH CUDA_HOME CUDA_VERSION NVIDIA_VISIBLE_DEVICES NVIDIA_DRIVER_CAPABILITIES NVIDIA_REQUIRE_CUDA; do\n" +
		"  val=\"${!v:-}\"; [ -n \"$val\" ] && printf '%%s=\"%%s\"\\n' \"$v\" \"$val\" >> /etc/environment\n" +
		"done\n")
	w("cat > /etc/profile.d/pandion-cuda.sh <<'PANDION_CUDA'\n" +
		"[ -d /usr/local/cuda/bin ] && case \":$PATH:\" in *:/usr/local/cuda/bin:*) ;; *) PATH=\"/usr/local/cuda/bin:$PATH\";; esac\n" +
		"[ -d /usr/local/cuda/lib64 ] && export LD_LIBRARY_PATH=\"/usr/local/cuda/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}\"\n" +
		"export PATH\n" +
		"PANDION_CUDA\n")
	w("/usr/sbin/sshd || fail 'start sshd'\n")
	w("echo 'sshd up'\n")

	// 3) toolchain + declared packages. tmux is required (`up` runs the workload in
	//    it). A bad package name must not sink the rest: on failure, retry one by
	//    one so `up` can name exactly what is missing (warnMissingPackages).
	pkgs := []string{"tmux"}
	for _, p := range ci.Packages {
		if p = strings.TrimSpace(p); p != "" && p != "tmux" {
			pkgs = append(pkgs, p)
		}
	}
	quoted := make([]string, len(pkgs))
	for i, p := range pkgs {
		quoted[i] = shq(p)
	}
	w("PKGS=(%s)\n", strings.Join(quoted, " "))
	w("if ! apt_install \"${PKGS[@]}\"; then\n" +
		"  for p in \"${PKGS[@]}\"; do apt_install \"$p\" || echo \"pandion: package $p did not install\"; done\n" +
		"fi\n")
	w("command -v tmux >/dev/null || fail 'tmux is required'\n")

	// 4) unprivileged run user (S-C), no sudo.
	if u := strings.TrimSpace(ci.RunUser); u != "" && u != "root" {
		w("id -u %s >/dev/null 2>&1 || useradd -m -s /bin/bash %s || fail 'create run user'\n", shq(u), shq(u))
	}

	w("touch %s\necho \"pandion bootstrap done $(date -u +%%FT%%TZ)\"\n", PodReadyFile)
	w("exec sleep infinity\n")
	return b.String()
}

// PodReadyCmd waits (on the node) until the bootstrap finished, printing the
// bootstrap log's tail and exiting non-zero if it failed. It returns as soon as
// either marker exists, so a broken bootstrap fails `up` fast instead of timing out.
func PodReadyCmd() string {
	return fmt.Sprintf("while :; do [ -e %s ] && exit 0; if [ -e %s ]; then tail -n 40 %s >&2; exit 1; fi; sleep 2; done",
		PodReadyFile, PodFailedFile, PodBootLog)
}

// shq single-quotes s for the shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
