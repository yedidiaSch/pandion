// SPDX-License-Identifier: AGPL-3.0-or-later

package harden

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// validHost guards against injection: egress-allow hostnames are written into an
// on-node file, so restrict to DNS-legal characters (no shell metacharacters).
var validHost = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// egressRefreshScript is the on-node re-resolver. Quoted-heredoc'd verbatim, so
// its $vars expand at RUN time, not install time. Add-only: it never removes an
// address, so a live IP is never briefly denied; a rotated-away IP just goes
// unused. Tolerant of the set not existing yet (|| true) so it is safe to run
// before the firewall ruleset lands.
const egressRefreshScript = `#!/bin/sh
# Pandion egress-allow re-resolution: keep hostname rules correct as CDN IPs
# rotate by ADDING each host's current IPv4s to the nftables egress set.
SET="inet pandion egress_ok"
[ -r /etc/pandion/egress-hosts ] || exit 0
nft list set $SET >/dev/null 2>&1 || exit 0   # set not created yet — nothing to do
while IFS= read -r h; do
  [ -z "$h" ] && continue
  for ip in $(getent ahostsv4 "$h" 2>/dev/null | awk '{print $1}' | sort -u); do
    nft add element $SET "{ $ip }" 2>/dev/null || true
  done
done < /etc/pandion/egress-hosts
`

// EgressRefreshInstall renders a shell script that installs (and starts) an
// on-node systemd timer which periodically re-resolves the given egress-allow
// HOSTNAMES and adds their current IPv4 addresses to the nftables egress set.
// This keeps hostname-based egress rules correct as CDN IPs rotate — the
// provision-time resolution is only a point-in-time snapshot.
//
// Returns "" if there are no valid hostnames (nothing to install). Callers pipe
// the result to `sh` on the node. `every` is clamped to a 30s floor.
func EgressRefreshInstall(hostnames []string, every time.Duration) string {
	var hosts []string
	seen := map[string]bool{}
	for _, h := range hostnames {
		h = strings.TrimSpace(h)
		if h != "" && !seen[h] && validHost.MatchString(h) {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return ""
	}
	secs := max(int(every.Seconds()), 30)

	var b strings.Builder
	b.WriteString("set -e\nmkdir -p /etc/pandion\n")
	b.WriteString("cat > /etc/pandion/egress-hosts <<'PANDION_EOF'\n")
	b.WriteString(strings.Join(hosts, "\n") + "\n")
	b.WriteString("PANDION_EOF\n")
	b.WriteString("cat > /usr/local/sbin/pandion-egress-refresh <<'PANDION_EOF'\n")
	b.WriteString(egressRefreshScript)
	b.WriteString("PANDION_EOF\nchmod 0755 /usr/local/sbin/pandion-egress-refresh\n")
	b.WriteString("cat > /etc/systemd/system/pandion-egress-refresh.service <<'PANDION_EOF'\n")
	b.WriteString("[Unit]\nDescription=Pandion egress-allow re-resolution\n\n[Service]\nType=oneshot\nExecStart=/usr/local/sbin/pandion-egress-refresh\nPANDION_EOF\n")
	fmt.Fprintf(&b, "cat > /etc/systemd/system/pandion-egress-refresh.timer <<'PANDION_EOF'\n"+
		"[Unit]\nDescription=Re-resolve Pandion egress allowlist every %ds\n\n"+
		"[Timer]\nOnBootSec=60\nOnUnitActiveSec=%d\nAccuracySec=15\n\n"+
		"[Install]\nWantedBy=timers.target\nPANDION_EOF\n", secs, secs)
	b.WriteString("systemctl daemon-reload\n")
	b.WriteString("systemctl enable --now pandion-egress-refresh.timer\n")
	b.WriteString("/usr/local/sbin/pandion-egress-refresh || true\n") // seed once now
	return b.String()
}
