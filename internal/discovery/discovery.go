// SPDX-License-Identifier: AGPL-3.0-or-later

// Package discovery builds the service-discovery environment injected into every
// cluster node (C5/H1). Each node learns its siblings' OVERLAY IPs via env vars
// (e.g. $PANDION_BROKER_IP), so run commands need no hardcoded IPs. IPC flows over
// the encrypted WireGuard overlay, which the host firewall already trusts.
//
// Per finding H1/F4, discovery is delivered as a shell-sourced file
// (/etc/profile.d/pandion.sh); a login shell (`bash -lc`) picks it up, and it
// survives for later `attach`/debug sessions.
package discovery

import (
	"fmt"
	"sort"
	"strings"
)

// Path is where the sourced discovery file is written on each node.
const Path = "/etc/profile.d/pandion.sh"

// EnvVarName maps a node name to its discovery variable, e.g. "worker-a" ->
// "PANDION_WORKER_A_IP".
func EnvVarName(node string) string {
	var b strings.Builder
	b.WriteString("PANDION_")
	for _, r := range strings.ToUpper(node) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	b.WriteString("_IP")
	return b.String()
}

// Script renders the /etc/profile.d/pandion.sh contents from node -> overlay IP.
// selfName (optional) also exports PANDION_SELF_IP / PANDION_SELF_NAME.
func Script(overlayIPByNode map[string]string, selfName string) string {
	names := make([]string, 0, len(overlayIPByNode))
	for n := range overlayIPByNode {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Pandion service discovery (overlay IPs). Generated — do not edit.\n")
	for _, n := range names {
		fmt.Fprintf(&b, "export %s=%s\n", EnvVarName(n), overlayIPByNode[n])
	}
	if selfName != "" {
		fmt.Fprintf(&b, "export PANDION_SELF_NAME=%s\n", selfName)
		if ip, ok := overlayIPByNode[selfName]; ok {
			fmt.Fprintf(&b, "export PANDION_SELF_IP=%s\n", ip)
		}
		// Distributed rendezvous (M5-R6): a stable rank from the sorted node order,
		// the world size, and rank-0's overlay IP as the master address — so a
		// distributed entrypoint can form a group over the mesh with no hardcoded
		// IPs, e.g. `MASTER_ADDR=$PANDION_MASTER_ADDR torchrun --nnodes
		// $PANDION_WORLD_SIZE --node-rank $PANDION_RANK …`. Harmless for CPU clusters.
		if len(names) > 0 {
			rank := 0
			for i, n := range names {
				if n == selfName {
					rank = i
				}
			}
			fmt.Fprintf(&b, "export PANDION_WORLD_SIZE=%d\n", len(names))
			fmt.Fprintf(&b, "export PANDION_RANK=%d\n", rank)
			fmt.Fprintf(&b, "export PANDION_MASTER_ADDR=%s\n", overlayIPByNode[names[0]])
			fmt.Fprintf(&b, "export PANDION_MASTER_PORT=29500\n")
		}
	}
	return b.String()
}
