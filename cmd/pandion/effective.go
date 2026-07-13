// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/yedidiaSch/pandion/internal/config"
	"github.com/yedidiaSch/pandion/internal/harden"
	"github.com/yedidiaSch/pandion/internal/userconfig"
)

// The intended resolution order (P2.4), highest priority first:
//
//	flag > env > cluster.yaml > ~/.pandion/config.yaml > built-in default
//
// resolveKnob walks candidates in that order and returns the first non-empty one
// with its source label — the single mechanism behind `validate --show-effective`,
// which answers "why did Pandion pick that value?".

// knobCandidate is one possible value for a setting and where it came from.
type knobCandidate struct {
	Value string
	From  string
}

// effectiveKnob is the resolved value of a setting plus the source that won.
type effectiveKnob struct {
	Name   string
	Value  string
	Source string
}

// resolveKnob returns the first candidate with a non-empty value; if none apply it
// falls back to fallback with source "built-in default".
func resolveKnob(name string, fallback string, cands ...knobCandidate) effectiveKnob {
	for _, c := range cands {
		if strings.TrimSpace(c.Value) != "" {
			return effectiveKnob{Name: name, Value: c.Value, Source: c.From}
		}
	}
	return effectiveKnob{Name: name, Value: fallback, Source: "built-in default"}
}

// effectiveKnobs computes the effective provider/region/size/ttl/engine for a
// cluster.yaml (cl may be nil when only the operator config applies) against the
// operator config and built-in defaults. Flags/env aren't included here — this is
// the `validate --show-effective` view, which reasons about the *files*.
func effectiveKnobs(cl *config.Cluster, cfg *userconfig.Config) []effectiveKnob {
	var clProvider, clRegion, clSize, clEngine, clTTL string
	if cl != nil {
		clProvider = cl.Provider.Name
		clRegion = cl.Provider.Region
		clSize = cl.Defaults.Size
		clEngine = cl.Defaults.Engine
		clTTL = cl.Defaults.TTL
	}
	return []effectiveKnob{
		resolveKnob("provider", "(inferred from credentials)",
			knobCandidate{clProvider, "cluster.yaml provider.name"},
			knobCandidate{cfg.DefaultProvider, "~/.pandion/config.yaml"}),
		resolveKnob("region", "(provider's choice)",
			knobCandidate{clRegion, "cluster.yaml provider.region"},
			knobCandidate{cfg.Defaults.Region, "~/.pandion/config.yaml"}),
		resolveKnob("size", "(auto-selected)",
			knobCandidate{clSize, "cluster.yaml defaults.size"},
			knobCandidate{cfg.Defaults.Size, "~/.pandion/config.yaml"}),
		resolveKnob("ttl", shortDur(harden.DefaultIdleTTL),
			knobCandidate{clTTL, "cluster.yaml defaults.ttl"},
			knobCandidate{cfg.Defaults.TTL, "~/.pandion/config.yaml"}),
		resolveKnob("engine", "native",
			knobCandidate{clEngine, "cluster.yaml defaults.engine"}),
	}
}

// showEffective prints the effective value + winning source for each knob (P2.4).
// clusterPath may be "" to show just config+defaults (no topology file).
func showEffective(w io.Writer, clusterPath string) {
	cfg, _ := userconfig.LoadProfile(envHome(), activeProfile)
	var cl *config.Cluster
	if clusterPath != "" {
		if _, err := os.Stat(clusterPath); err == nil {
			if c, err := config.Load(clusterPath); err == nil {
				cl = c
			}
		}
	}
	fmt.Fprintln(w, "effective settings (precedence: flag > env > cluster.yaml > ~/.pandion/config.yaml > default):")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  KNOB\tVALUE\tSOURCE")
	for _, k := range effectiveKnobs(cl, cfg) {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", k.Name, k.Value, k.Source)
	}
	tw.Flush()
}
