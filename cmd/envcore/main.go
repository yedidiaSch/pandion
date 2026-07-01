// Command envcore is the M1 CLI: a single-node provision/run/teardown flow over
// either the in-memory mock provider (default, free, offline) or the real
// Hetzner provider (--provider=hetzner), with security-hardened bootstrap.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/envcore/envcore/internal/harden"
	"github.com/envcore/envcore/internal/orchestrator"
	"github.com/envcore/envcore/internal/provider"
	"github.com/envcore/envcore/internal/provider/hetzner"
	"github.com/envcore/envcore/internal/provider/mock"
	envssh "github.com/envcore/envcore/internal/ssh"
	"github.com/envcore/envcore/internal/sshkeys"
	"github.com/envcore/envcore/internal/state"
)

const version = "0.1.0-m1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("envcore", version)
	case "demo":
		runDemo()
	case "up":
		runUp(os.Args[2:])
	case "down":
		runDown(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

// splitRunCmd separates flag args from the user run command after "--".
func splitRunCmd(args []string) (flags []string, runCmd string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], strings.Join(args[i+1:], " ")
		}
	}
	return args, ""
}

func newProvider(name string) (provider.Provider, error) {
	switch name {
	case "mock", "":
		return mock.New(), nil
	case "hetzner":
		token := os.Getenv("HCLOUD_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("HCLOUD_TOKEN not set (required for --provider=hetzner)")
		}
		return hetzner.New(token), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (use mock|hetzner)", name)
	}
}

func runUp(args []string) {
	flagArgs, runCmd := splitRunCmd(args)
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	prov := fs.String("provider", "mock", "provider: mock|hetzner")
	id := fs.String("id", "demo", "cluster id")
	node := fs.String("node", "node-a", "node name")
	noToolchain := fs.Bool("no-toolchain", false, "skip installing the C++ toolchain (faster)")
	_ = fs.Parse(flagArgs)

	p, err := newProvider(*prov)
	must(err)
	o := orchestrator.New(p, mustStore())

	switch p.Name() {
	case "mock":
		c, err := o.Up(context.Background(), *id, *node, "#cloud-config\n", "")
		must(err)
		fmt.Printf("UP (mock): cluster %q node %q -> %s\n", c.ID, *node, c.Nodes[0].Phase)
		fmt.Println("note: mock provider creates no cloud resources and runs no SSH.")
	case "hetzner":
		upHetzner(o, *id, *node, runCmd, !*noToolchain)
	}
}

func upHetzner(o *orchestrator.Orchestrator, id, node, runCmd string, toolchain bool) {
	if runCmd == "" {
		runCmd = "echo ENVCORE_READY && uname -a"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// 1) generate the host key (to inject + pin) and the login key (S1)
	host, err := sshkeys.Generate("envcore-host")
	must(err)
	login, err := sshkeys.Generate("envcore-login")
	must(err)

	// 2) build hardened cloud-init: inject host key (race-free, S1/F1) + install
	//    the C++ toolchain per the Execution Contract (§5)
	ci := harden.CloudInit{
		HostPrivKeyPEM: host.PrivatePEM,
		HostPubKey:     host.PublicAuthorized,
		LoginPubKey:    login.PublicAuthorized,
	}
	if toolchain {
		ci.Packages = harden.DefaultToolchain()
	}
	userData := harden.Build(ci)

	// 3) provision (tagged; journaled state machine). The login key is registered
	//    with the provider so it lands on root (validated path, S1).
	c, err := o.Up(ctx, id, node, userData, login.PublicAuthorized)
	must(err)
	ip := c.Nodes[0].IP
	fmt.Printf("UP (hetzner): cluster %q node %q running at %s (host fp %s)\n",
		c.ID, node, ip, host.Fingerprint())

	// 4) persist keys for later attach/down (0600)
	keyDir := filepath.Join(envHome(), ".envcore", "keys", id)
	must(host.Save(keyDir, "host_ed25519"))
	must(login.Save(keyDir, "login_ed25519"))

	// 5) connect with the PINNED host key. RunWithRetry tolerates the cloud-init
	//    window (until our key is installed, pinning rejects and we wait), and
	//    `cloud-init status --wait` blocks until packages + hardening are applied
	//    (S1/F4) — so the toolchain is READY before we run the user command.
	addr := ip + ":22"
	fmt.Println("connecting (host-key pinned; waiting for cloud-init + toolchain)...")
	onAttempt := func(n int, reason string) { fmt.Printf("  attempt %d: %s\n", n, reason) }
	if _, err := envssh.RunWithRetry(ctx, addr, "root", login.Signer, host.Public,
		"cloud-init status --wait || true", 5*time.Second, onAttempt); err != nil {
		fmt.Printf("readiness gate failed: %v (node left running for debugging)\n", err)
		fmt.Printf("node is live. teardown with:  envcore down --provider=hetzner --id %s\n", id)
		return
	}
	fmt.Println("node ready (cloud-init complete). running command...")

	// 6) run the user command on the now-ready node.
	out, runErr := envssh.Run(ctx, addr, "root", login.Signer, host.Public, runCmd)
	fmt.Printf("---- run output ----\n%s\n--------------------\n", strings.TrimRight(out, "\n"))
	if runErr != nil {
		fmt.Printf("run finished with error: %v (node left running for debugging)\n", runErr)
	}
	fmt.Printf("node is live. teardown with:  envcore down --provider=hetzner --id %s\n", id)
}

func runDown(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	prov := fs.String("provider", "mock", "provider: mock|hetzner")
	id := fs.String("id", "demo", "cluster id")
	_ = fs.Parse(args)

	p, err := newProvider(*prov)
	must(err)
	o := orchestrator.New(p, mustStore())
	must(o.Down(context.Background(), *id))
	fmt.Printf("DOWN (%s): cluster %q reconciled to empty.\n", p.Name(), *id)
}

func runDemo() {
	o := orchestrator.New(mock.New(), mustStore())
	ctx := context.Background()
	c, err := o.Up(ctx, "demo", "node-a", "#cloud-config\n", "")
	must(err)
	fmt.Printf("UP    cluster=%q node=%q phase=%s\n", c.ID, c.Nodes[0].Name, c.Nodes[0].Phase)
	must(o.Down(ctx, "demo"))
	fmt.Println("DOWN  cluster=\"demo\" reconciled (mock: no cloud resources).")
}

func mustStore() *state.Store {
	st, err := state.NewStore(filepath.Join(envHome(), ".envcore", "state"))
	must(err)
	return st
}

func envHome() string {
	h, _ := os.UserHomeDir()
	return h
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  envcore up   [--provider mock|hetzner] [--id ID] [--node NAME] -- <run cmd>")
	fmt.Fprintln(os.Stderr, "  envcore down [--provider mock|hetzner] [--id ID]")
	fmt.Fprintln(os.Stderr, "  envcore demo | version")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
