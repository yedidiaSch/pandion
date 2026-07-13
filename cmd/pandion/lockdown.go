// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/firewall"
	"github.com/yedidiaSch/pandion/internal/overlay"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// runLockdown flips a cluster to FULL public-ingress deny-all: SSH becomes
// reachable ONLY over the WireGuard overlay (M3.5). It is LOCKOUT-SAFE by design:
// it first verifies it can SSH every node over the OVERLAY, and refuses to change
// anything if it cannot — so you can never cut off your own access. The firewall
// change is applied over the overlay connection itself.
//
// Prereq: you must have joined the overlay first, e.g.
//
//	sudo wg-quick up ~/.pandion/keys/<id>/wg-<id>.conf
func runLockdown(args []string) {
	fs := newCmdFlagSet("lockdown")
	id := fs.String("id", "", "cluster id (required)")
	auditMode := fs.Bool("audit", false, "DRY RUN: apply the deny-all in audit mode — log what would be dropped but enforce nothing (inspect with `journalctl -k | grep pandion-audit`)")
	_ = fs.Parse(args)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "lockdown: --id is required")
		os.Exit(2)
	}
	initAudit()
	// serialize against a concurrent up/down/reap on this id (P0.5).
	lk := lockClusterOrExit(*id)
	defer lk.Unlock()

	man, err := loadManifest(*id)
	if err != nil {
		bailIfTornDown(err)
		fmt.Fprintf(os.Stderr, "lockdown: cannot load cluster manifest for %q: %v\n", *id, err)
		fmt.Fprintln(os.Stderr, "  (was the cluster created with `up -f`? manifest lives in ~/.pandion/keys/<id>/)")
		os.Exit(3)
	}

	// login signer for SSH
	loginPath := filepath.Join(pandionDir(), "keys", *id, "login_ed25519")
	pem, err := os.ReadFile(loginPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lockdown: cannot read login key %s: %v\n", loginPath, err)
		os.Exit(3)
	}
	signer, err := gossh.ParsePrivateKey(pem)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lockdown: bad login key: %v\n", err)
		os.Exit(3)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// STEP 1 (safety gate): confirm we can reach EVERY node over the overlay.
	fmt.Printf("verifying overlay reachability to %d node(s) before locking down...\n", len(man.Nodes))
	for _, n := range man.Nodes {
		pinned, perr := parsePinned(n.HostPub)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "lockdown: bad host key for %s: %v\n", n.Name, perr)
			os.Exit(3)
		}
		if _, err := envssh.Run(ctx, n.OverlayIP+":22", "root", signer, pinned, "true"); err != nil {
			fmt.Fprintf(os.Stderr, "REFUSING to lock down: cannot SSH %s over the overlay (%s): %v\n",
				n.Name, n.OverlayIP, err)
			fmt.Fprintf(os.Stderr, "  join the overlay first:  sudo wg-quick up %s\n",
				filepath.Join(pandionDir(), "keys", *id, "wg-"+*id+".conf"))
			fmt.Fprintln(os.Stderr, "  (locking down without overlay access would cut you off — aborted, nothing changed)")
			os.Exit(6)
		}
		fmt.Printf("  %s (%s): reachable over overlay\n", n.Name, n.OverlayIP)
	}

	// STEP 2: apply deny-all-public firewall over the OVERLAY connection, so the
	// change can't sever us (established + iif wg0 keep the session alive).
	if *auditMode {
		fmt.Println("all nodes reachable over overlay — applying deny-all in AUDIT mode (log only, nothing enforced)...")
	} else {
		fmt.Println("all nodes reachable over overlay — applying public deny-all...")
	}
	rules := firewall.NFTables(firewall.Spec{
		AllowDNS: true, NoPublicSSH: true,
		WGPort: overlay.DefaultPort, AllowOverlayInput: true,
		BlockMetadata: true, // S-F: no workload may read cloud metadata
		AuditOnly:     *auditMode,
	})
	cmd := "echo " + b64(rules) + " | base64 -d | nft -f -"
	for _, n := range man.Nodes {
		pinned, _ := parsePinned(n.HostPub)
		if _, err := envssh.Run(ctx, n.OverlayIP+":22", "root", signer, pinned, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "lockdown: firewall apply failed on %s: %v\n", n.Name, err)
			os.Exit(6)
		}
		if *auditMode {
			fmt.Printf("  %s: audit ruleset applied (public SSH still open — logging what deny-all would drop)\n", n.Name)
		} else {
			fmt.Printf("  %s: public SSH removed (overlay-only)\n", n.Name)
		}
	}

	audit.Event("lockdown", "id", *id, "nodes", len(man.Nodes), "audit", *auditMode)
	fmt.Println("----------------------------------------------------------------")
	if *auditMode {
		fmt.Printf("cluster %q lockdown AUDIT applied: nothing enforced; inspect would-be-drops with\n", *id)
		fmt.Printf("  pandion ssh --id %s -- 'journalctl -k | grep pandion-audit'\n", *id)
	} else {
		fmt.Printf("cluster %q locked down: public ingress = deny-all; SSH only over the overlay.\n", *id)
		fmt.Println("a public scan now sees only the WireGuard port. Reach nodes at their 10.99.0.x IPs.")
	}
}

// parsePinned turns an authorized-keys line into a pinned host key.
func parsePinned(authorized string) (gossh.PublicKey, error) {
	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(authorized))
	return pk, err
}
