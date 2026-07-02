// Package ssh runs commands on a node over SSH with a PINNED host key.
//
// Security (spike S1): the host key is one EnvCore generated and injected via
// cloud-init, so we pin it with gossh.FixedHostKey. A node presenting any other
// key — a MITM, or a not-yet-hardened boot image — is rejected. EnvCore never
// uses an accept-any host-key callback.
package ssh

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// pinnedCallback returns a host-key verifier that accepts ONLY the pinned key.
// Exposed (unexported) so it can be unit-tested offline without a live server.
func pinnedCallback(pinned gossh.PublicKey) gossh.HostKeyCallback {
	return gossh.FixedHostKey(pinned)
}

// Run dials addr ("host:port"), authenticates with signer, verifies the host
// key against pinned, runs cmd, and returns combined stdout+stderr.
func Run(ctx context.Context, addr, user string, signer gossh.Signer, pinned gossh.PublicKey, cmd string) (string, error) {
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: pinnedCallback(pinned),
		Timeout:         10 * time.Second,
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", addr, err)
	}
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	client := gossh.NewClient(c, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// RunWithInput runs cmd with stdin fed from r (e.g. streaming a tar archive to
// `tar -x` on the node), returning combined stdout+stderr. Same pinned-host-key
// verification as Run.
func RunWithInput(ctx context.Context, addr, user string, signer gossh.Signer, pinned gossh.PublicKey, cmd string, r io.Reader) (string, error) {
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: pinnedCallback(pinned),
		Timeout:         10 * time.Second,
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", addr, err)
	}
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	client := gossh.NewClient(c, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var out bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &out
	stdin, err := sess.StdinPipe()
	if err != nil {
		return "", err
	}
	if err := sess.Start(cmd); err != nil {
		return "", fmt.Errorf("start %q: %w", cmd, err)
	}
	if _, err := io.Copy(stdin, r); err != nil {
		_ = stdin.Close()
		return out.String(), fmt.Errorf("stream stdin: %w", err)
	}
	if err := stdin.Close(); err != nil {
		return out.String(), err
	}
	return out.String(), sess.Wait()
}

// Stream runs cmd and streams stdout/stderr line-by-line to onLine (streamName
// is "out" or "err"), returning the command's exit error when it finishes. The
// session is torn down if ctx is cancelled (used to detach on Ctrl+C — C3).
func Stream(ctx context.Context, addr, user string, signer gossh.Signer, pinned gossh.PublicKey, cmd string, onLine func(streamName, line string)) error {
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: pinnedCallback(pinned),
		Timeout:         10 * time.Second,
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	c, chans, reqs, err := gossh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	client := gossh.NewClient(c, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return err
	}
	if err := sess.Start(cmd); err != nil {
		return fmt.Errorf("start %q: %w", cmd, err)
	}

	// cancel -> close the session so blocked reads unwind (detach on Ctrl+C).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-stop:
		}
	}()

	var wg sync.WaitGroup
	scan := func(r interface{ Read([]byte) (int, error) }, name string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			onLine(name, sc.Text())
		}
	}
	wg.Add(2)
	go scan(stdout, "out")
	go scan(stderr, "err")
	wg.Wait()

	return sess.Wait() // exit error (e.g. *ExitError with code, incl. 139)
}

// Classify maps a connection error to a short, human diagnosis so retries are
// not a silent black box. Distinguishes the expected provisioning-window states
// from a genuine misconfiguration.
func Classify(err error) string {
	if err == nil {
		return "ok"
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "host key"):
		return "waiting for host key (cloud-init applying)"
	case strings.Contains(m, "unable to authenticate"), strings.Contains(m, "no supported methods"):
		return "auth not ready (login key not yet on root)"
	case strings.Contains(m, "refused"), strings.Contains(m, "no route"), strings.Contains(m, "timeout"), strings.Contains(m, "i/o timeout"):
		return "sshd not up yet"
	default:
		return "error: " + err.Error()
	}
}

// RunWithRetry retries Run until it succeeds or ctx expires. This tolerates the
// provisioning window: until cloud-init installs our host key, the node presents
// a different key and pinning (correctly) rejects it — so we wait and retry
// rather than disabling verification (S1/F4). onAttempt (optional) is called
// after each failed attempt with the attempt number and a classified reason.
func RunWithRetry(ctx context.Context, addr, user string, signer gossh.Signer, pinned gossh.PublicKey, cmd string, every time.Duration, onAttempt func(attempt int, reason string)) (string, error) {
	var lastErr error
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("gave up after %v: %w", ctx.Err(), lastErr)
			}
			return "", ctx.Err()
		default:
		}
		out, err := Run(ctx, addr, user, signer, pinned, cmd)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if onAttempt != nil {
			onAttempt(attempt, Classify(err))
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("gave up after %v: %w", ctx.Err(), lastErr)
		case <-time.After(every):
		}
	}
}
