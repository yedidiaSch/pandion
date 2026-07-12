// SPDX-License-Identifier: AGPL-3.0-or-later

package lambda

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider"
)

// fwServer emulates Lambda's account-wide firewall (GET returns the current rules,
// PUT replaces them), starting from the real default (SSH + ICMP only).
func fwServer(t *testing.T) (*Lambda, *[]fwRule) {
	t.Helper()
	rules := []fwRule{
		{Protocol: "tcp", PortRange: []int{22, 22}, SourceNetwork: "0.0.0.0/0", Description: "SSH"},
		{Protocol: "icmp", SourceNetwork: "0.0.0.0/0", Description: "Ping"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/firewall-rules", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string][]fwRule{"data": rules})
		case http.MethodPut:
			var body struct {
				Data []fwRule `json:"data"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			rules = body.Data
			json.NewEncoder(w).Encode(map[string][]fwRule{"data": rules})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	l := New("k", WithBaseURL(srv.URL+"/api/v1"), WithHTTPClient(srv.Client()))
	return l, &rules
}

func TestImplementsClusterFirewaller(t *testing.T) {
	var _ provider.ClusterFirewaller = New("k")
}

func TestEnsureClusterFirewall_AddsWGPort(t *testing.T) {
	l, rules := fwServer(t)
	if err := l.EnsureClusterFirewall(context.Background(), "any", 51820); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// SSH + ICMP preserved, WG added (additive).
	var haveSSH, haveWG bool
	for _, r := range *rules {
		if r.Protocol == "tcp" && len(r.PortRange) == 2 && r.PortRange[0] == 22 {
			haveSSH = true
		}
		if r.Protocol == "udp" && len(r.PortRange) == 2 && r.PortRange[0] == 51820 {
			haveWG = true
		}
	}
	if !haveSSH {
		t.Error("SSH rule must be preserved (additive)")
	}
	if !haveWG {
		t.Errorf("WireGuard udp/51820 rule not added: %+v", *rules)
	}
}

func TestEnsureClusterFirewall_Idempotent(t *testing.T) {
	l, rules := fwServer(t)
	ctx := context.Background()
	if err := l.EnsureClusterFirewall(ctx, "c", 51820); err != nil {
		t.Fatal(err)
	}
	n := len(*rules)
	if err := l.EnsureClusterFirewall(ctx, "c", 51820); err != nil {
		t.Fatal(err)
	}
	if len(*rules) != n {
		t.Fatalf("second call must not add a duplicate rule: %d -> %d", n, len(*rules))
	}
}
