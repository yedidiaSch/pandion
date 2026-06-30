// Package harden builds the provision-time hardening artifacts. M0 ships the
// cloud-init builder for SSH host-key injection; firewall/overlay/LUKS land in M1–M2.
package harden

import "strings"

// BuildCloudInit produces cloud-init user-data that injects a KNOWN SSH host key
// via the cloud-init `ssh_keys:` module.
//
// Spike S1 finding F1 (validated 2026-06-30): this is the race-free method.
// Do NOT use `write_files` + `systemctl restart ssh` — it races with cloud-init's
// own host-key handling and Ubuntu 24.04's socket-activated sshd, and hangs.
//
//	hostPrivKeyPEM — the OpenSSH-format ed25519 private key (multi-line)
//	hostPubKey     — "ssh-ed25519 AAAA... [comment]"
//	loginPubKey    — optional client login public key for ssh_authorized_keys
func BuildCloudInit(hostPrivKeyPEM, hostPubKey, loginPubKey string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_keys:\n")
	b.WriteString("  ed25519_private: |\n")
	for _, line := range strings.Split(strings.TrimRight(hostPrivKeyPEM, "\n"), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("  ed25519_public: ")
	b.WriteString(strings.TrimSpace(hostPubKey))
	b.WriteString("\n")

	if loginPubKey != "" {
		b.WriteString("ssh_authorized_keys:\n")
		b.WriteString("  - ")
		b.WriteString(strings.TrimSpace(loginPubKey))
		b.WriteString("\n")
	}
	return b.String()
}
