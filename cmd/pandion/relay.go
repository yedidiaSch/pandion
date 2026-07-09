// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/relay"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	"github.com/yedidiaSch/pandion/internal/sshkeys"
)

// runRelayDispatch routes `pandion relay <subcommand>`.
func runRelayDispatch(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pandion relay up|share|unshare|status ...")
		os.Exit(2)
	}
	switch args[0] {
	case "up":
		runRelayUp(args[1:])
	case "share":
		runRelayShare(args[1:])
	case "unshare":
		runRelayUnshare(args[1:])
	case "status":
		runRelayStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "relay: unknown subcommand %q (up|share|unshare|status)\n", args[0])
		os.Exit(2)
	}
}

const relayShareMarker = "pandion-rshare-" // + shareID, the authorized_keys comment

// relayShareRecord is the local bookkeeping unshare/down need to revoke a grant.
type relayShareRecord struct {
	ShareID      string `json:"share_id"`
	Node         string `json:"node"`           // target node
	NodeIP       string `json:"node_ip"`        // target public IP (to remove the user's key)
	NodeHostPub  string `json:"node_host_pub"`  // target pinned host key
	User         string `json:"user"`           // scoped login user on the target
	Token        string `json:"token"`          // to locate the relay spool file
	RelayIP      string `json:"relay_ip"`       // relay node public IP (to remove the spool file)
	RelayHostPub string `json:"relay_host_pub"` // relay node pinned host key
	RelayPort    int    `json:"relay_port"`
	URL          string `json:"url"` // the full clickable link
	Expiry       string `json:"expiry"`
}

func relaySharesDir(id string) string {
	return filepath.Join(envHome(), ".pandion", "keys", id, "relay-shares")
}

