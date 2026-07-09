// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "testing"

func TestPickProvider(t *testing.T) {
	cases := []struct {
		name          string
		explicit, cfg string
		creds         []string
		tty           bool
		wantProv      string
		wantPrompt    bool
	}{
		{"explicit wins", "hetzner", "digitalocean", []string{"vultr"}, false, "hetzner", false},
		{"explicit mock", "mock", "", nil, false, "mock", false},
		{"config default", "", "digitalocean", nil, true, "digitalocean", false},
		{"infer single cred", "", "", []string{"vultr"}, false, "vultr", false},
		{"ambiguous on tty -> prompt", "", "", []string{"a", "b"}, true, "", true},
		{"ambiguous no tty -> error", "", "", []string{"a", "b"}, false, "", false},
		{"none on tty -> prompt", "", "", nil, true, "", true},
		{"none no tty -> error", "", "", nil, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, prompt := pickProvider(c.explicit, c.cfg, c.creds, c.tty)
			if p != c.wantProv || prompt != c.wantPrompt {
				t.Fatalf("pickProvider = (%q,%v), want (%q,%v)", p, prompt, c.wantProv, c.wantPrompt)
			}
		})
	}
}
