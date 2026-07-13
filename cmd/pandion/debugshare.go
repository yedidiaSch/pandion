// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/overlay"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	"github.com/yedidiaSch/pandion/internal/sshkeys"
	gossh "golang.org/x/crypto/ssh"
)

// Tier-2 SHARED debugging: hand a teammate one token that grants a scoped,
// expiring, revocable remote debug-attach to a running process — without root,
// without your overlay, and without a backend.
//
// The debugger on the node is `gdbserver --once --attach - <PID>` in STDIO mode,
// run (as root, via a locked sudoers rule) ONLY through a ForceCommand wrapper
// bound to one pinned PID. The guest's local gdb connects with `target remote |
// ssh …`, so the gdb remote protocol rides the same host-key-pinned SSH channel
// Pandion already uses — no open port, no forwarding, and it passes the deny-all
// firewall. gdbserver-as-root reads the target's memory correctly (a non-root
// capability-based gdb cannot, on hardened Ubuntu), yet only ever proxies that one
// process — no shell, no arbitrary-process access, no code-exec as root. The grant
// is a non-root `pandion-debug` user whose key is `restrict`ed + `expiry-time`d;
// `unshare`/`down` revoke it; every step is audit-logged.

const (
	debugUser     = "pandion-debug"
	debugForced   = "/usr/local/bin/pandion-debug-forced" // ForceCommand wrapper
	debugSudoers  = "/etc/sudoers.d/pandion-debug"
	sharesEtcDir  = "/etc/pandion/shares" // per-share pinned target PID (trusted)
	gdbserverBin  = "/usr/bin/gdbserver"
	shareMarker   = "pandion-share-" // + shareID, the authorized_keys comment
	guestIPHi     = 252              // guest overlay IPs live in 10.99.0.240..252
	guestIPLo     = 240              // (operator is .254, nodes .1..N)
	tokenPrefix   = "PDBG1-"
	defaultExpiry = 2 * time.Hour
)

// shareBundle is everything `join` needs to assemble the teammate's launch.json
// (paths are written locally by join, so it carries the ingredients, not paths).
type shareBundle struct {
	Version       int    `json:"v"`
	ClusterID     string `json:"id"`
	Node          string `json:"node"`
	ShareID       string `json:"share"`
	Expiry        string `json:"expiry"` // RFC3339
	User          string `json:"user"`   // remote SSH user (pandion-debug)
	NodeOverlayIP string `json:"node_overlay_ip"`
	Program       string `json:"program"` // remote binary basename hint (local symbols)
	WGConfig      string `json:"wg"`      // guest wg-quick config
	SSHKeyPEM     string `json:"ssh_key"` // guest private key (bearer secret)
	KnownHosts    string `json:"known_hosts"`
}

// shareRecord is the local bookkeeping `unshare`/`down` need to revoke a grant.
type shareRecord struct {
	ShareID     string `json:"share"`
	Node        string `json:"node"`
	NodeIP      string `json:"node_ip"`
	NodeHostPub string `json:"node_host_pub"`
	GuestWGPub  string `json:"guest_wg_pub"`
	GuestIP     string `json:"guest_ip"`
	Expiry      string `json:"expiry"`
}

