// Package discovery builds the service-discovery environment injected into every
// cluster node (C5/H1). Each node learns its siblings' OVERLAY IPs via env vars
// (e.g. $ENVCORE_BROKER_IP), so run commands need no hardcoded IPs. IPC flows over
// the encrypted WireGuard overlay, which the host firewall already trusts.
//
// Per finding H1/F4, discovery is delivered as a shell-sourced file
// (/etc/profile.d/envcore.sh); a login shell (`bash -lc`) picks it up, and it
// survives for later `attach`/debug sessions.
package discovery

import (
	"fmt"
	"sort"
	"strings"
)

// Path is where the sourced discovery file is written on each node.
const Path = "/etc/profile.d/envcore.sh"

// EnvVarName maps a node name to its discovery variable, e.g. "worker-a" ->
// "ENVCORE_WORKER_A_IP".
func EnvVarName(node string) string {
	var b strings.Builder
	b.WriteString("ENVCORE_")
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

// Script renders the /etc/profile.d/envcore.sh contents from node -> overlay IP.
// selfName (optional) also exports ENVCORE_SELF_IP / ENVCORE_SELF_NAME.
func Script(overlayIPByNode map[string]string, selfName string) string {
	names := make([]string, 0, len(overlayIPByNode))
	for n := range overlayIPByNode {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# EnvCore service discovery (overlay IPs). Generated — do not edit.\n")
	for _, n := range names {
		fmt.Fprintf(&b, "export %s=%s\n", EnvVarName(n), overlayIPByNode[n])
	}
	if selfName != "" {
		fmt.Fprintf(&b, "export ENVCORE_SELF_NAME=%s\n", selfName)
		if ip, ok := overlayIPByNode[selfName]; ok {
			fmt.Fprintf(&b, "export ENVCORE_SELF_IP=%s\n", ip)
		}
	}
	return b.String()
}
