// SPDX-License-Identifier: AGPL-3.0-or-later

package relay

import (
	"fmt"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// SSHDialer is the production Dialer: it opens a host-key-pinned SSH connection to
// the session's target node over the overlay, using the session's scoped key, and
// requests an interactive PTY. The relay node is itself on the overlay, so it
// reaches the target at its 10.99.0.x address directly.
func SSHDialer(s *Session) (PTYConn, error) {
	signer, err := gossh.ParsePrivateKey([]byte(s.SSHKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("scoped key: %w", err)
	}
	hostKey, _, _, _, err := gossh.ParseAuthorizedKey([]byte(s.HostPub))
	if err != nil {
		return nil, fmt.Errorf("pinned host key: %w", err)
	}
	cfg := &gossh.ClientConfig{
		User:            s.User,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.FixedHostKey(hostKey), // MITM-proof: exact pin
		Timeout:         10 * time.Second,
	}
	client, err := gossh.Dial("tcp", s.Target+":22", cfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", s.Target, err)
	}
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	modes := gossh.TerminalModes{gossh.ECHO: 1, gossh.TTY_OP_ISPEED: 14400, gossh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	if err := sess.Shell(); err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	return &sshPTY{client: client, sess: sess, in: stdin, out: stdout}, nil
}

// sshPTY adapts an SSH PTY session to PTYConn. With a PTY requested, the remote
// merges stdout+stderr onto the terminal, so out carries everything.
type sshPTY struct {
	client *gossh.Client
	sess   *gossh.Session
	in     interface{ Write([]byte) (int, error) }
	out    interface{ Read([]byte) (int, error) }
}

func (p *sshPTY) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *sshPTY) Write(b []byte) (int, error) { return p.in.Write(b) }

// Resize forwards a window change (SSH takes height=rows, width=cols).
func (p *sshPTY) Resize(rows, cols int) error { return p.sess.WindowChange(rows, cols) }

func (p *sshPTY) Close() error {
	_ = p.sess.Close()
	return p.client.Close()
}
