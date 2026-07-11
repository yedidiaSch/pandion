// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/yedidiaSch/pandion/internal/provider/digitalocean"
	"github.com/yedidiaSch/pandion/internal/provider/hetzner"
	"github.com/yedidiaSch/pandion/internal/provider/lambda"
	"github.com/yedidiaSch/pandion/internal/provider/linode"
	"github.com/yedidiaSch/pandion/internal/provider/scaleway"
	"github.com/yedidiaSch/pandion/internal/provider/vultr"
)

// Regression guard for the G1 bug where `up --provider lambda` silently no-opped:
// lambda resolved and priced but was missing from the `up` dispatch switch, so it
// fell through and exited 0 without provisioning. Every real provider's Name()
// MUST be recognized by the up dispatch (isCloudProvider, derived from
// cloudProviders) — otherwise `up` does nothing.
func TestEveryProviderHasUpDispatch(t *testing.T) {
	scw, _ := scaleway.New("secret", "access", "project")
	providers := []interface{ Name() string }{
		hetzner.New("k"),
		digitalocean.New("k"),
		vultr.New("k"),
		linode.New("k"),
		scw,
		lambda.New("k"),
	}
	for _, p := range providers {
		if !isCloudProvider(p.Name()) {
			t.Errorf("provider %q resolves but is absent from the `up` dispatch — `up --provider %s` would silently no-op",
				p.Name(), p.Name())
		}
	}
}
