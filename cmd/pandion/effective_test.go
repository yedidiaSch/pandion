// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/yedidiaSch/pandion/internal/config"
	"github.com/yedidiaSch/pandion/internal/userconfig"
)

// TestEffectiveKnobsPrecedence is the P2.4 matrix: cluster.yaml beats config.yaml
// beats the built-in default, and the winning source is reported.
func TestEffectiveKnobsPrecedence(t *testing.T) {
	find := func(ks []effectiveKnob, name string) effectiveKnob {
		for _, k := range ks {
			if k.Name == name {
				return k
			}
		}
		t.Fatalf("no knob %q", name)
		return effectiveKnob{}
	}

	t.Run("cluster.yaml wins over config", func(t *testing.T) {
		cl := &config.Cluster{}
		cl.Provider.Region = "fsn1"
		cl.Defaults.Size = "cpx31"
		cfg := &userconfig.Config{Defaults: userconfig.Defaults{Region: "nbg1", Size: "cpx11"}}
		ks := effectiveKnobs(cl, cfg)
		if k := find(ks, "region"); k.Value != "fsn1" || k.Source != "cluster.yaml provider.region" {
			t.Errorf("region = %+v, want fsn1 from cluster.yaml", k)
		}
		if k := find(ks, "size"); k.Value != "cpx31" {
			t.Errorf("size = %+v, want cpx31", k)
		}
	})

	t.Run("config used when cluster.yaml silent", func(t *testing.T) {
		cfg := &userconfig.Config{DefaultProvider: "hetzner", Defaults: userconfig.Defaults{Size: "cpx11", TTL: "3h"}}
		ks := effectiveKnobs(nil, cfg)
		if k := find(ks, "provider"); k.Value != "hetzner" || k.Source != "~/.pandion/config.yaml" {
			t.Errorf("provider = %+v, want hetzner from config", k)
		}
		if k := find(ks, "ttl"); k.Value != "3h" {
			t.Errorf("ttl = %+v, want 3h", k)
		}
	})

	t.Run("built-in default when nothing set", func(t *testing.T) {
		ks := effectiveKnobs(nil, &userconfig.Config{})
		if k := find(ks, "engine"); k.Value != "native" || k.Source != "built-in default" {
			t.Errorf("engine = %+v, want native built-in", k)
		}
		if k := find(ks, "size"); k.Source != "built-in default" {
			t.Errorf("size source = %q, want built-in default", k.Source)
		}
	})
}