// runRelayShare mints a scoped, expiring, browser-SSH grant to a node and prints a
// clickable URL. It provisions a scoped, non-root login user on the target (with an
// expiring key), writes the session to the relay node's spool, and records the grant
// locally for revocation.
//
//	pandion relay share --id ID --node TARGET [--expires 4h] [--user pandion-lab]
func runRelayShare(args []string) {
	fs := flag.NewFlagSet("relay share", flag.ExitOnError)
	id := fs.String("id", "demo", "cluster id")
	node := fs.String("node", "", "target node to share (required)")
	expires := fs.Duration("expires", 4*time.Hour, "how long the link is valid")
	user := fs.String("user", "pandion-lab", "scoped non-root login user on the target")
	_ = fs.Parse(args)
	initAudit()

	if *node == "" {
		fmt.Fprintln(os.Stderr, "relay share: --node TARGET is required")
		os.Exit(2)
	}
	rec, err := loadRelayRecord(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay share: no relay for %q — run `pandion relay up --id %s` first\n", *id, *id)
		os.Exit(1)
	}
	man, err := loadManifest(*id)
	must(err)
	var target *nodeManifest
	for i := range man.Nodes {
		if man.Nodes[i].Name == *node {
			target = &man.Nodes[i]
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "relay share: no node %q in cluster %q\n", *node, *id)
		os.Exit(1)
	}
	signer, err := loadLoginSigner(*id)
	must(err)

	shareID := randID()
	scoped, err := sshkeys.Generate("pandion-relay-" + shareID)
	must(err)
	expiry := time.Now().Add(*expires).UTC()
	token, err := relay.NewToken()
	must(err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1) provision the scoped, expiring login user on the TARGET node.
	targetPinned, err := parsePinned(target.HostPub)
	must(err)
	if out, err := envssh.Run(ctx, target.IP+":22", "root", signer, targetPinned,
		relayShareProvisionScript(*user, scoped.PublicAuthorized, expiry, shareID)); err != nil {
		fmt.Fprintf(os.Stderr, "relay share: provision on target failed: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// 2) write the session to the relay node's spool (reachable only by the relay).
	sess := relay.Session{
		ID: shareID, Token: token, ClusterID: *id, Node: target.Name,
		Target: target.OverlayIP, HostPub: target.HostPub, User: *user,
		SSHKeyPEM: scoped.PrivatePEM, Expiry: expiry,
	}
	data, err := json.Marshal(sess)
	must(err)
	// the relay node's pinned host key (to SSH the session into its spool).
	var relayNode *nodeManifest
	for i := range man.Nodes {
		if man.Nodes[i].Name == rec.Node {
			relayNode = &man.Nodes[i]
		}
	}
	if relayNode == nil {
		fmt.Fprintf(os.Stderr, "relay share: relay node %q not in manifest\n", rec.Node)
		os.Exit(1)
	}
	relayPinned, err := parsePinned(relayNode.HostPub)
	must(err)
	spool := "/var/lib/pandion-relay/sessions/" + relay.SpoolFilename(token)
	writeCmd := "cat > " + spool + " && chown pandion-relay:pandion-relay " + spool + " && chmod 600 " + spool
	if out, err := envssh.RunWithInput(ctx, rec.IP+":22", "root", signer, relayPinned, writeCmd, bytes.NewReader(data)); err != nil {
		fmt.Fprintf(os.Stderr, "relay share: writing session to relay failed: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// 3) record locally for revoke.
	must(os.MkdirAll(relaySharesDir(*id), 0o700))
	url := rec.baseURL() + "/s/" + token
	sr := relayShareRecord{
		ShareID: shareID, Node: target.Name, NodeIP: target.IP, NodeHostPub: target.HostPub,
		User: *user, Token: token, RelayIP: rec.IP, RelayHostPub: relayNode.HostPub,
		RelayPort: rec.Port, URL: url, Expiry: expiry.Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(sr, "", "  ")
	_ = os.WriteFile(filepath.Join(relaySharesDir(*id), shareID+".json"), b, 0o600)
	audit.Event("relay.share", "id", *id, "node", target.Name, "share", shareID, "user", *user, "expiry", expiry.Format(time.RFC3339))

	fmt.Printf("shared %q/%s as %s (expires %s):\n\n", *id, target.Name, *user, expiry.Format("2006-01-02 15:04 MST"))
	fmt.Println("  " + url)
	if rec.Domain != "" {
		fmt.Println("\n# send that link — browser-trusted TLS (Let's Encrypt).")
	} else {
		fmt.Printf("\n# send that link. TLS is self-signed (fingerprint %s) — the browser will warn.\n", rec.Fingerprint)
	}
	fmt.Printf("# revoke:  pandion relay unshare --id %s --share %s   (or --all)\n", *id, shareID)
}

// relayShareProvisionScript creates the scoped, non-root login user (idempotent) and
// appends an EXPIRING authorized_keys entry for the scoped key — a real shell (so the
// browser terminal works), but time-boxed and tagged for revoke. No forced command.
func relayShareProvisionScript(user, scopedPub string, expiry time.Time, shareID string) string {
	ak := "/home/" + user + "/.ssh/authorized_keys"
	line := fmt.Sprintf(`expiry-time="%s" %s %s%s`, expiry.Format("20060102150405"), scopedPub, relayShareMarker, shareID)
	return "set -e\n" +
		"id -u " + user + " >/dev/null 2>&1 || useradd -m -s /bin/bash " + user + "\n" +
		"install -d -m700 -o " + user + " -g " + user + " /home/" + user + "/.ssh\n" +
		"touch " + ak + "\n" +
		"sed -i '/" + relayShareMarker + shareID + "/d' " + ak + "\n" +
		"echo " + b64(line) + " | base64 -d >> " + ak + "\n" +
		"chown " + user + ":" + user + " " + ak + " && chmod 600 " + ak + "\n"
}

// runRelayUnshare revokes one or all relay grants: remove the target's authorized_keys
// entry and the relay's spool session, then delete the local record.
func runRelayUnshare(args []string) {
	fs := flag.NewFlagSet("relay unshare", flag.ExitOnError)
	id := fs.String("id", "demo", "cluster id")
	share := fs.String("share", "", "share id to revoke")
	all := fs.Bool("all", false, "revoke every relay grant for this cluster")
	_ = fs.Parse(args)
	initAudit()

	recs := loadRelayShareRecords(*id)
	if len(recs) == 0 {
		fmt.Println("no relay grants to revoke.")
		return
	}
	signer, err := loadLoginSigner(*id)
	must(err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	n := 0
	for _, r := range recs {
		if !*all && r.ShareID != *share {
			continue
		}
		// remove the scoped key line on the target
		if tp, perr := parsePinned(r.NodeHostPub); perr == nil {
			ak := "/home/" + r.User + "/.ssh/authorized_keys"
			_, _ = envssh.Run(ctx, r.NodeIP+":22", "root", signer, tp,
				"[ -f "+ak+" ] && sed -i '/"+relayShareMarker+r.ShareID+"/d' "+ak+" || true")
		}
		// remove the relay spool session
		if rp, perr := parsePinned(r.RelayHostPub); perr == nil {
			_, _ = envssh.Run(ctx, r.RelayIP+":22", "root", signer, rp,
				"rm -f /var/lib/pandion-relay/sessions/"+relay.SpoolFilename(r.Token))
		}
		_ = os.Remove(filepath.Join(relaySharesDir(*id), r.ShareID+".json"))
		audit.Event("relay.unshare", "id", *id, "share", r.ShareID)
		fmt.Printf("revoked %s (%s)\n", r.ShareID, r.Node)
		n++
	}
	if n == 0 {
		fmt.Printf("no matching relay grant (share %q)\n", *share)
	}
}

// runRelayStatus lists live relay grants for a cluster.
func runRelayStatus(args []string) {
	fs := flag.NewFlagSet("relay status", flag.ExitOnError)
	id := fs.String("id", "demo", "cluster id")
	_ = fs.Parse(args)
	recs := loadRelayShareRecords(*id)
	if rec, err := loadRelayRecord(*id); err == nil {
		fmt.Printf("relay: %s/  (node %q)\n", rec.baseURL(), rec.Node)
	} else {
		fmt.Println("relay: not deployed (pandion relay up)")
	}
	if len(recs) == 0 {
		fmt.Println("no active grants.")
		return
	}
	for _, r := range recs {
		fmt.Printf("  %s  node=%s  user=%s  expires=%s\n  %s\n",
			r.ShareID, r.Node, r.User, r.Expiry, r.URL)
	}
}

func loadRelayShareRecords(id string) []relayShareRecord {
	entries, err := os.ReadDir(relaySharesDir(id))
	if err != nil {
		return nil
	}
	var out []relayShareRecord
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(relaySharesDir(id), e.Name()))
		if err != nil {
			continue
		}
		var r relayShareRecord
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// relayRecord is persisted locally so `relay share`/`down` know where the relay is.
type relayRecord struct {
	Node        string `json:"node"`
	IP          string `json:"ip"`
	Domain      string `json:"domain,omitempty"` // Let's Encrypt DNS name, if any
	Port        int    `json:"port"`
	Fingerprint string `json:"fingerprint"` // self-signed only; "(Let's Encrypt)" for --domain
}

// baseURL is the relay's public origin: the domain (Let's Encrypt) if set, else the
// node IP, dropping :443 for a clean URL.
func (r *relayRecord) baseURL() string {
	host := r.IP
	if r.Domain != "" {
		host = r.Domain
	}
	if r.Port == 443 {
		return "https://" + host
	}
	return fmt.Sprintf("https://%s:%d", host, r.Port)
}

func relayRecordPath(id string) string {
	return filepath.Join(envHome(), ".pandion", "keys", id, "relay.json")
}

// runRelayUp deploys the browser-SSH relay onto a designated cluster node: uploads
// the pandion-relay binary (built for the node's arch), installs a non-root systemd
// service, opens the one firewall port, starts it, and prints the base URL.
//
//	pandion relay up --id ID [--node NAME] [--port 8443] [--relay-binary PATH]
func runRelayUp(args []string) {
	fs := flag.NewFlagSet("relay up", flag.ExitOnError)
	id := fs.String("id", "demo", "cluster id")
	node := fs.String("node", "", "node to host the relay (default: first node)")
	port := fs.Int("port", 8443, "public TLS port for the relay")
	domain := fs.String("domain", "", "public DNS name for browser-trusted Let's Encrypt TLS (implies :443)")
	relayBin := fs.String("relay-binary", "", "prebuilt linux pandion-relay binary (default: build from source)")
	_ = fs.Parse(args)
	initAudit()

	// Let's Encrypt (TLS-ALPN-01) is answered on :443; force it unless the operator
	// deliberately chose another port.
	if *domain != "" && *port == 8443 {
		*port = 443
	}
	if *domain != "" && *port != 443 {
		fmt.Fprintf(os.Stderr, "warning: Let's Encrypt (TLS-ALPN-01) needs :443; issuance will fail on :%d\n", *port)
	}

	man, err := loadManifest(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay up: %v\n", err)
		os.Exit(1)
	}
	if len(man.Nodes) == 0 {
		fmt.Fprintln(os.Stderr, "relay up: cluster has no nodes")
		os.Exit(1)
	}
	target := man.Nodes[0]
	if *node != "" {
		found := false
		for _, n := range man.Nodes {
			if n.Name == *node {
				target, found = n, true
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "relay up: no node %q in cluster %q\n", *node, *id)
			os.Exit(1)
		}
	}
	signer, err := loadLoginSigner(*id)
	must(err)
	pinned, err := parsePinned(target.HostPub)
	must(err)
	addr := target.IP + ":22"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1) obtain a linux/<node-arch> pandion-relay binary.
	arch := nodeArch(ctx, addr, signer, pinned)
	if arch == "" {
		arch = "amd64"
	}
	binPath := *relayBin
	if binPath == "" {
		fmt.Printf("building pandion-relay for linux/%s...\n", arch)
		binPath, err = buildRelayBinary(arch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "relay up: could not build pandion-relay (%v)\n", err)
			fmt.Fprintln(os.Stderr, "  provide a prebuilt one with --relay-binary PATH")
			os.Exit(1)
		}
		defer os.Remove(binPath)
	}

	// 2) upload the binary.
	fmt.Printf("[%s] uploading relay binary...\n", target.Name)
	binData, err := os.ReadFile(binPath)
	must(err)
	if out, err := envssh.RunWithInput(ctx, addr, "root", signer, pinned,
		"cat > /usr/local/bin/pandion-relay && chmod 0755 /usr/local/bin/pandion-relay", bytes.NewReader(binData)); err != nil {
		fmt.Fprintf(os.Stderr, "relay up: upload failed: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// with --domain, check the A record points at this node before we bother.
	if *domain != "" {
		if ips, lerr := net.LookupHost(*domain); lerr != nil || !contains(ips, target.IP) {
			fmt.Fprintf(os.Stderr, "warning: %s does not resolve to %s yet — create an A record:\n  %s  A  %s\n",
				*domain, target.IP, *domain, target.IP)
			fmt.Fprintln(os.Stderr, "  (Let's Encrypt will keep retrying once DNS is correct.)")
		}
	}

	// 3) provision the non-root service + state dir + systemd unit.
	fmt.Printf("[%s] installing pandion-relay.service...\n", target.Name)
	if out, err := envssh.Run(ctx, addr, "root", signer, pinned, relayProvisionScript(*port, target.IP, *domain)); err != nil {
		fmt.Fprintf(os.Stderr, "relay up: provision failed: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// 4) open the one public inbound port in the host firewall (targeted rule, no
	//    flush). With --domain, ALSO allow outbound :443 so autocert can reach the
	//    Let's Encrypt ACME API (the node is otherwise default-deny egress).
	fwCmd := fmt.Sprintf("nft add rule inet pandion input tcp dport %d accept 2>/dev/null || true", *port)
	if *domain != "" {
		fwCmd += "; nft add rule inet pandion output tcp dport 443 accept 2>/dev/null || true"
	}
	if out, err := envssh.Run(ctx, addr, "root", signer, pinned, fwCmd); err != nil {
		fmt.Fprintf(os.Stderr, "relay up: firewall rule warning: %v\n%s\n", err, out)
	}

	// 5) start it.
	if out, err := envssh.Run(ctx, addr, "root", signer, pinned, "systemctl enable --now pandion-relay"); err != nil {
		fmt.Fprintf(os.Stderr, "relay up: start failed: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// 6) TLS identity: Let's Encrypt certs are browser-trusted (no fingerprint to
	//    show); for self-signed, read back the fingerprint the relay generated.
	fp := "(Let's Encrypt)"
	if *domain == "" {
		fp = "(pending)"
		for attempt := 0; attempt < 5; attempt++ {
			time.Sleep(time.Second)
			out, rerr := envssh.Run(ctx, addr, "root", signer, pinned, "cat /var/lib/pandion-relay/relay.crt 2>/dev/null")
			if rerr == nil {
				if f, ferr := relay.PEMFingerprint([]byte(out)); ferr == nil {
					fp = f
					break
				}
			}
		}
	}

	rec := relayRecord{Node: target.Name, IP: target.IP, Domain: *domain, Port: *port, Fingerprint: fp}
	if err := writeRelayRecord(*id, rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist relay record: %v\n", err)
	}
	audit.Event("relay.up", "id", *id, "node", target.Name, "ip", target.IP, "port", *port, "domain", *domain)

	fmt.Printf("\nrelay up on %s (node %q): %s/\n", *id, target.Name, rec.baseURL())
	if *domain != "" {
		fmt.Printf("  TLS: Let's Encrypt for %s (browser-trusted — first request may take ~30s while the cert issues)\n", *domain)
	} else {
		fmt.Printf("  TLS SHA-256: %s\n", fp)
		fmt.Printf("  (self-signed — participants see a browser warning; use --domain <name> for a trusted cert)\n")
	}
	fmt.Printf("  next:  pandion relay share --id %s --node TARGET   (mint a clickable link)\n", *id)
}

// buildRelayBinary cross-compiles cmd/pandion-relay for linux/<arch> to a temp file.
// Works from a source checkout with the Go toolchain (dev/CI/e2e); released binaries
// will instead download the matching signed artifact (Phase 2).
func buildRelayBinary(arch string) (string, error) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("pandion-relay-linux-%s-%d", arch, time.Now().UnixNano()))
	cmd := exec.Command("go", "build", "-o", out, "./cmd/pandion-relay")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, b)
	}
	return out, nil
}

// relayProvisionScript creates the non-root relay user, its state dir, and a systemd
// unit that serves TLS on port and reaches targets over the overlay. With domain set,
// the relay uses Let's Encrypt (needs :443 and the bind capability); otherwise it
// self-signs with hostIP in the cert SANs.
func relayProvisionScript(port int, hostIP, domain string) string {
	execStart := fmt.Sprintf("/usr/local/bin/pandion-relay --addr :%d --spool /var/lib/pandion-relay/sessions --state /var/lib/pandion-relay", port)
	caps := ""
	if domain != "" {
		execStart += " --domain " + domain
		// binding :443 as a non-root user needs CAP_NET_BIND_SERVICE.
		caps = "AmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE\n"
	} else {
		execStart += " --hosts " + hostIP
	}
	unit := fmt.Sprintf(`[Unit]
Description=Pandion browser-SSH relay
After=network-online.target wg-quick@wg0.service
Wants=network-online.target

[Service]
User=pandion-relay
ExecStart=%s
Restart=on-failure
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/pandion-relay
ProtectHome=true
%s
[Install]
WantedBy=multi-user.target
`, execStart, caps)

	return "set -e\n" +
		"id -u pandion-relay >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin -d /var/lib/pandion-relay pandion-relay\n" +
		"mkdir -p /var/lib/pandion-relay/sessions\n" +
		"chown -R pandion-relay:pandion-relay /var/lib/pandion-relay\n" +
		"chmod 700 /var/lib/pandion-relay /var/lib/pandion-relay/sessions\n" +
		"cat > /etc/systemd/system/pandion-relay.service <<'PANDION_RELAY_UNIT'\n" + unit + "PANDION_RELAY_UNIT\n" +
		"systemctl daemon-reload\n"
}

func writeRelayRecord(id string, rec relayRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(relayRecordPath(id), b, 0o600)
}

// loadRelayRecord reads the persisted relay record (used by share/down).
func loadRelayRecord(id string) (*relayRecord, error) {
	b, err := os.ReadFile(relayRecordPath(id))
	if err != nil {
		return nil, err
	}
	var r relayRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