// runDebugShare grants a teammate a scoped remote-debug attach and prints a token.
//
//	pandion debug share --id ID [--node N] (--pid P | --program NAME) [--expires 2h]
func runDebugShare(args []string) {
	fs := newCmdFlagSet("debug share")
	id := fs.String("id", "", "cluster id (required)")
	node := fs.String("node", "", "node name (default: the first node)")
	pid := fs.Int("pid", 0, "the running PID to grant (required, or use --program)")
	program := fs.String("program", "", "resolve the PID by process name (owned by the run user)")
	expires := fs.Duration("expires", defaultExpiry, "how long the grant is valid")
	printOnly := fs.Bool("print", false, "print the token only (don't persist a share record)")
	_ = fs.Parse(args)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "debug share: --id is required")
		os.Exit(2)
	}
	if *pid == 0 && *program == "" {
		fmt.Fprintln(os.Stderr, "debug share: give --pid N or --program NAME (gdbserver attaches to one process)")
		os.Exit(2)
	}

	man, err := loadManifest(*id)
	if err != nil {
		bailIfTornDown(err)
		fmt.Fprintf(os.Stderr, "debug share: no manifest for %q: %v\n", *id, err)
		os.Exit(3)
	}
	target, ok := pickNode(man.Nodes, *node)
	if !ok {
		fmt.Fprintf(os.Stderr, "debug share: node not found in cluster %q\n", *id)
		os.Exit(3)
	}
	if target.OverlayIP == "" {
		fmt.Fprintln(os.Stderr, "debug share: node has no overlay IP (shared debug rides the overlay)")
		os.Exit(3)
	}
	signer, pinned := operatorConn(*id, target)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// the node's WireGuard public key (not in the manifest) — for the guest config.
	nodeWGPub, err := envssh.Run(ctx, target.IP+":22", "root", signer, pinned, "wg show wg0 public-key")
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug share: could not read node WireGuard key: %v\n", err)
		os.Exit(3)
	}
	nodeWGPub = strings.TrimSpace(nodeWGPub)

	// resolve + validate the target PID on the node: it must be owned by a non-root
	// (>=1000) user, so a grant can never target root/system processes.
	targetPID, err := resolveTargetPID(ctx, target, signer, pinned, *pid, *program)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug share: %v\n", err)
		os.Exit(3)
	}

	shareID := randID()
	guestIP, err := allocGuestIP(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug share: %v\n", err)
		os.Exit(3)
	}
	guestWG, err := overlay.GenerateKeypair()
	must(err)
	guestSSH, err := sshkeys.Generate("pandion-debug-" + shareID)
	must(err)
	expiry := time.Now().Add(*expires).UTC()

	// provision the scoped grant on the node (idempotent).
	script := provisionScript(guestSSH.PublicAuthorized, expiry, shareID, guestWG.Public, guestIP, targetPID)
	if _, err := envssh.Run(ctx, target.IP+":22", "root", signer, pinned, script); err != nil {
		fmt.Fprintf(os.Stderr, "debug share: provisioning the grant failed: %v\n", err)
		os.Exit(3)
	}

	guestWGConf := overlay.OperatorConfigMulti(guestWG.Private, guestIP+"/32", []overlay.OperatorPeer{{
		PubKey:    nodeWGPub,
		Endpoint:  fmt.Sprintf("%s:%d", target.IP, overlay.DefaultPort),
		AllowedIP: target.OverlayIP + "/32",
	}})
	bundle := shareBundle{
		Version: 1, ClusterID: *id, Node: target.Name, ShareID: shareID,
		Expiry: expiry.Format(time.RFC3339), User: debugUser, NodeOverlayIP: target.OverlayIP,
		Program: programHint(*program), WGConfig: guestWGConf, SSHKeyPEM: guestSSH.PrivatePEM,
		KnownHosts: knownHostsLine(target.OverlayIP, target.HostPub),
	}
	token, err := packToken(bundle)
	must(err)

	if !*printOnly {
		must(saveShareRecord(*id, shareRecord{
			ShareID: shareID, Node: target.Name, NodeIP: target.IP, NodeHostPub: target.HostPub,
			GuestWGPub: guestWG.Public, GuestIP: guestIP, Expiry: expiry.Format(time.RFC3339),
		}))
	}
	audit.Event("debug.share", "id", *id, "node", target.Name, "share", shareID,
		"expiry", expiry.Format(time.RFC3339), "pid", targetPID)

	// The token embeds a private SSH key + WireGuard config — it IS the access.
	// Keep stdout token-ONLY (so `pandion debug share … | pbcopy` is script-safe),
	// and put the secret warning + instructions on stderr (P3.4).
	exp := expiry.Format("2006-01-02 15:04 MST")
	fmt.Fprintf(os.Stderr, "⚠ this token IS the access — anyone holding it can attach to PID %d on %q/%s until %s.\n", targetPID, *id, target.Name, exp)
	fmt.Fprintln(os.Stderr, "  share it over a PRIVATE channel (not email/Slack in the clear); it can't be rotated, only revoked.")
	fmt.Fprintln(os.Stderr, "  the teammate runs:  pandion debug join <token>")
	fmt.Fprintf(os.Stderr, "  revoke any time:    pandion debug unshare --id %s --share %s   (or --all)\n\n", *id, shareID)
	fmt.Println(token) // stdout: the token, and nothing else
}

