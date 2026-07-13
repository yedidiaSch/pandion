// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/stream"
	gossh "golang.org/x/crypto/ssh"
)

// runStart implements `pandion start` — launch the run command(s) on a cluster
// or node that was deployed but not started (e.g. `up --no-run`, or a node with
// no `run:`). It works entirely from the persisted manifest (node IPs, pinned
// host keys, and the run spec saved at `up`), so it needs no cluster.yaml. The
// workloads run in the same durable tmux sessions as `up`, then stream live;
// Ctrl+C detaches (the workloads keep running — reattach with `pandion attach`).
func runStart(args []string) {
	fs := newCmdFlagSet("start")
	id := fs.String("id", "", "cluster id (required)")
	node := fs.String("node", "", "only start this node (default: all runnable nodes)")
	detach := fs.Bool("detach", false, "launch the workloads but do not stream (return immediately)")
	_ = fs.Parse(args)

	if *id == "" {
		fmt.Fprintln(os.Stderr, "start: --id is required")
		os.Exit(2)
	}

	initAudit()
	// serialize against a concurrent up/down/reap on this id (P0.5).
	lk := lockClusterOrExit(*id)
	defer lk.Unlock()
	if err := startCluster(*id, *node, *detach); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// startCluster launches and (unless detach) streams the run commands recorded in
// the manifest. A node is "runnable" if its manifest carries a non-empty Run.
func startCluster(id, only string, detach bool) error {
	man, err := loadManifest(id)
	if err != nil {
		bailIfTornDown(err)
		return fmt.Errorf("no manifest for %q (is the id correct? manifest lives in ~/.pandion/keys/%s/): %w", id, id, err)
	}
	signer, err := loadLoginSigner(id)
	if err != nil {
		return err
	}

	// select the nodes to start: those with a run command, optionally filtered.
	sel, skipped, err := selectStartNodes(man.Nodes, only, id)
	if err != nil {
		return err
	}

	// launch each workload in its detached tmux session (idempotent — a re-start
	// replaces the session), reconstructing the exact run shell from the manifest.
	ctx := context.Background()
	for _, n := range sel {
		pinned, perr := parsePinned(n.HostPub)
		if perr != nil {
			return fmt.Errorf("bad host key for %s: %w", n.Name, perr)
		}
		var runShell string
		if n.Engine == "docker" {
			runShell = dockerRun(n.ContainerImg, n.Workdir, n.Run, n.Caps)
		} else {
			runShell = runAs(n.RunUser, n.Workdir, n.Run, n.Caps)
		}
		if err := launchRun(ctx, n.IP+":22", signer, pinned, runShell); err != nil {
			return fmt.Errorf("launch on %s failed: %w", n.Name, err)
		}
		audit.Event("start", "id", id, "node", n.Name)
	}
	fmt.Printf("started %d node(s): %s\n", len(sel), strings.Join(nodeNames(sel), ", "))
	if len(skipped) > 0 && only == "" {
		fmt.Printf("  (skipped deploy-only node(s) with no run: %s)\n", strings.Join(skipped, ", "))
	}
	if detach {
		return nil
	}

	// stream the launched workloads (multiplexed), mirroring `attach`.
	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; fmt.Println("\n^C — detaching; workloads left running."); cancel() }()
	defer signal.Stop(sig)

	printer := stream.NewPrinter(os.Stdout, filepath.Join(pandionDir(), "logs", id), colorEnabled())
	defer printer.Close()
	statusf("streaming %d node(s) (Ctrl+C detaches; reattach: pandion attach --id %s)\n", len(sel), id)
	statusln("----------------------------------------------------------------")
	var wg sync.WaitGroup
	for _, n := range sel {
		pinned, _ := parsePinned(n.HostPub) // already validated above
		wg.Add(1)
		go func(name, ip string, pk gossh.PublicKey) {
			defer wg.Done()
			tailLog(streamCtx, ip+":22", signer, pk, name, printer)
		}(n.Name, n.IP, pinned)
	}
	wg.Wait()
	return nil
}

// selectStartNodes picks the manifest nodes to start: those carrying a non-empty
// Run, optionally filtered to `only`. It returns the selected nodes, the names of
// deploy-only nodes it skipped (no run), and a helpful error when the selection is
// empty (unknown node, a deploy-only node named explicitly, or nothing runnable).
func selectStartNodes(nodes []nodeManifest, only, id string) (sel []nodeManifest, skipped []string, err error) {
	for _, n := range nodes {
		if only != "" && n.Name != only {
			continue
		}
		if strings.TrimSpace(n.Run) == "" {
			skipped = append(skipped, n.Name)
			continue
		}
		sel = append(sel, n)
	}
	if len(sel) > 0 {
		return sel, skipped, nil
	}
	if only != "" {
		if contains(nodeNames(nodes), only) {
			return nil, nil, fmt.Errorf("node %q has no run command to start (deploy-only). Run it manually: pandion ssh --id %s --node %s", only, id, only)
		}
		return nil, nil, fmt.Errorf("no node %q in cluster %q", only, id)
	}
	return nil, nil, fmt.Errorf("nothing to start: no node in cluster %q has a run command", id)
}

func nodeNames(ns []nodeManifest) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Name
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
