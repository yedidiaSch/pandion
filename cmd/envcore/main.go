// Command envcore is the M1 CLI: a single-node provision/run/teardown flow over
// either the in-memory mock provider (default, free, offline) or the real
// Hetzner provider (--provider=hetzner), with security-hardened bootstrap.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/envcore/envcore/internal/config"
	"github.com/envcore/envcore/internal/firewall"
	"github.com/envcore/envcore/internal/harden"
	"github.com/envcore/envcore/internal/orchestrator"
	"github.com/envcore/envcore/internal/overlay"
	"github.com/envcore/envcore/internal/provider"
	"github.com/envcore/envcore/internal/provider/hetzner"
	"github.com/envcore/envcore/internal/provider/mock"
	envssh "github.com/envcore/envcore/internal/ssh"
	"github.com/envcore/envcore/internal/sshkeys"
	"github.com/envcore/envcore/internal/state"
)

// version is set at release time via -ldflags "-X main.version=...". Defaults to
// "dev" for local builds.
var version = "dev"

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
	case "validate":
		runValidate(os.Args[2:])
	case "lockdown":
		runLockdown(os.Args[2:])
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
	noFirewall := fs.Bool("no-firewall", false, "skip the default-deny firewall lockdown")
	noOverlay := fs.Bool("no-overlay", false, "skip the WireGuard management overlay")
	egressAllow := fs.String("egress-allow", "", "comma-separated IPv4/CIDR outbound allowlist")
	file := fs.String("f", "", "cluster.yaml for a multi-node topology")
	_ = fs.Parse(flagArgs)

	p, err := newProvider(*prov)
	must(err)
	o := orchestrator.New(p, mustStore())

	// multi-node path: -f cluster.yaml. M3.2a wires the concurrent provisioning +
	// barrier on the mock provider; the real WG-mesh path lands in M3.2b.
	if *file != "" {
		upCluster(o, p.Name(), *file, *id)
		return
	}

	switch p.Name() {
	case "mock":
		c, err := o.Up(context.Background(), *id, *node, "#cloud-config\n", "")
		must(err)
		fmt.Printf("UP (mock): cluster %q node %q -> %s\n", c.ID, *node, c.Nodes[0].Phase)
		fmt.Println("note: mock provider creates no cloud resources and runs no SSH.")
	case "hetzner":
		upHetzner(o, hetznerUpOpts{
			id: *id, node: *node, runCmd: runCmd,
			toolchain: !*noToolchain, firewall: !*noFirewall, overlay: !*noOverlay,
			egressAllow: splitCSV(*egressAllow),
		})
	}
}

type hetznerUpOpts struct {
	id, node, runCmd string
	toolchain        bool
	firewall         bool
	overlay          bool
	egressAllow      []string
}

// upCluster provisions a multi-node topology from cluster.yaml. M3.2a: mock
// provider only (concurrent provisioning + barrier). The real Hetzner mesh path
// (per-node hardened cloud-init + WG mesh + discovery) lands in M3.2b.
func upCluster(o *orchestrator.Orchestrator, providerName, file, id string) {
	cl, err := config.Load(file)
	must(err)
	if id == "demo" || id == "" {
		id = cl.Name // default the cluster id to the topology name
	}

	if providerName == "hetzner" {
		upClusterHetzner(o, cl, id)
		return
	}

	// mock path: concurrent provisioning + barrier only (no cloud/mesh).
	specs := make([]orchestrator.NodeSpec, len(cl.Nodes))
	for i, n := range cl.Nodes {
		specs[i] = orchestrator.NodeSpec{Name: n.Name, UserData: "#cloud-config\n"}
	}
	fmt.Printf("UP cluster %q: provisioning %d nodes (provider=mock, bounded concurrency)...\n", id, len(specs))
	c, err := o.UpCluster(context.Background(), id, specs, orchestrator.DefaultMaxConcurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster provisioning failed: %v — rolling back...\n", err)
		_ = o.Down(context.Background(), id)
		os.Exit(5)
	}
	for _, n := range c.Nodes {
		fmt.Printf("  %-12s %-8s ip=%s\n", n.Name, n.Phase, n.IP)
	}
	fmt.Printf("barrier passed: all %d nodes RUNNING. teardown: envcore down --id %s\n", len(c.Nodes), id)
	fmt.Println("note: mock provider creates no cloud resources; real mesh + IPC land in M3.2b/M3.3.")
}

// small helpers shared by the cluster flow
func trimSpace(s string) string    { return strings.TrimSpace(s) }
func joinAmp(cmds []string) string { return strings.Join(cmds, " && ") }
func b64(s string) string          { return base64.StdEncoding.EncodeToString([]byte(s)) }

// shellQuote single-quotes s for safe use as one shell argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string { return strconv.Itoa(n) }