// resolveTargetPID returns the PID to grant: the explicit --pid, else the newest
// process named --program owned by the run user. It refuses a root/system-owned
// PID so a share can never target privileged processes.
func resolveTargetPID(ctx context.Context, node nodeManifest, signer gossh.Signer, pinned gossh.PublicKey, pid int, program string) (int, error) {
	got := pid
	if got == 0 {
		out, err := envssh.Run(ctx, node.IP+":22", "root", signer, pinned,
			"pgrep -u "+pgrepRunUser(program)+" 2>/dev/null | head -1")
		if err != nil {
			return 0, fmt.Errorf("resolve --program %q: %w", program, err)
		}
		got, _ = strconv.Atoi(strings.TrimSpace(out))
		if got == 0 {
			return 0, fmt.Errorf("no process %q owned by the run user is running", program)
		}
	}
	// verify the target is a non-root/system process (uid >= 1000).
	out, err := envssh.Run(ctx, node.IP+":22", "root", signer, pinned,
		fmt.Sprintf("stat -c %%u /proc/%d 2>/dev/null || echo 0", got))
	if err != nil {
		return 0, fmt.Errorf("inspect PID %d: %w", got, err)
	}
	uid, _ := strconv.Atoi(strings.TrimSpace(out))
	if uid < 1000 {
		return 0, fmt.Errorf("PID %d is owned by a system/root user (uid %d) — refusing to share it", got, uid)
	}
	return got, nil
}

// pgrepRunUser builds a safe `pgrep` selector for a process owned by the run user,
// sanitizing the name to a safe subset (alnum, ., _, -) to avoid shell injection.
func pgrepRunUser(name string) string {
	name = filepath.Base(name)
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return -1
	}, name)
	if safe == "" {
		safe = "pandion-run"
	}
	// -u <run user> -x <name>: match by exact process name owned by pandion-run.
	return "pandion-run -x " + safe
}

func programHint(program string) string {
	if program == "" {
		return "a.out"
	}
	return filepath.Base(program)
}

// runDebugJoin materializes a share token on the teammate's machine.
//
//	pandion debug join <token>
func runDebugJoin(args []string) {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(os.Stderr, "usage: pandion debug join <token>")
		os.Exit(2)
	}
	bundle, err := unpackToken(strings.TrimSpace(args[0]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug join: invalid token: %v\n", err)
		os.Exit(2)
	}
	if exp, err := time.Parse(time.RFC3339, bundle.Expiry); err == nil && time.Now().After(exp) {
		fmt.Fprintf(os.Stderr, "debug join: this token expired at %s — ask for a fresh one.\n", bundle.Expiry)
		os.Exit(4)
	}

	dir := filepath.Join(pandionDir(), "guest", bundle.ClusterID+"-"+bundle.Node)
	must(os.MkdirAll(dir, 0o700))
	keyPath := filepath.Join(dir, "id_ed25519")
	khPath := filepath.Join(dir, "known_hosts")
	confPath := filepath.Join(dir, "wg.conf")
	must(os.WriteFile(keyPath, []byte(bundle.SSHKeyPEM), 0o600))
	must(os.WriteFile(khPath, []byte(bundle.KnownHosts), 0o600))
	must(os.WriteFile(confPath, []byte(bundle.WGConfig), 0o600))

	cfg := buildServerAttachConfig(bundle, keyPath, khPath)
	launchPath := filepath.Join(".vscode", "launch.json")
	created, dropped, merr := mergeLaunchJSON(launchPath, cfg)

	fmt.Printf("joined shared debug for %q/%s (expires %s).\n", bundle.ClusterID, bundle.Node, bundle.Expiry)
	fmt.Printf("\n# 1. bring up the scoped overlay peer (needs WireGuard + sudo):\n")
	fmt.Printf("#      sudo wg-quick up %s\n", confPath)
	switch {
	case merr != nil:
		fmt.Printf("# 2. couldn't merge launch.json (%v) — see %s\n", merr, dir)
	case created:
		fmt.Printf("# 2. created %s with %q\n", launchPath, cfg["name"])
	default:
		fmt.Printf("# 2. merged %q into %s%s\n", cfg["name"], launchPath,
			map[bool]string{true: " (comments dropped)", false: ""}[dropped])
	}
	fmt.Printf("# 3. set \"program\" to your LOCAL binary with symbols, then F5.\n")
	fmt.Printf("#    (the debug session rides the pinned SSH pipe to a root gdbserver on the node.)\n")
}

