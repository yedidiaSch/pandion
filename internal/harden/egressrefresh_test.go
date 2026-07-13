// SPDX-License-Identifier: AGPL-3.0-or-later

package harden

import (
	"strings"
	"testing"
	"time"
)

func TestEgressRefreshInstall(t *testing.T) {
	out := EgressRefreshInstall([]string{"github.com", "  api.example.org  ", "github.com"}, 2*time.Minute)

	// de-duplicated hostnames land in the on-node file
	for _, want := range []string{"github.com", "api.example.org"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing hostname %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "github.com") != 1 {
		t.Errorf("github.com should appear once (de-duped):\n%s", out)
	}
	// installs the refresher, the units, and enables the timer
	for _, want := range []string{
		"/usr/local/sbin/pandion-egress-refresh",
		"pandion-egress-refresh.service",
		"pandion-egress-refresh.timer",
		"OnUnitActiveSec=120",
		"systemctl enable --now pandion-egress-refresh.timer",
		"nft add element $SET",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install script missing %q:\n%s", want, out)
		}
	}
}

// Interval below the floor is clamped; sub-30s must not produce a hot loop.
func TestEgressRefreshInstall_ClampsInterval(t *testing.T) {
	out := EgressRefreshInstall([]string{"example.com"}, time.Second)
	if !strings.Contains(out, "OnUnitActiveSec=30") {
		t.Errorf("interval must be clamped to 30s floor:\n%s", out)
	}
}

// No valid hostnames ⇒ nothing to install. IPs/CIDRs and injection attempts are
// not hostnames and must be dropped.
func TestEgressRefreshInstall_NoHostnames(t *testing.T) {
	if out := EgressRefreshInstall(nil, time.Minute); out != "" {
		t.Errorf("no hostnames should render nothing, got:\n%s", out)
	}
	if out := EgressRefreshInstall([]string{"evil.com; rm -rf /", "a b", ""}, time.Minute); out != "" {
		t.Errorf("only shell-unsafe/blank entries should render nothing, got:\n%s", out)
	}
}
