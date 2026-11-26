// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Shared pieces of the `up` flows (single-node and cluster) that decide what
// happens around failures: the build window (sync → setup → build), rollback of
// a provision that never finished, and the "how to reach / remove this node"
// hints. Fixes for gpu/docs/pandion_issues.md #1, #3, #11.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yedidiaSch/pandion/internal/orchestrator"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Provisioning deadlines: how long the provider may take to report a server
// running. GPU images are large and Lambda boots were measured at 2–15 minutes,
// so GPU nodes get far more room. Overridable with --provision-timeout.
const (
	defaultProvisionTimeoutCPU = 10 * time.Minute
	defaultProvisionTimeoutGPU = 25 * time.Minute
	// defaultSetupTimeout bounds everything AFTER the server is running: cloud-init
	// (apt toolchain), sync, setup, build, firewall, launch. It is a separate budget
	// so a slow boot can no longer starve setup. Overridable with --setup-timeout.
	defaultSetupTimeout = 30 * time.Minute
	// rollbackTimeout bounds the cleanup of a failed provision.
	rollbackTimeout = 6 * time.Minute
)

// provisionTimeoutFor picks the provisioning deadline: an explicit flag value
// wins, else the CPU/GPU default.
func provisionTimeoutFor(flagVal time.Duration, gpu bool) time.Duration {
	if flagVal > 0 {
		return flagVal
	}
	if gpu {
		return defaultProvisionTimeoutGPU
	}
	return defaultProvisionTimeoutCPU
}

// setupTimeoutFor picks the post-provision deadline (flag value or default).
func setupTimeoutFor(flagVal time.Duration) time.Duration {
	if flagVal > 0 {
		return flagVal
	}
	return defaultSetupTimeout
}

