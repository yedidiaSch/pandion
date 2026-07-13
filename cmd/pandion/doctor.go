// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// doctorRow is one line of the doctor report (also the JSON element).
type doctorRow struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	Local    string `json:"local"`         // torn-down | journal | manifest
	Cloud    *int   `json:"cloud"`         // live server count; null = not checked
	Status   string `json:"status"`        // running | stale | torn-down | LEAK | unchecked
	Leak     bool   `json:"leak"`          // true only for the LEAK status
	Fix      string `json:"fix,omitempty"` // the command that resolves it, if any
}

// runDoctor reports where the local state under ~/.pandion diverges from the
// provider's truth (F6/R7): stale journals/manifests for clusters that no longer
// exist, tombstones that still have running servers (a real leak), and local
// leftovers — each with the command that fixes it. It reconciles against a
// provider only when that provider's credentials are available; otherwise it
// reports the local view and says the cloud wasn't checked. Exit is non-zero
// only when a LEAK (running servers not reflected locally) is found, so a script
// or CI can gate on it.
func runDoctor(args []string) {
	fs := newCmdFlagSet("doctor")
	jsonOut := fs.Bool("json", false, "emit the report as JSON (stable schema)")
	_ = fs.Parse(args)

	ids := localStateIDs()
	if len(ids) == 0 {
		if *jsonOut {
			fmt.Println("[]")
		} else {
			fmt.Println("no local Pandion state under", filepath.Join(pandionDir(), "{state,keys}"))
		}
		return
	}

	// group ids by their recorded provider so we query each provider once.
	byProvider := map[string][]string{}
	facts := map[string]localFacts{}
	for _, id := range ids {
		f := gatherLocalFacts(id)
		facts[id] = f
		byProvider[f.provider] = append(byProvider[f.provider], id)
	}

	// reconcile against provider truth where credentials exist. The mock is only
	// checkable when persistent (PANDION_MOCK_STATE) — an in-memory mock has no
	// cross-process truth, so it stays "unchecked" rather than falsely "stale".
	mockCheckable := strings.TrimSpace(os.Getenv("PANDION_MOCK_STATE")) != ""
	running := map[string]int{} // id -> live server count; absent = not checked
	for prov, pids := range byProvider {
		if prov == "" {
			continue
		}
		if prov == "mock" && !mockCheckable {
			continue
		}
		if prov != "mock" && !hasCreds(prov) {
			continue
		}
		p, err := newProvider(prov)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		servers, err := p.ListAllTagged(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "doctor: could not query %s (%v) — its clusters shown unchecked\n", prov, err)
			continue
		}
		counts := map[string]int{}
		for _, s := range servers {
			counts[s.ClusterID]++
		}
		for _, id := range pids {
			running[id] = counts[id] // 0 if the provider has nothing for it
		}
	}

	sort.Strings(ids)
	rows := make([]doctorRow, 0, len(ids))
	leaks := 0
	for _, id := range ids {
		f := facts[id]
		n, checked := running[id]
		status, fix := classifyDoctor(f.tombstoned, checked, n)
		leak := strings.HasPrefix(status, "LEAK")
		if leak {
			leaks++
		}
		row := doctorRow{ID: id, Provider: f.provider, Local: f.localLabel(), Status: status, Leak: leak, Fix: strings.ReplaceAll(fix, "<id>", id)}
		if checked {
			nn := n
			row.Cloud = &nn
		}
		rows = append(rows, row)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		must(enc.Encode(rows))
	} else {
		fmt.Printf("%-24s %-12s %-14s %-8s %s\n", "CLUSTER", "PROVIDER", "LOCAL", "CLOUD", "STATUS")
		for _, r := range rows {
			cloud := "?"
			if r.Cloud != nil {
				cloud = fmt.Sprintf("%d", *r.Cloud)
			}
			fmt.Printf("%-24s %-12s %-14s %-8s %s\n", r.ID, dashIfEmpty(r.Provider), r.Local, cloud, r.Status)
			if r.Fix != "" {
				fmt.Printf("  ↳ %s\n", r.Fix)
			}
		}
	}

	if leaks > 0 {
		fmt.Fprintf(os.Stderr, "\n%d cluster(s) marked torn down locally but still running at the provider — money is leaking.\n", leaks)
		os.Exit(codeInfraDegraded)
	}
}

// localFacts is the local view of one cluster id.
type localFacts struct {
	provider   string
	tombstoned bool
	hasJournal bool
	updated    time.Time
}

func (f localFacts) localLabel() string {
	switch {
	case f.tombstoned:
		return "torn-down"
	case f.hasJournal:
		return "journal"
	default:
		return "manifest"
	}
}

// classifyDoctor maps (tombstoned, cloudChecked, running) to a status + fix hint.
// Pure, so it is unit-testable without any filesystem or provider.
func classifyDoctor(tombstoned, checked bool, running int) (status, fix string) {
	if !checked {
		if tombstoned {
			return "torn-down (local)", "local tombstone; safe to delete keys/logs for <id> if you're done with it"
		}
		return "unchecked", "provider creds absent — set them (or `pandion login`) to verify <id> against the cloud"
	}
	switch {
	case running > 0 && tombstoned:
		return fmt.Sprintf("LEAK (%d running)", running), "marked torn down but still running — `pandion down --id <id>`"
	case running > 0:
		return "running", ""
	case tombstoned:
		return "torn-down", "provider is empty; safe to delete keys/logs for <id>"
	default:
		return "stale", "no servers at the provider — `pandion down --id <id>` to clear the stale local state"
	}
}

// localStateIDs is the union of ids that have a state journal or a key manifest.
func localStateIDs() []string {
	seen := map[string]bool{}
	for _, id := range localClusterIDs() { // state/*.json
		seen[id] = true
	}
	if entries, err := os.ReadDir(filepath.Join(pandionDir(), "keys")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(pandionDir(), "keys", e.Name(), "manifest.json")); err == nil {
					seen[e.Name()] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

// gatherLocalFacts reads the manifest (for provider + tombstone) and the journal
// (for existence + last update) for one id. Reads raw so a tombstone is a fact,
// not an error.
func gatherLocalFacts(id string) localFacts {
	var f localFacts
	if b, err := os.ReadFile(manifestPath(id)); err == nil {
		var m clusterManifest
		if json.Unmarshal(b, &m) == nil {
			f.provider = m.Provider
			f.tombstoned = m.DestroyedAt != ""
		}
	}
	if c, err := mustStore().Load(id); err == nil {
		f.hasJournal = true
		f.updated = c.Updated
		if f.provider == "" {
			f.provider = c.Provider
		}
	}
	return f
}
