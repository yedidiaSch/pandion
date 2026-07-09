// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "testing"

func TestRelayBaseURL(t *testing.T) {
	cases := []struct {
		r    relayRecord
		want string
	}{
		{relayRecord{IP: "1.2.3.4", Port: 8443}, "https://1.2.3.4:8443"},
		{relayRecord{IP: "1.2.3.4", Port: 443}, "https://1.2.3.4"}, // :443 dropped
		{relayRecord{IP: "1.2.3.4", Domain: "lab.example.com", Port: 443}, "https://lab.example.com"},
		{relayRecord{IP: "1.2.3.4", Domain: "lab.example.com", Port: 9000}, "https://lab.example.com:9000"},
	}
	for _, c := range cases {
		if got := c.r.baseURL(); got != c.want {
			t.Errorf("baseURL(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestRelayProvisionScript_DomainAddsCapAndDomainFlag(t *testing.T) {
	// self-signed: --hosts, no bind capability.
	self := relayProvisionScript(8443, "1.2.3.4", "")
	if !contains([]string{self}, self) || // trivially true; assert content below
		!containsSub(self, "--hosts 1.2.3.4") || containsSub(self, "CAP_NET_BIND_SERVICE") {
		t.Errorf("self-signed unit wrong:\n%s", self)
	}
	// domain: --domain flag + the bind capability, no --hosts.
	dom := relayProvisionScript(443, "1.2.3.4", "lab.example.com")
	if !containsSub(dom, "--domain lab.example.com") || !containsSub(dom, "AmbientCapabilities=CAP_NET_BIND_SERVICE") {
		t.Errorf("domain unit missing --domain/cap:\n%s", dom)
	}
	if containsSub(dom, "--hosts") {
		t.Errorf("domain unit should not pass --hosts:\n%s", dom)
	}
}