func wgPortIf(overlayOn bool) int {
	if overlayOn {
		return overlay.DefaultPort
	}
	return 0
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func upHetzner(o *orchestrator.Orchestrator, opt hetznerUpOpts) {
	id, node, runCmd, toolchain := opt.id, opt.node, opt.runCmd, opt.toolchain
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
	if opt.firewall {
		ci.Packages = append(ci.Packages, "nftables") // needed to apply the lockdown
	}

	// overlay: generate node + operator WG keys; node peers with the operator.
	// node = 10.99.0.1, operator = 10.99.0.2 (single-node; mesh is M3).
	const nodeOverlayIP, opOverlayIP = "10.99.0.1", "10.99.0.2"
	var nodeWG, opWG overlay.Keypair
	if opt.overlay {
		nodeWG, err = overlay.GenerateKeypair()
		must(err)
		opWG, err = overlay.GenerateKeypair()
		must(err)
		ci.Packages = append(ci.Packages, "wireguard")
		ci.WGConfig = overlay.NodeConfig(overlay.NodeSpec{
			PrivKey: nodeWG.Private, Address: nodeOverlayIP + "/24",
			PeerPubKey: opWG.Public, PeerAllowedIP: opOverlayIP + "/32",
		})
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
	fmt.Println("node ready (cloud-init complete).")

	// 6) overlay: the node's wg0 came up at boot. Detect the operator's public IP
	//    (as the node sees our SSH connection — no external lookup needed) to use
	//    as the SSH safety-valve source, then write the operator's local WG config.
	var operatorCIDR string
	if opt.overlay {
		srcIP, err := envssh.Run(ctx, addr, "root", login.Signer, host.Public,
			"echo $SSH_CLIENT | awk '{print $1}'")
		srcIP = strings.TrimSpace(srcIP)
		if err != nil || srcIP == "" {
			fmt.Printf("could not detect operator IP (%v); SSH will stay open to any source\n", err)
		} else {
			operatorCIDR = srcIP + "/32"
		}
		opConf := overlay.OperatorConfig(overlay.OperatorSpec{
			PrivKey: opWG.Private, Address: opOverlayIP + "/32",
			PeerPubKey: nodeWG.Public, Endpoint: ip + ":" + itoa(overlay.DefaultPort),
			PeerAllowedIP: nodeOverlayIP + "/32",
		})
		confPath := filepath.Join(keyDir, "wg-"+id+".conf")
		must(os.WriteFile(confPath, []byte(opConf), 0o600))
		fmt.Printf("overlay up on node (%s). operator config: %s\n", nodeOverlayIP, confPath)
		fmt.Printf("  join the overlay:  sudo wg-quick up %s   (then ssh root@%s)\n", confPath, nodeOverlayIP)
	}

	// 7) close the egress build-window (S2) and apply ingress hardening: the
	//    toolchain was fetched with egress open; now default-deny egress + restrict
	//    ingress to SSH-from-operator + the WG port. Atomic `nft -f` keeps the
	//    established control connection alive.
	if opt.firewall {
		rules := firewall.NFTables(firewall.Spec{
			AllowDNS:          true,
			EgressAllowIPs:    opt.egressAllow,
			SSHFromCIDR:       operatorCIDR,
			WGPort:            wgPortIf(opt.overlay),
			AllowOverlayInput: opt.overlay,
		})
		b64 := base64.StdEncoding.EncodeToString([]byte(rules))
		applyCmd := "echo " + b64 + " | base64 -d | nft -f -"
		if out, err := envssh.Run(ctx, addr, "root", login.Signer, host.Public, applyCmd); err != nil {
			fmt.Printf("firewall apply failed: %v\n%s(node left running)\n", err, out)
			fmt.Printf("node is live. teardown with:  envcore down --provider=hetzner --id %s\n", id)
			return
		}
		sshScope := "any source"
		if operatorCIDR != "" {
			sshScope = operatorCIDR
		}
		fmt.Printf("firewall applied: egress default-deny; ingress SSH from %s + WG overlay.\n", sshScope)
	}

	fmt.Println("running command...")

	// 7) run the user command on the now-ready, locked-down node.
	out, runErr := envssh.Run(ctx, addr, "root", login.Signer, host.Public, runCmd)
	fmt.Printf("---- run output ----\n%s\n--------------------\n", strings.TrimRight(out, "\n"))
	if runErr != nil {
		fmt.Printf("run finished with error: %v (node left running for debugging)\n", runErr)
	}
	fmt.Printf("node is live. teardown with:  envcore down --provider=hetzner --id %s\n", id)
}

func runValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	file := fs.String("f", "cluster.yaml", "path to cluster.yaml")
	_ = fs.Parse(args)
	if _, err := config.Load(*file); err != nil {
		fmt.Fprintf(os.Stderr, "invalid: %v\n", err)
		os.Exit(2) // usage/validation failure per the CLI spec
	}
	fmt.Printf("%s: valid\n", *file)
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
	fmt.Fprintln(os.Stderr, "  envcore validate [-f cluster.yaml]")
	fmt.Fprintln(os.Stderr, "  envcore lockdown --id ID   (public deny-all; SSH over overlay only)")
	fmt.Fprintln(os.Stderr, "  envcore demo | version")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