// buildServerAttachConfig builds a cppdbg config that connects a LOCAL gdb to the
// node's gdbserver over the pinned SSH channel (`target remote | ssh …`). This is
// the mechanism that actually reads process memory: gdbserver runs privileged on
// the node, the client just speaks the gdb remote protocol over SSH.
func buildServerAttachConfig(b shareBundle, keyPath, khPath string) map[string]any {
	sshCmd := strings.Join([]string{
		"ssh", "-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + khPath,
		"-o", "BatchMode=yes",
		b.User + "@" + b.NodeOverlayIP,
	}, " ")
	return map[string]any{
		"name":           "Pandion shared debug: " + b.ClusterID + "-" + b.Node,
		"type":           "cppdbg",
		"request":        "launch",
		"program":        "${workspaceFolder}/" + b.Program,
		"cwd":            "${workspaceFolder}",
		"MIMode":         "gdb",
		"miDebuggerPath": "/usr/bin/gdb",
		"customLaunchSetupCommands": []map[string]any{
			{"text": "target remote | " + sshCmd, "description": "connect to the node's gdbserver over the pinned SSH pipe"},
		},
		"launchCompleteCommand": "None",
		"setupCommands":         []map[string]any{{"text": "-enable-pretty-printing", "ignoreFailures": true}},
	}
}

// runDebugUnshare revokes one or all shares for a cluster (key + PID + WG peer + record).
func runDebugUnshare(args []string) {
	fs := newCmdFlagSet("debug unshare")
	id := fs.String("id", "", "cluster id (required)")
	share := fs.String("share", "", "share id to revoke")
	all := fs.Bool("all", false, "revoke every share for the cluster")
	_ = fs.Parse(args)
	if *id == "" || (*share == "" && !*all) {
		fmt.Fprintln(os.Stderr, "usage: pandion debug unshare --id ID [--share SID | --all]")
		os.Exit(2)
	}
	recs, err := loadShareRecords(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug unshare: %v\n", err)
		os.Exit(3)
	}
	n := 0
	for _, rec := range recs {
		if !*all && rec.ShareID != *share {
			continue
		}
		revokeShare(*id, rec)
		n++
	}
	if n == 0 {
		fmt.Printf("no matching share to revoke for %q.\n", *id)
		return
	}
	fmt.Printf("revoked %d share(s) for %q.\n", n, *id)
}

// reapShares revokes and deletes every share for a cluster — called by `down`.
// Best-effort: a destroyed node makes the SSH revoke fail (the grant dies with the
// node); local records are always removed.
func reapShares(id string) {
	recs, err := loadShareRecords(id)
	if err != nil || len(recs) == 0 {
		return
	}
	for _, rec := range recs {
		revokeShare(id, rec)
	}
	os.RemoveAll(sharesDir(id))
}

// revokeShare removes the grant on the node (key line + PID file + WG peer) and
// deletes the local record. Node-unreachable errors are non-fatal.
func revokeShare(id string, rec shareRecord) {
	if pinned, perr := parsePinned(rec.NodeHostPub); perr == nil {
		if signer, serr := loadLoginSigner(id); serr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = envssh.Run(ctx, rec.NodeIP+":22", "root", signer, pinned, revokeScript(rec))
			cancel()
		}
	}
	_ = os.Remove(shareRecordPath(id, rec.ShareID))
	audit.Event("debug.unshare", "id", id, "node", rec.Node, "share", rec.ShareID)
}