// ensureLogsDir creates ~/.pandion/logs/<id> up front, so the path every message
// points at ("keys + logs kept in …") actually exists, even for a run that never
// streams output (#7).
func ensureLogsDir(id string) string {
	dir := filepath.Join(pandionDir(), "logs", id)
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// buildWindow is one node's egress-open setup sequence.
type buildWindow struct {
	node         string
	addr         string
	signer       gossh.Signer
	pinned       gossh.PublicKey
	setup        []string  // --setup / cluster.yaml setup:, run as root
	sync         *syncSpec // workspace to upload (+ optional build); nil = none
	runUser      string    // native engine: owner of the synced files / build user
	engine       string    // "native" | "docker"
	containerImg string    // engine=docker image
}

// runBuildWindow runs a node's build window, in the order that lets each step use
// the previous one's output (#1):
//
//  1. sync the workspace   — so setup scripts that live in it exist on the node
//  2. setup commands       — may run those scripts; installs non-apt software
//  3. (docker) pull image  — while egress is still open
//  4. build                — can use what setup installed
//
// Returns the remote workspace dir ("" if nothing was synced). Every step runs
// before the firewall lockdown, with egress open.
func runBuildWindow(ctx context.Context, b buildWindow) (string, error) {
	workdir := ""
	if b.sync != nil {
		owner := b.runUser
		if b.engine == "docker" {
			owner = "root" // the container mounts it; ownership is root's
		}
		fmt.Printf("[%s] workspace sync...\n", b.node)
		wd, err := syncFiles(ctx, b.addr, b.signer, b.pinned, *b.sync, owner)
		if err != nil {
			return "", err
		}
		workdir = wd
	}
	if err := runSetup(ctx, b.addr, b.signer, b.pinned, b.node, b.setup); err != nil {
		return workdir, err
	}
	if b.engine == "docker" {
		fmt.Printf("[%s] pulling image %s...\n", b.node, b.containerImg)
		if out, err := envssh.Run(ctx, b.addr, "root", b.signer, b.pinned,
			"docker pull "+shellQuote(b.containerImg)); err != nil {
			return workdir, fmt.Errorf("docker pull failed: %v\n%s", err, out)
		}
		if b.sync != nil {
			if bc := strings.TrimSpace(b.sync.Build); bc != "" {
				fmt.Printf("[%s] building in container (%s): %s\n", b.node, b.containerImg, bc)
				if out, err := envssh.Run(ctx, b.addr, "root", b.signer, b.pinned,
					dockerRun(b.containerImg, workdir, bc, nil)); err != nil {
					return workdir, fmt.Errorf("container build failed: %v\n%s", err, out)
				}
			}
		}
		return workdir, nil
	}
	if b.sync != nil {
		if err := runWorkspaceBuild(ctx, b.addr, b.signer, b.pinned, *b.sync, b.runUser, workdir); err != nil {
			return workdir, err
		}
	}
	return workdir, nil
}

// liveNodeHints prints how to reach and remove a cluster that was left running
// (setup failed, or the run ended). The manifest is written as soon as the
// servers exist, so `pandion ssh` works here (#3).
func liveNodeHints(prov, id string) {
	fmt.Printf("node is live (and billing).\n")
	fmt.Printf("  debug:    pandion ssh --id %s\n", id)
	fmt.Printf("  teardown: pandion down --provider=%s --id %s\n", prov, id)
}

// rollbackFailedProvision handles a provision that never finished (#11). The
// provider may already have launched instances (Lambda accepts the launch, then
// boots for minutes), so a bare "context deadline exceeded" would orphan a
// billing node. It tears down whatever exists for the cluster — via the provider
// listing AND the server ids journaled during the launch — and reports the
// outcome truthfully. It never returns:
//
//   - rolled back, nothing left          → exit codeRollback (5)
//   - teardown accepted, still terminating → exit codeReapFailure (8) + re-check hint
//   - teardown failed                    → exit codeReapFailure (8) + the exact `down` command
func rollbackFailedProvision(o *orchestrator.Orchestrator, prov, id string, provErr error) {
	fmt.Fprintf(os.Stderr, "error: provisioning failed: %s\n", friendlyErr(provErr))
	if errors.Is(provErr, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "  the provider did not report the server running within the provisioning deadline.\n")
		fmt.Fprintf(os.Stderr, "  slow-booting providers (Lambda GPU nodes: 2–15+ min) may need a longer one: --provision-timeout 40m\n")
	}
	fmt.Fprintf(os.Stderr, "rolling back: destroying anything launched for cluster %q at %s (it may already exist and bill)...\n", id, prov)
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	err := o.Down(ctx, id)
	switch {
	case err == nil:
		tombstoneManifest(id)
		fmt.Fprintf(os.Stderr, "rolled back: nothing left at %s for %q.\n", prov, id)
		os.Exit(codeRollback)
	case orchestrator.IsStillTerminating(err):
		fmt.Fprintf(os.Stderr, "rollback: %v\n", err)
		fmt.Fprintf(os.Stderr, "  check again:  pandion ls --provider %s   /   pandion down --provider=%s --id %s\n", prov, prov, id)
		os.Exit(codeReapFailure)
	default:
		fmt.Fprintf(os.Stderr, "rollback FAILED: %v\n", err)
		fmt.Fprintf(os.Stderr, "  an instance may still be running AND BILLING. remove it with:\n")
		fmt.Fprintf(os.Stderr, "    pandion down --provider=%s --id %s\n", prov, id)
		os.Exit(codeReapFailure)
	}
}

// progressPrinter forwards provider launch progress to the operator (#10).
func progressPrinter(prefix string) func(string) {
	return func(msg string) { statusln(prefix + msg) }
}

// egressLockdownNote explains what the egress default-deny means for later work
// on the node (#8): anything that reaches out (apt, pip, curl, git clone) over
// `pandion ssh` is now refused, and names the flags that install software before
// the lockdown or open specific destinations.
func egressLockdownNote() string {
	return "  note: outbound connections from the node are now refused (apt/pip/curl/git fail with \"connection refused\").\n" +
		"        install apt packages with --packages and other software with --setup (both run BEFORE the lockdown),\n" +
		"        or allow specific destinations with --egress-allow host1,host2 (cluster.yaml: egress_allow).\n"
}
