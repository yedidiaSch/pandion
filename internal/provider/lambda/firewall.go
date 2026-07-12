// SPDX-License-Identifier: AGPL-3.0-or-later

package lambda

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// Lambda's firewall is ACCOUNT-WIDE (one inbound rule set for all your instances)
// and defaults to SSH + ICMP only — so the WireGuard overlay port (UDP 51820) is
// closed and the operator can't reach a node over the mesh, which makes `lockdown`
// refuse (it verifies overlay reach first, to avoid locking you out). Hetzner/DO
// open the port at their per-cluster cloud firewall; Lambda has no per-cluster
// firewall, so we ensure the port on the account-wide rule set instead —
// ADDITIVELY (we never remove your existing rules), and it persists after teardown
// (harmless: WireGuard is encrypted and the port is inert with no node listening).

// fwRule mirrors a Lambda firewall rule (GET/PUT /firewall-rules).
type fwRule struct {
	Protocol      string `json:"protocol"`
	PortRange     []int  `json:"port_range,omitempty"` // [lo,hi]; absent for icmp
	SourceNetwork string `json:"source_network"`
	Description   string `json:"description,omitempty"`
}

// EnsureClusterFirewall implements provider.ClusterFirewaller. Lambda's firewall
// is account-wide, so clusterID is not used for scoping; we ensure inbound UDP
// wgPort (WireGuard) is allowed, adding it only if absent. SSH + ICMP are left as
// they are (Lambda allows them by default).
func (l *Lambda) EnsureClusterFirewall(ctx context.Context, _ string, wgPort int) error {
	var cur struct {
		Data []fwRule `json:"data"`
	}
	if err := l.do(ctx, http.MethodGet, "/firewall-rules", nil, &cur); err != nil {
		return fmt.Errorf("list firewall rules: %w", err)
	}
	for _, r := range cur.Data {
		if strings.EqualFold(r.Protocol, "udp") && len(r.PortRange) == 2 &&
			r.PortRange[0] <= wgPort && wgPort <= r.PortRange[1] {
			return nil // already open — idempotent
		}
	}
	rules := append(cur.Data, fwRule{
		Protocol:      "udp",
		PortRange:     []int{wgPort, wgPort},
		SourceNetwork: "0.0.0.0/0",
		Description:   "Pandion WireGuard overlay",
	})
	body := struct {
		Data []fwRule `json:"data"`
	}{Data: rules}
	if err := l.do(ctx, http.MethodPut, "/firewall-rules", body, nil); err != nil {
		return fmt.Errorf("open WireGuard port udp/%d in the Lambda account firewall: %w", wgPort, err)
	}
	return nil
}

var _ provider.ClusterFirewaller = (*Lambda)(nil)
