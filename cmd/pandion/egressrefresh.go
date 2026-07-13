// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/yedidiaSch/pandion/internal/harden"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
)

// egressRefreshEvery is how often the on-node timer re-resolves egress-allow
// hostnames (internal/harden.EgressRefreshInstall).
const egressRefreshEvery = 2 * time.Minute

// installEgressRefresh installs the on-node re-resolution timer for any HOSTNAME
// egress-allow entries, so their nftables rules track rotating CDN IPs rather
// than freezing at the provision-time snapshot. Best-effort: a failure warns but
// does not fail the up (the provision-time IPs still apply). A no-op when there
// are no hostnames.
func installEgressRefresh(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, hostnames []string) {
	script := harden.EgressRefreshInstall(hostnames, egressRefreshEvery)
	if script == "" {
		return
	}
	cmd := "echo " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sh"
	if out, err := envssh.Run(ctx, addr, "root", signer, pinned, cmd); err != nil {
		fmt.Fprintf(os.Stderr, "warning: egress-allow refresh timer not installed: %v\n%s\n", err, out)
		return
	}
	fmt.Printf("egress-allow: on-node re-resolution installed (%d hostname(s), every %s)\n",
		len(hostnames), egressRefreshEvery)
}