// operatorConn loads the operator login signer + the node's pinned host key.
func operatorConn(id string, node nodeManifest) (gossh.Signer, gossh.PublicKey) {
	signer, err := loadLoginSigner(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug share: %v\n", err)
		os.Exit(3)
	}
	pinned, err := parsePinned(node.HostPub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug share: bad host key for %s: %v\n", node.Name, err)
		os.Exit(3)
	}
	return signer, pinned
}

func loadLoginSigner(id string) (gossh.Signer, error) {
	p := filepath.Join(pandionDir(), "keys", id, "login_ed25519")
	pem, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("cannot read login key %s: %w", p, err)
	}
	return gossh.ParsePrivateKey(pem)
}

// --- on-node scripts (idempotent; delivered base64 to avoid quoting hell) ---

// authorizedKeysLine renders the guest's locked-down authorized_keys entry: no
// shell/forwarding (restrict), forced to the gdbserver wrapper bound to this
// share's pinned PID (command with the share id), native auto-expiry (expiry-time),
// and tagged with the share marker for revoke.
func authorizedKeysLine(guestSSHPub string, expiry time.Time, shareID string) string {
	return fmt.Sprintf(`restrict,expiry-time="%s",command="%s %s" %s %s%s`,
		expiry.Format("20060102150405"), debugForced, shareID, guestSSHPub, shareMarker, shareID)
}

func provisionScript(guestSSHPub string, expiry time.Time, shareID, guestWGPub, guestIP string, targetPID int) string {
	ak := "/home/" + debugUser + "/.ssh/authorized_keys"
	var b strings.Builder
	b.WriteString("set -eu\n")
	// 1. non-root debug user + its .ssh. /bin/bash because sshd execs the
	// ForceCommand via the login shell; interactive access is still impossible
	// (authorized_keys forces the wrapper via command= and restrict drops the pty).
	b.WriteString(`id -u ` + debugUser + ` >/dev/null 2>&1 || useradd -m -s /bin/bash ` + debugUser + "\n")
	b.WriteString(`install -d -m700 -o ` + debugUser + ` -g ` + debugUser + ` /home/` + debugUser + "/.ssh\n")
	// 2. gdbserver must be present (shipped in the default toolchain — a locked-down
	// node can't apt-install it, so fail clearly if it's somehow missing).
	b.WriteString(`command -v gdbserver >/dev/null 2>&1 || { echo "gdbserver not installed (provision the node with the default toolchain)" >&2; exit 1; }` + "\n")
	// 3. sudoers: the debug user may run gdbserver as root — reachable ONLY via the
	// forced wrapper (the user has no shell and command= is fixed), so this is safe.
	b.WriteString(`echo '` + debugUser + ` ALL=(root) NOPASSWD: ` + gdbserverBin + `' > ` + debugSudoers + `; chmod 0440 ` + debugSudoers + "\n")
	// 4. ForceCommand wrapper.
	b.WriteString(`echo ` + b64(forcedWrapper) + ` | base64 -d > ` + debugForced + `; chmod 0755 ` + debugForced + "\n")
	// 5. per-share pinned target PID (trusted; the wrapper reads it, not the guest).
	b.WriteString(`install -d -m0755 ` + sharesEtcDir + "\n")
	b.WriteString(`echo ` + strconv.Itoa(targetPID) + ` > ` + sharesEtcDir + `/` + shareID + "\n")
	// 6. authorized_keys line (deduped by marker).
	b.WriteString(`touch ` + ak + `; sed -i '/` + shareMarker + shareID + `/d' ` + ak + "\n")
	b.WriteString(`echo ` + b64(authorizedKeysLine(guestSSHPub, expiry, shareID)) + ` | base64 -d >> ` + ak + "\n")
	b.WriteString(`chown -R ` + debugUser + `:` + debugUser + ` /home/` + debugUser + `/.ssh; chmod 600 ` + ak + "\n")
	// 7. scoped WireGuard peer (AllowedIPs = only this guest).
	b.WriteString(`wg set wg0 peer ` + guestWGPub + ` allowed-ips ` + guestIP + "/32\n")
	return b.String()
}

