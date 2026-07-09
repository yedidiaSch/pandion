// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/relay"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
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
	default:
		fmt.Fprintf(os.Stderr, "relay: unknown subcommand %q (share/unshare/status land in the next slice)\n", args[0])
		os.Exit(2)
	}
}

// relayRecord is persisted locally so `relay share`/`down` know where the relay is.
type relayRecord struct {
	Node        string `json:"node"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Fingerprint string `json:"fingerprint"`
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
	relayBin := fs.String("relay-binary", "", "prebuilt linux pandion-relay binary (default: build from source)")
	_ = fs.Parse(args)
	initAudit()

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

	// 3) provision the non-root service + state dir + systemd unit.
	fmt.Printf("[%s] installing pandion-relay.service...\n", target.Name)
	if out, err := envssh.Run(ctx, addr, "root", signer, pinned, relayProvisionScript(*port, target.IP)); err != nil {
		fmt.Fprintf(os.Stderr, "relay up: provision failed: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// 4) open the one public port in the host firewall (targeted rule, no flush).
	if out, err := envssh.Run(ctx, addr, "root", signer, pinned,
		fmt.Sprintf("nft add rule inet pandion input tcp dport %d accept 2>/dev/null || true", *port)); err != nil {
		fmt.Fprintf(os.Stderr, "relay up: firewall rule warning: %v\n%s\n", err, out)
	}

	// 5) start it.
	if out, err := envssh.Run(ctx, addr, "root", signer, pinned, "systemctl enable --now pandion-relay"); err != nil {
		fmt.Fprintf(os.Stderr, "relay up: start failed: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// 6) read back the cert fingerprint the relay just generated (a couple of tries;
	//    the relay writes it within ~1s of first start).
	fp := "(pending)"
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

	rec := relayRecord{Node: target.Name, IP: target.IP, Port: *port, Fingerprint: fp}
	if err := writeRelayRecord(*id, rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist relay record: %v\n", err)
	}
	audit.Event("relay.up", "id", *id, "node", target.Name, "ip", target.IP, "port", *port)

	fmt.Printf("\nrelay up on %s (node %q): https://%s:%d/\n", *id, target.Name, target.IP, *port)
	fmt.Printf("  TLS SHA-256: %s\n", fp)
	fmt.Printf("  (self-signed — participants will see a browser warning; --domain Let's Encrypt is coming)\n")
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
// unit that serves TLS on port and reaches targets over the overlay. hostIP is put
// in the self-signed cert's SANs.
func relayProvisionScript(port int, hostIP string) string {
	unit := fmt.Sprintf(`[Unit]
Description=Pandion browser-SSH relay
After=network-online.target wg-quick@wg0.service
Wants=network-online.target

[Service]
User=pandion-relay
ExecStart=/usr/local/bin/pandion-relay --addr :%d --spool /var/lib/pandion-relay/sessions --state /var/lib/pandion-relay --hosts %s
Restart=on-failure
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/pandion-relay
ProtectHome=true

[Install]
WantedBy=multi-user.target
`, port, hostIP)

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