func revokeScript(rec shareRecord) string {
	ak := "/home/" + debugUser + "/.ssh/authorized_keys"
	return "set -eu\n" +
		`[ -f ` + ak + ` ] && sed -i '/` + shareMarker + rec.ShareID + `/d' ` + ak + " || true\n" +
		`rm -f ` + sharesEtcDir + `/` + rec.ShareID + "\n" +
		`wg set wg0 peer ` + rec.GuestWGPub + " remove || true\n"
}

// forcedWrapper is the ForceCommand: it ignores the client's requested command and
// runs gdbserver (as root) in STDIO mode, attached ONLY to this share's pinned PID
// (from a trusted per-share file), and only if that PID is a non-root/system
// process. No shell, no argument injection, no other process.
const forcedWrapper = `#!/bin/bash
set -eu
sid="${1:-}"
case "$sid" in ''|*[!a-f0-9]*) echo "pandion-debug: bad share id" >&2; exit 1 ;; esac
f="/etc/pandion/shares/$sid"
[ -f "$f" ] || { echo "pandion-debug: unknown or revoked share" >&2; exit 1; }
pid="$(cat "$f")"
case "$pid" in ''|*[!0-9]*) echo "pandion-debug: bad target" >&2; exit 1 ;; esac
uid="$(stat -c %u "/proc/$pid" 2>/dev/null || echo 0)"
if [ "$uid" -lt 1000 ]; then
  echo "pandion-debug: refusing to debug system/root process (uid $uid)" >&2; exit 1
fi
exec sudo -n /usr/bin/gdbserver --once --attach - "$pid"
`

// --- token codec (json -> gzip -> base64url, prefixed) ---

func packToken(b shareBundle) (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return tokenPrefix + base64.URLEncoding.EncodeToString(buf.Bytes()), nil
}

func unpackToken(tok string) (shareBundle, error) {
	var b shareBundle
	if !strings.HasPrefix(tok, tokenPrefix) {
		return b, fmt.Errorf("not a pandion debug token")
	}
	data, err := base64.URLEncoding.DecodeString(strings.TrimPrefix(tok, tokenPrefix))
	if err != nil {
		return b, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return b, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.Version != 1 {
		return b, fmt.Errorf("unsupported token version %d", b.Version)
	}
	return b, nil
}

// --- share records + guest-IP allocation ---

func sharesDir(id string) string {
	return filepath.Join(pandionDir(), "keys", id, "shares")
}
func shareRecordPath(id, shareID string) string {
	return filepath.Join(sharesDir(id), shareID+".json")
}

func saveShareRecord(id string, rec shareRecord) error {
	if err := os.MkdirAll(sharesDir(id), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(shareRecordPath(id, rec.ShareID), b, 0o600)
}

func loadShareRecords(id string) ([]shareRecord, error) {
	entries, err := os.ReadDir(sharesDir(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []shareRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sharesDir(id), e.Name()))
		if err != nil {
			continue
		}
		var rec shareRecord
		if json.Unmarshal(b, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// allocGuestIP picks a free guest overlay IP (10.99.0.252 down to .240), avoiding
// IPs already assigned to live shares of this cluster.
func allocGuestIP(id string) (string, error) {
	recs, _ := loadShareRecords(id)
	used := map[string]bool{}
	for _, r := range recs {
		used[r.GuestIP] = true
	}
	for i := guestIPHi; i >= guestIPLo; i-- {
		ip := fmt.Sprintf("10.99.0.%d", i)
		if !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no free guest overlay IP (max %d concurrent shares) — run `unshare` first", guestIPHi-guestIPLo+1)
}

// randID returns a short random hex id for a share.
func randID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
