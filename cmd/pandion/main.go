// SPDX-License-Identifier: AGPL-3.0-or-later

// Command pandion is the CLI: a provision/run/teardown flow over the in-memory
// mock provider (default, free, offline) or a real cloud backend
// (--provider=hetzner | digitalocean | vultr | linode | scaleway), with
// security-hardened bootstrap.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/config"
	"github.com/yedidiaSch/pandion/internal/firewall"
	"github.com/yedidiaSch/pandion/internal/harden"
	"github.com/yedidiaSch/pandion/internal/lockfile"
	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/overlay"
	"github.com/yedidiaSch/pandion/internal/provider"
	"github.com/yedidiaSch/pandion/internal/provider/digitalocean"
	"github.com/yedidiaSch/pandion/internal/provider/hetzner"
	"github.com/yedidiaSch/pandion/internal/provider/lambda"
	"github.com/yedidiaSch/pandion/internal/provider/linode"
	"github.com/yedidiaSch/pandion/internal/provider/mock"
	"github.com/yedidiaSch/pandion/internal/provider/scaleway"
	"github.com/yedidiaSch/pandion/internal/provider/vultr"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	"github.com/yedidiaSch/pandion/internal/sshkeys"
	"github.com/yedidiaSch/pandion/internal/state"
	"github.com/yedidiaSch/pandion/internal/stream"
	"github.com/yedidiaSch/pandion/internal/userconfig"
)

// version is set at release time via -ldflags "-X main.version=...". Defaults to
// "dev" for local builds.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// resolve the global --profile / $PANDION_PROFILE selector once and strip it from
	// the args before per-command parsing (it applies to every command).
	args := initProfile(os.Args[2:])
	switch os.Args[1] {
	case "init":
		runInit(args)
	case "version":
		fmt.Println("pandion", version)
	case "demo":
		runDemo()
	case "up":
		runUp(args)
	case "build":
		runBuild(args)
	case "down":
		runDown(args)
	case "validate":
		runValidate(args)
	case "lockdown":
		runLockdown(args)
	case "reap":
		runReap(args)
	case "attach":
		runAttach(args)
	case "start":
		runStart(args)
	case "ssh":
		runSSH(args)
	case "cp":
		runCP(args)
	case "code":
		runCode(args)
	case "debug":
		runDebugDispatch(args)
	case "relay":
		runRelayDispatch(args)
	case "ls", "status":
		runLs(args)
	case "profiles":
		runProfiles(args)
	case "list-gpus":
		runListGPUs(args)
	case "completion":
		runCompletion(args)
	case "login":
		runLogin(args)
	case "logout":
		runLogout(args)
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
		token, err := providerToken("hetzner", "HCLOUD_TOKEN")
		if err != nil {
			return nil, err
		}
		return hetzner.New(token), nil
	case "digitalocean", "do":
		token, err := providerToken("digitalocean", "DIGITALOCEAN_TOKEN")
		if err != nil {
			return nil, err
		}
		return digitalocean.New(token), nil
	case "vultr":
		token, err := providerToken("vultr", "VULTR_API_KEY")
		if err != nil {
			return nil, err
		}
		return vultr.New(token), nil
	case "linode", "akamai":
		token, err := providerToken("linode", "LINODE_TOKEN")
		if err != nil {
			return nil, err
		}
		return linode.New(token), nil
	case "scaleway", "scw":
		// Scaleway auth is a triple: the secret key is sensitive (keychain/env), the
		// access key and project id are non-secret identifiers taken from env.
		secretKey, err := providerToken("scaleway", "SCW_SECRET_KEY")
		if err != nil {
			return nil, err
		}
		accessKey := strings.TrimSpace(os.Getenv("SCW_ACCESS_KEY"))
		projectID := strings.TrimSpace(os.Getenv("SCW_DEFAULT_PROJECT_ID"))
		// Preflight: name exactly what's missing. Scaleway needs all three values;
		// they look alike (two UUIDs + one SCW-prefixed key) and are easy to mix up.
		// Skip the check if a Scaleway config file can supply them (WithEnv reads it).
		if !scaleway.ConfigFileExists() {
			var missing []string
			if accessKey == "" {
				missing = append(missing, "SCW_ACCESS_KEY")
			}
			if projectID == "" {
				missing = append(missing, "SCW_DEFAULT_PROJECT_ID")
			}
			if len(missing) > 0 {
				return nil, fmt.Errorf("scaleway: %s not set — Scaleway auth needs three values: "+
					"SCW_ACCESS_KEY (SCW…), SCW_SECRET_KEY (uuid, the only sensitive one), and "+
					"SCW_DEFAULT_PROJECT_ID (uuid). Get the key pair from the console → IAM → API Keys, "+
					"and the project id from the project dashboard", strings.Join(missing, " and "))
			}
		}
		return scaleway.New(secretKey, accessKey, projectID)
	case "lambda", "lambdalabs":
		token, err := providerToken("lambda", "LAMBDA_API_KEY")
		if err != nil {
			return nil, err
		}
		return lambda.New(token), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (use mock|hetzner|digitalocean|vultr|linode|scaleway|lambda)", name)
	}
}

// applyUpDefaults seeds unspecified up knobs (size/region/ttl) from the operator's
// `pandion init` defaults. An explicit flag always wins: size/region are considered
// explicit when non-empty; ttl is explicit when ttlSet (it has a non-empty flag
// default, so emptiness cannot signal "unset"). --no-ttl suppresses the ttl default.
// Returns a non-empty warn string when the config's defaults.ttl is unparseable.
func applyUpDefaults(size, region string, ttl time.Duration, ttlSet, noTTL bool, d userconfig.Defaults) (outSize, outRegion string, outTTL time.Duration, warn string) {
	outSize, outRegion, outTTL = size, region, ttl
	if outSize == "" {
		outSize = d.Size
	}
	if outRegion == "" {
		outRegion = d.Region
	}
	if !ttlSet && !noTTL && d.TTL != "" {
		if dd, err := time.ParseDuration(d.TTL); err == nil {
			outTTL = dd
		} else {
			warn = fmt.Sprintf("warning: ignoring invalid defaults.ttl %q in config: %v", d.TTL, err)
		}
	}
	return
}

func runUp(args []string) {
	flagArgs, runCmd := splitRunCmd(args)
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	prov := fs.String("provider", "", "provider: mock|hetzner|digitalocean|vultr|linode|scaleway|lambda (default: from `pandion init` or your credentials)")
	id := fs.String("id", "demo", "cluster id")
	node := fs.String("node", "node-a", "node name")
	noToolchain := fs.Bool("no-toolchain", false, "skip the built-in C++ toolchain (install only --packages, faster)")
	packages := fs.String("packages", "", "comma-separated apt packages/libraries to install (e.g. libzmq3-dev,libboost-dev); added to the toolchain")
	setup := fs.String("setup", "", "shell command run on the node (as root) in the build window, before your build — for non-apt software (pip/npm/curl; chain with &&)")
	noFirewall := fs.Bool("no-firewall", false, "skip the default-deny firewall lockdown")
	noOverlay := fs.Bool("no-overlay", false, "skip the WireGuard management overlay")
	egressAllow := fs.String("egress-allow", "", "comma-separated IPv4/CIDR outbound allowlist")
	workspacePath := fs.String("workspace", "", "local dir to sync to the node before running")
	remotePath := fs.String("remote-path", "", "where to place the workspace on the node")
	buildCmd := fs.String("build", "", "build command to run on the node after sync (source mode)")
	syncMode := fs.String("sync-mode", "source", "workspace sync: source (build on node) | binaries (upload prebuilt as-is, no .gitignore filtering, no build)")
	runAsUser := fs.String("run-as", harden.DefaultRunUser, "unprivileged user to run the workload as (or 'root')")
	size := fs.String("size", "", "provider server type/size (e.g. cpx21); default: `pandion init` default, else auto-select")
	region := fs.String("region", "", "preferred region/location, comma-separated fallbacks (e.g. nbg1,fsn1); default: config default, else provider's choice")
	gpu := fs.String("gpu", "", "provision a GPU node: MODEL[:COUNT], e.g. a100 or a100:2 (the provider must offer GPUs; see `pandion list-gpus`)")
	gpuIdleUtil := fs.Int("gpu-idle-util", harden.DefaultGPUIdleUtil, "GPU node: utilization %% at/below which the GPU counts as idle for the dead-man's-switch (so headless jobs keep the node alive)")
	ttl := fs.Duration("ttl", harden.DefaultIdleTTL, "idle poweroff after no SSH for this long (security)")
	noTTL := fs.Bool("no-ttl", false, "disable the idle dead-man's-switch")
	maxCost := fs.Float64("max-cost", 0, "budget cap: refuse to provision if projected spend (hourly × TTL) exceeds this (provider currency; 0 = off)")
	dryRun := fs.Bool("dry-run", false, "preview the plan + projected cost and exit; create nothing")
	noRun := fs.Bool("no-run", false, "deploy only: provision + sync + build but do NOT launch the run command (start it later with `pandion start`)")
	lock := fs.String("lock", "", "reproducibility: pin toolchain versions from this lockfile (H2)")
	encWorkspace := fs.Bool("encrypt-workspace", false, "encrypt the workspace at rest with LUKS (ephemeral key; S-E)")
	engine := fs.String("engine", "native", "execution engine: native|docker")
	containerImage := fs.String("container-image", "ubuntu:24.04", "image for --engine=docker")
	capAdd := fs.String("cap-add", "", "comma-separated capabilities to grant the workload (e.g. NET_RAW)")
	file := fs.String("f", "", "cluster.yaml for a multi-node topology")
	_ = fs.Parse(flagArgs)

	// seed unspecified knobs from the operator's `pandion init` defaults (an explicit
	// flag always wins; --ttl needs the fs.Visit check because it has a non-empty default).
	cfg, _ := userconfig.LoadProfile(envHome(), activeProfile)
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var warn string
	*size, *region, *ttl, warn = applyUpDefaults(*size, *region, *ttl, set["ttl"], *noTTL, cfg.Defaults)
	if warn != "" {
		fmt.Fprintln(os.Stderr, warn)
	}

	provName := resolveProvider(*prov)
	if provName == "" {
		fmt.Fprintln(os.Stderr, "no provider set. Run `pandion init`, or pass --provider=<name>.")
		os.Exit(2)
	}
	p, err := newProvider(provName)
	must(err)
	o := orchestrator.New(p, mustStore())
	initAudit()
	audit.Event("up.start", "provider", p.Name(), "id", *id, "cluster_file", *file)

	// --gpu: parse MODEL[:COUNT] and require a GPU-capable provider (G0). The
	// multi-node -f path gets a per-node gpu: key later; reject the combo for now.
	gpuReq, err := parseGPUFlag(*gpu)
	must(err)
	if gpuReq.Wanted() {
		if *file != "" {
			fmt.Fprintln(os.Stderr, "--gpu is not yet supported with -f cluster.yaml (use a single-node `up`)")
			os.Exit(2)
		}
		if _, ok := p.(provider.GPUProvider); !ok {
			fmt.Fprintf(os.Stderr, "provider %q has no GPU offerings — pick a GPU provider (see `pandion list-gpus`)\n", p.Name())
			os.Exit(2)
		}
	}

	// multi-node path: -f cluster.yaml. M3.2a wires the concurrent provisioning +
	// barrier on the mock provider; the real WG-mesh path lands in M3.2b.
	if *file != "" {
		upCluster(o, p.Name(), *file, *id, *maxCost, *dryRun, *lock, *noRun)
		return
	}

	// dry-run works for any pricing provider, incl. mock (offline preview).
	if *dryRun {
		dryRunSingle(o, *id, *node, *size, splitCSV(*region), ttlOrZero(*ttl, *noTTL), gpuReq)
		return
	}

	switch p.Name() {
	case "mock":
		c, err := o.UpSpec(context.Background(), *id,
			orchestrator.NodeSpec{Name: *node, GPU: gpuReq}, "#cloud-config\n", "")
		must(err)
		fmt.Printf("UP (mock): cluster %q node %q -> %s\n", c.ID, *node, c.Nodes[0].Phase)
		fmt.Println("note: mock provider creates no cloud resources and runs no SSH.")
	case "hetzner", "digitalocean", "do", "vultr", "linode", "scaleway":
		// the hardened single-node flow is provider-agnostic (it drives the
		// orchestrator + SSH); the provider is injected via `o`.
		var ws *syncSpec
		if *workspacePath != "" {
			if strings.EqualFold(*syncMode, "binaries") {
				ws = &syncSpec{LocalPath: *workspacePath, RemotePath: *remotePath, Binaries: true}
			} else {
				ws = &syncSpec{LocalPath: *workspacePath, RemotePath: *remotePath, Build: *buildCmd}
			}
		}
		idleTTL := *ttl
		if *noTTL {
			idleTTL = 0
		}
		upHetzner(o, hetznerUpOpts{
			id: *id, node: *node, runCmd: runCmd,
			toolchain: !*noToolchain, packages: splitCSV(*packages), setup: setupCmds(*setup),
			firewall: !*noFirewall, overlay: !*noOverlay,
			egressAllow: splitCSV(*egressAllow), sync: ws, runUser: *runAsUser, idleTTL: idleTTL,
			size: *size, regionPref: splitCSV(*region), gpu: gpuReq, gpuIdleUtil: *gpuIdleUtil,
			engine: *engine, containerImage: *containerImage, caps: capsFor(splitCSV(*capAdd), nil),
			maxCost: *maxCost, lockPath: *lock, encryptWorkspace: *encWorkspace, noRun: *noRun,
		})
	}
}

type hetznerUpOpts struct {
	id, node, runCmd string
	toolchain        bool
	packages         []string // extra apt packages/libraries (--packages), added to the toolchain
	setup            []string // setup commands run (as root) in the build window (--setup)
	firewall         bool
	overlay          bool
	egressAllow      []string
	sync             *syncSpec
	runUser          string
	idleTTL          time.Duration
	size             string          // provider server type (--size); empty = auto-select
	regionPref       []string        // preferred regions (--region); empty = provider's choice
	gpu              provider.GPUReq // optional GPU request (--gpu); zero = CPU-only
	gpuIdleUtil      int             // GPU idle-utilization %% for the dead-man (--gpu-idle-util)
	engine           string          // native | docker
	containerImage   string
	caps             []string
	maxCost          float64 // budget cap (projected spend); 0 = off
	lockPath         string  // reproducibility: pin toolchain from this lockfile (H2)
	encryptWorkspace bool    // LUKS-encrypt the workspace at rest (S-E)
	noRun            bool    // deploy only: don't launch the run command (start later)
}

// upCluster provisions a multi-node topology from cluster.yaml. M3.2a: mock
// provider only (concurrent provisioning + barrier). The real Hetzner mesh path
// (per-node hardened cloud-init + WG mesh + discovery) lands in M3.2b.
func upCluster(o *orchestrator.Orchestrator, providerName, file, id string, maxCost float64, dryRun bool, lockPath string, noRun bool) {
	cl, err := config.Load(file)
	must(err)
	if id == "demo" || id == "" {
		id = cl.Name // default the cluster id to the topology name
	}

	// dry-run works for any pricing provider, incl. mock (offline preview).
	if dryRun {
		dryRunCluster(o, cl, id)
		return
	}

	if providerName != "mock" && providerName != "" {
		// the hardened multi-node mesh flow is provider-agnostic (driven through `o`).
		upClusterHetzner(o, cl, id, maxCost, lockPath, noRun)
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
	fmt.Printf("barrier passed: all %d nodes RUNNING. teardown: pandion down --id %s\n", len(c.Nodes), id)
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

// parseTTL resolves a cluster.yaml ttl string to a duration: "" -> default,
// false/none/off/0 -> disabled (0), otherwise a parsed duration.
func parseTTL(raw string) time.Duration {
	switch s := strings.ToLower(strings.TrimSpace(raw)); s {
	case "":
		return harden.DefaultIdleTTL
	case "false", "none", "off", "0":
		return 0
	default:
		if d, err := time.ParseDuration(raw); err == nil {
			return d
		}
		return harden.DefaultIdleTTL
	}
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

// setupCmds wraps a single-node --setup command as a one-element list (or nil).
// It is NOT comma-split — a setup command may legitimately contain commas; chain
// multiple steps with `&&`. The cluster path takes a real list via `setup:`.
func setupCmds(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return []string{s}
}

func upHetzner(o *orchestrator.Orchestrator, opt hetznerUpOpts) {
	id, node, runCmd, toolchain := opt.id, opt.node, opt.runCmd, opt.toolchain
	prov := o.P.Name() // hetzner | digitalocean — used in teardown hints
	if runCmd == "" {
		runCmd = "echo PANDION_READY && uname -a"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// 1) generate the host key (to inject + pin) and the login key (S1)
	host, err := sshkeys.Generate("pandion-host")
	must(err)
	login, err := sshkeys.Generate("pandion-login")
	must(err)

	// 2) build hardened cloud-init: inject host key (race-free, S1/F1) + install
	//    the C++ toolchain per the Execution Contract (§5)
	ci := harden.CloudInit{
		HostPrivKeyPEM:   host.PrivatePEM,
		HostPubKey:       host.PublicAuthorized,
		LoginPubKey:      login.PublicAuthorized,
		RunUser:          opt.runUser,          // unprivileged workload user (S-C)
		IdleTTL:          opt.idleTTL,          // idle poweroff dead-man's-switch (P2b)
		HasGPU:           opt.gpu.Wanted(),     // GPU util counts as liveness (§4)
		GPUIdleUtil:      opt.gpuIdleUtil,      // GPU idle threshold %
		Fail2ban:         true,                 // SSH brute-force protection (P1)
		AuditLog:         true,                 // on-node audit trail (S-F)
		SysctlHardening:  true,                 // CIS-lite kernel network baseline (P1)
		EncryptWorkspace: opt.encryptWorkspace, // LUKS at rest (S-E; opt-in)
	}
	switch opt.engine {
	case "docker":
		ci.Packages = []string{"docker.io"} // the image provides the toolchain
	default: // native
		// built-in toolchain (unless --no-toolchain) + declared --packages, deduped.
		ci.Packages = harden.ResolveToolchain(opt.packages, !toolchain)
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
	// reproducibility (H2): pin toolchain versions from a prior lockfile if asked,
	// then remember the package list to record what actually resolved.
	if opt.lockPath != "" {
		lk, lerr := lockfile.Load(opt.lockPath)
		must(lerr)
		ci.Packages = lk.PinnedPackages(node, ci.Packages)
		fmt.Printf("pinning packages from lockfile %s\n", opt.lockPath)
	}
	lockPkgs := append([]string(nil), ci.Packages...)
	userData := harden.Build(ci)

	// --max-cost preflight (before spending a cent): the single node auto-discovers
	// its type; project its hourly × TTL and refuse if it exceeds the cap.
	if err := o.CheckBudget(ctx, []orchestrator.NodeSpec{{Name: node, Type: opt.size, RegionPref: opt.regionPref, GPU: opt.gpu}}, []time.Duration{opt.idleTTL}, opt.maxCost); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(6)
	}

	// 3) provision (tagged; journaled state machine). The login key is registered
	//    with the provider so it lands on root (validated path, S1).
	c, err := o.UpSpec(ctx, id, orchestrator.NodeSpec{Name: node, Type: opt.size, RegionPref: opt.regionPref, GPU: opt.gpu}, userData, login.PublicAuthorized)
	must(err)
	ip := c.Nodes[0].IP
	audit.Event("provision", "id", id, "node", node, "provider", prov, "ip", ip, "engine", opt.engine)
	fmt.Printf("UP (hetzner): cluster %q node %q running at %s (host fp %s)\n",
		c.ID, node, ip, host.Fingerprint())

	// cloud-edge firewall (defense-in-depth, M8) — tied to the firewall posture.
	if opt.firewall {
		ensureCloudFirewall(ctx, o, id)
	}

	// 4) persist keys for later attach/down (0600)
	keyDir := filepath.Join(envHome(), ".pandion", "keys", id)
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
		fmt.Printf("node is live. teardown with:  pandion down --provider=%s --id %s\n", prov, id)
		return
	}
	fmt.Println("node ready (cloud-init complete).")

	// reproducibility (H2): record the resolved toolchain versions so a later
	// `up --lock ~/.pandion/lock/<id>.json` reproduces this environment.
	if len(lockPkgs) > 0 {
		nl := queryNodeLock(ctx, addr, login.Signer, host.Public, node, "", lockPkgs)
		if err := writeLock(id, prov, []lockfile.NodeLock{nl}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write lockfile: %v\n", err)
		} else {
			fmt.Printf("wrote reproducibility lockfile: %s\n", lockfile.Path(envHome(), id))
		}
	}

	// 5b) sync workspace + build, in the egress build-window (before the firewall
	//     lockdown) so the build can fetch dependencies (P0-1). native builds on the
	//     host as the run user; docker builds inside the hardened container.
	workdir := ""
	failNode := func(err error) {
		fmt.Fprintf(os.Stderr, "%v (node left running for debugging)\n", err)
		fmt.Printf("node is live. teardown with:  pandion down --provider=%s --id %s\n", prov, id)
	}
	// setup commands (non-apt software) run first — after apt packages, before the
	// build — while egress is open. A failure is fail-fast: report it, leave the node
	// up for debugging, and exit NON-ZERO so scripts/CI notice.
	if err := runSetup(ctx, addr, login.Signer, host.Public, node, opt.setup); err != nil {
		failNode(err)
		os.Exit(1)
	}
	if opt.engine == "docker" {
		// pull the image NOW (egress is still open); the post-lockdown `docker run`
		// then uses the cached image offline.
		fmt.Printf("pulling image %s...\n", opt.containerImage)
		if out, err := envssh.Run(ctx, addr, "root", login.Signer, host.Public,
			"docker pull "+shellQuote(opt.containerImage)); err != nil {
			failNode(fmt.Errorf("docker pull failed: %v\n%s", err, out))
			return
		}
		if opt.sync != nil {
			fmt.Println("workspace sync...")
			wd, err := syncFiles(ctx, addr, login.Signer, host.Public, *opt.sync, "root")
			if err != nil {
				failNode(err)
				return
			}
			workdir = wd
			if b := strings.TrimSpace(opt.sync.Build); b != "" {
				fmt.Printf("  building in container (%s): %s\n", opt.containerImage, b)
				if out, err := envssh.Run(ctx, addr, "root", login.Signer, host.Public,
					dockerRun(opt.containerImage, workdir, b, nil)); err != nil {
					failNode(fmt.Errorf("container build failed: %v\n%s", err, out))
					return
				}
			}
		}
	} else if opt.sync != nil {
		fmt.Println("workspace sync...")
		wd, err := syncWorkspace(ctx, addr, login.Signer, host.Public, *opt.sync, opt.runUser)
		if err != nil {
			failNode(err)
			return
		}
		workdir = wd
	}

	// 5c) verify requested packages actually installed (native): a typo'd/unavailable
	//     apt name is logged by cloud-init but doesn't stop boot, so surface it now —
	//     while egress is still open — rather than as a confusing runtime failure.
	if opt.engine != "docker" {
		warnMissingPackages(ctx, addr, login.Signer, host.Public, node, ci.Packages)
	}

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
			BlockMetadata:     true, // S-F: no workload may read cloud metadata
		})
		b64 := base64.StdEncoding.EncodeToString([]byte(rules))
		applyCmd := "echo " + b64 + " | base64 -d | nft -f -"
		if out, err := envssh.Run(ctx, addr, "root", login.Signer, host.Public, applyCmd); err != nil {
			fmt.Printf("firewall apply failed: %v\n%s(node left running)\n", err, out)
			fmt.Printf("node is live. teardown with:  pandion down --provider=%s --id %s\n", prov, id)
			return
		}
		sshScope := "any source"
		if operatorCIDR != "" {
			sshScope = operatorCIDR
		}
		fmt.Printf("firewall applied: egress default-deny; ingress SSH from %s + WG overlay.\n", sshScope)
	}

	// 8) persist a manifest (node IP + pinned host key) so `attach` can reconnect,
	//    then run the user command: native = as the unprivileged run user (S-C) from
	//    the workspace; docker = in a hardened container (S-D). Both post-lockdown.
	overlayIP := ""
	if opt.overlay {
		overlayIP = nodeOverlayIP
	}
	if err := writeManifest(id, prov, []nodeManifest{{
		Name: node, IP: ip, OverlayIP: overlayIP, HostPub: host.PublicAuthorized,
		Run: runCmd, Engine: opt.engine, ContainerImg: opt.containerImage,
		Workdir: workdir, RunUser: opt.runUser, Caps: opt.caps,
	}}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save manifest (attach won't work): %v\n", err)
	}

	// --no-run: DEPLOY only (provision + sync + build), leave the workload unstarted.
	if opt.noRun {
		audit.Event("up.complete", "id", id, "node", node, "provider", prov, "ip", ip, "no_run", true)
		fmt.Printf("node deployed — nothing started (--no-run).\n")
		fmt.Printf("  start:    pandion start --id %s\n", id)
		fmt.Printf("  teardown: pandion down --provider=%s --id %s\n", prov, id)
		return
	}

	var runShell string
	if opt.engine == "docker" {
		fmt.Printf("running command in hardened container (%s)...\n", opt.containerImage)
		runShell = dockerRun(opt.containerImage, workdir, runCmd, opt.caps)
	} else {
		fmt.Printf("running command (as %s)...\n", orRoot(opt.runUser))
		runShell = runAs(opt.runUser, workdir, runCmd, opt.caps)
	}
	// launch in a DETACHED tmux session teeing to the run log (survives detach, C3),
	// then stream that log. Ctrl+C detaches; the workload keeps running in tmux and
	// is reachable again with `pandion attach --id ID`.
	if err := launchRun(ctx, addr, login.Signer, host.Public, runShell); err != nil {
		failNode(fmt.Errorf("launch failed: %v", err))
		return
	}
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() { <-sigCh; fmt.Println("\n^C — detaching from stream; node left running."); streamCancel() }()

	printer := stream.NewPrinter(os.Stdout, filepath.Join(envHome(), ".pandion", "logs", id), colorEnabled())
	defer printer.Close()
	fmt.Printf("streaming (Ctrl+C detaches; reattach: pandion attach --id %s)\n", id)
	fmt.Println("----------------------------------------------------------------")
	tailLog(streamCtx, addr, login.Signer, host.Public, node, printer)

	audit.Event("up.complete", "id", id, "node", node, "provider", prov, "ip", ip)
	fmt.Printf("node is live. teardown with:  pandion down --provider=%s --id %s\n", prov, id)
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

// runReap finds every Pandion-tagged server at the provider and destroys orphans
// — the no-backend way to prevent billing leaks when local state or the
// controlling laptop is gone (C4). Confirms in a TTY unless --yes.
// runAttach reconnects to a running cluster's multiplexed streams.
func runAttach(args []string) {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	id := fs.String("id", "", "cluster id (required)")
	_ = fs.Parse(args)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "attach: --id is required")
		os.Exit(2)
	}
	if err := attachCluster(*id); err != nil {
		fmt.Fprintf(os.Stderr, "attach: %v\n", err)
		os.Exit(3)
	}
}

func runReap(args []string) {
	fs := flag.NewFlagSet("reap", flag.ExitOnError)
	prov := fs.String("provider", "", "provider (default: from `pandion init` or your credentials)")
	olderThan := fs.Duration("older-than", 0, "only reap clusters whose oldest node is at least this age (e.g. 2h)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	_ = fs.Parse(args)

	p, err := newProvider(resolveProviderOrExit(*prov))
	must(err)
	o := orchestrator.New(p, mustStore())
	initAudit()

	plan, err := o.ReapPlan(context.Background(), *olderThan)
	must(err)
	if len(plan) == 0 {
		fmt.Println("reap: no Pandion clusters to remove.")
		return
	}
	fmt.Printf("reap: %d cluster(s) at %s:\n", len(plan), p.Name())
	total := 0
	for _, c := range plan {
		fmt.Printf("  %-24s %d node(s)  oldest %s\n", c.ClusterID, c.Servers, c.OldestAge.Round(time.Second))
		total += c.Servers
	}
	if !*yes {
		fmt.Printf("destroy all %d node(s)? this is irreversible. [y/N]: ", total)
		var ans string
		_, _ = fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" {
			fmt.Println("aborted; nothing changed.")
			return
		}
	}
	n, err := o.Reap(context.Background(), plan)
	audit.Event("reap", "provider", p.Name(), "clusters", n, "planned", len(plan))
	fmt.Printf("reaped %d/%d cluster(s).\n", n, len(plan))
	if err != nil {
		fmt.Fprintf(os.Stderr, "reap: %v\n", err)
		os.Exit(8)
	}
}

// runLs (aka `status`) lists every Pandion cluster alive at the provider with
// uptime and live cost — the fleet-wide view over the reconcile source of truth
// (works with no local state). Cost is shown when the provider prices its types.
func runLs(args []string) {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	prov := fs.String("provider", "", "provider (default: from `pandion init` or your credentials)")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output (for scripting/automation)")
	_ = fs.Parse(args)

	p, err := newProvider(resolveProviderOrExit(*prov))
	must(err)
	o := orchestrator.New(p, mustStore())

	clusters, currency, err := o.Status(context.Background())
	must(err)
	if *jsonOut {
		must(renderStatusJSON(os.Stdout, clusters, currency))
		return
	}
	if len(clusters) == 0 {
		fmt.Printf("no active Pandion clusters at %s.\n", p.Name())
		return
	}
	renderStatus(os.Stdout, clusters, currency)
}

// renderStatusJSON emits the fleet status as stable JSON (durations in seconds,
// prices as numbers) — for scripting, CI, and automation. Always renders, even
// when empty (`{"clusters":[],...}`), so consumers can rely on the shape.
func renderStatusJSON(w io.Writer, clusters []orchestrator.ClusterStatus, currency string) error {
	type jsonNode struct {
		Name          string  `json:"name"`
		Type          string  `json:"type"`
		GPU           string  `json:"gpu,omitempty"`
		Region        string  `json:"region"`
		IP            string  `json:"ip"`
		UptimeSeconds int64   `json:"uptime_seconds"`
		Hourly        float64 `json:"hourly"`
		Accrued       float64 `json:"accrued"`
	}
	type jsonCluster struct {
		ID      string     `json:"id"`
		Nodes   []jsonNode `json:"nodes"`
		Hourly  float64    `json:"hourly_total"`
		Accrued float64    `json:"accrued_total"`
	}
	out := struct {
		Currency     string        `json:"currency"`
		NodeCount    int           `json:"node_count"`
		TotalHourly  float64       `json:"total_hourly"`
		TotalAccrued float64       `json:"total_accrued"`
		Clusters     []jsonCluster `json:"clusters"`
	}{Currency: currency, Clusters: []jsonCluster{}}
	for _, c := range clusters {
		jc := jsonCluster{ID: c.ClusterID, Nodes: []jsonNode{}, Hourly: c.Hourly, Accrued: c.Accrued}
		for _, n := range c.Nodes {
			gpu := ""
			if n.GPU.Present() {
				gpu = gpuInfoLabel(n.GPU)
			}
			jc.Nodes = append(jc.Nodes, jsonNode{
				Name: n.Name, Type: n.Type, GPU: gpu, Region: n.Region, IP: n.IP,
				UptimeSeconds: int64(n.Age.Seconds()),
				Hourly:        n.Hourly.Amount,
				Accrued:       n.Hourly.Amount * n.Age.Hours(),
			})
		}
		out.NodeCount += len(c.Nodes)
		out.TotalHourly += c.Hourly
		out.TotalAccrued += c.Accrued
		out.Clusters = append(out.Clusters, jc)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// renderStatus writes the `ls`/`status` table + totals. Separated from runLs so
// the output format is unit-testable without a live provider.
func renderStatus(w io.Writer, clusters []orchestrator.ClusterStatus, currency string) {
	if currency == "" {
		currency = "?"
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "CLUSTER\tNODE\tTYPE\tGPU\tREGION\tUPTIME\t%s/hr\t~%s spent\n", currency, currency)
	var totHourly, totAccrued float64
	totNodes := 0
	for _, c := range clusters {
		for i, n := range c.Nodes {
			cid := c.ClusterID + l2Tag(c.ClusterID)
			if i > 0 {
				cid = "" // label the cluster only on its first row
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				cid, n.Name, dashIfEmpty(n.Type), gpuInfoLabel(n.GPU), dashIfEmpty(n.Region),
				shortDur(n.Age), money(n.Hourly), estMoney(n.Hourly, n.Age))
		}
		totHourly += c.Hourly
		totAccrued += c.Accrued
		totNodes += len(c.Nodes)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d cluster(s), %d node(s) — ~%.4f %s/hr, est. %.4f %s spent so far.\n",
		len(clusters), totNodes, totHourly, currency, totAccrued, currency)
	fmt.Fprintln(w, "(cost is an estimate: server age × hourly rate — your provider invoice is authoritative.)")
}

// shortDur renders an uptime compactly: "8m", "3h07m", "2d5h".
func shortDur(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	d = d.Round(time.Minute)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func money(m provider.Money) string {
	if !m.Known() {
		return "—"
	}
	return fmt.Sprintf("%.4f", m.Amount)
}

// ttlOrZero resolves the effective idle-TTL: 0 when --no-ttl.
func ttlOrZero(ttl time.Duration, noTTL bool) time.Duration {
	if noTTL {
		return 0
	}
	return ttl
}

// runListGPUs prints the GPU SKUs a provider can serve, priced — offline for the
// mock provider, so the GPU catalog is browsable without spending a cent (G0).
func runListGPUs(args []string) {
	fs := flag.NewFlagSet("list-gpus", flag.ExitOnError)
	prov := fs.String("provider", "", "provider to query (default: from `pandion init` or your credentials)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)

	provName := resolveProvider(*prov)
	if provName == "" {
		fmt.Fprintln(os.Stderr, "no provider set. Run `pandion init`, or pass --provider=<name>.")
		os.Exit(2)
	}
	p, err := newProvider(provName)
	must(err)
	gp, ok := p.(provider.GPUProvider)
	if !ok {
		fmt.Fprintf(os.Stderr, "provider %q has no GPU offerings.\n", p.Name())
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	offers, err := gp.GPUOfferings(ctx)
	must(err)

	if *asJSON {
		must(json.NewEncoder(os.Stdout).Encode(offers))
		return
	}
	if len(offers) == 0 {
		fmt.Printf("%s: no GPU offerings available.\n", p.Name())
		return
	}
	fmt.Printf("GPU offerings (%s):\n", p.Name())
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tVRAM\tTYPE\tREGIONS\tPRICE/hr")
	for _, o := range offers {
		fmt.Fprintf(tw, "%s\t%dG\t%s\t%s\t%s %s\n",
			o.GPU.Model, o.GPU.VRAM, o.ServerType, strings.Join(o.Regions, ","),
			money(o.Hourly), o.Hourly.Currency)
	}
	tw.Flush()
}

// dryRunSingle previews the single-node `up` (spec-discovered type) and exits.
func dryRunSingle(o *orchestrator.Orchestrator, id, node, size string, regionPref []string, window time.Duration, gpu provider.GPUReq) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, est, err := o.PlanUp(ctx, []orchestrator.NodeSpec{{Name: node, Type: size, RegionPref: regionPref, GPU: gpu}}, []time.Duration{window})
	must(err)
	renderDryRun(os.Stdout, o.P.Name(), id, nodes, est)
}

// parseGPUFlag parses --gpu "MODEL[:COUNT]" into a GPUReq. Empty = no GPU.
// COUNT defaults to 1 and must be a positive integer.
func parseGPUFlag(s string) (provider.GPUReq, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return provider.GPUReq{}, nil
	}
	model, count := s, 1
	if i := strings.IndexByte(s, ':'); i >= 0 {
		model = strings.TrimSpace(s[:i])
		n, err := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if err != nil || n < 1 {
			return provider.GPUReq{}, fmt.Errorf("invalid --gpu count in %q (want MODEL[:COUNT] with COUNT >= 1)", s)
		}
		count = n
	}
	if model == "" {
		return provider.GPUReq{}, fmt.Errorf("invalid --gpu %q (a model is required, e.g. a100 or a100:2)", s)
	}
	return provider.GPUReq{Model: model, Count: count}, nil
}

// renderDryRun writes the `--dry-run` plan + projected cost. Separated so the
// format is unit-testable without a live provider.
func renderDryRun(w io.Writer, providerName, clusterID string, nodes []orchestrator.DryRunNode, est orchestrator.CostEstimate) {
	cur := est.Currency
	if cur == "" {
		cur = "?"
	}
	fmt.Fprintf(w, "DRY RUN — pandion up (provider=%s): nothing will be created.\n", providerName)
	fmt.Fprintf(w, "cluster: %s\n", clusterID)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "NODE\tSIZE\tGPU\tREGION\tTTL\t%s/hr\t~%s (over TTL)\n", cur, cur)
	for _, n := range nodes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Name, autoIf(n.Size), gpuLabel(n.GPU), autoIf(n.Region), ttlLabel(n.Window),
			money(n.Hourly), projMoney(n.Hourly, n.Window))
	}
	tw.Flush()
	proj := fmt.Sprintf("%.4f %s", est.Projected, cur)
	if est.Unbounded {
		proj = "unbounded (a node has no TTL)"
	}
	fmt.Fprintf(w, "\n%d node(s): ~%.4f %s/hr; projected ~%s over TTL.\n", len(nodes), est.Hourly, cur, proj)
	fmt.Fprintln(w, "(estimate — an auto-selected size may vary with live availability; no resources created.)")
}

func autoIf(s string) string {
	if s == "" {
		return "auto"
	}
	return s
}

// gpuLabel renders a GPU request for the plan table: "—" for CPU, "a100" for a
// single GPU, "a100×2" for several.
func gpuLabel(g provider.GPUReq) string {
	if !g.Wanted() {
		return "—"
	}
	model := g.Model
	if model == "" {
		model = "any"
	}
	if g.Count > 1 {
		return fmt.Sprintf("%s×%d", model, g.Count)
	}
	return model
}

// gpuInfoLabel renders a realized GPU (on a running node) for the `ls` table.
func gpuInfoLabel(g provider.GPUInfo) string {
	if !g.Present() {
		return "—"
	}
	if g.Count > 1 {
		return fmt.Sprintf("%s×%d", g.Model, g.Count)
	}
	return g.Model
}

// receipt is the closing cost summary printed on teardown — the zero-backend way
// to shut the "what did that cost me?" loop (docs/gpu-design.md §6.2).
type receipt struct {
	nodes    int
	ran      time.Duration // oldest node's wall-clock lifetime
	ranKnown bool          // false when the provider gives no creation time (e.g. lambda)
	total    float64       // Σ hourly × age (estimate)
	currency string
	priced   bool
	gpus     []string // distinct GPU labels present, e.g. ["a100", "h100×8"]
}

// buildReceipt summarizes a cluster's spend from the servers about to be destroyed
// (call BEFORE teardown, while their creation times are live). Pricing is
// best-effort: an unpriced provider yields priced=false ("cost unknown").
func buildReceipt(ctx context.Context, p provider.Provider, servers []provider.Server) receipt {
	r := receipt{nodes: len(servers)}
	pricer, _ := p.(provider.Pricer)
	now := time.Now()
	seen := map[string]bool{}
	for _, s := range servers {
		if !s.Created.IsZero() {
			age := now.Sub(s.Created)
			if age > r.ran {
				r.ran = age
			}
			r.ranKnown = true
			if pricer != nil {
				if m, err := pricer.HourlyPrice(ctx, s.Type, s.Region); err == nil && m.Known() {
					r.total += m.Amount * age.Hours()
					r.currency = m.Currency
					r.priced = true
				}
			}
		}
		if s.GPU.Present() {
			if lbl := gpuInfoLabel(s.GPU); !seen[lbl] {
				seen[lbl] = true
				r.gpus = append(r.gpus, lbl)
			}
		}
	}
	return r
}

// renderReceipt writes the one-line teardown receipt. Pure, for unit tests.
func renderReceipt(w io.Writer, r receipt) {
	if r.nodes == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  receipt: %d node(s)", r.nodes)
	if len(r.gpus) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(r.gpus, ", "))
	}
	if r.ranKnown {
		fmt.Fprintf(&b, ", ran %s", shortDur(r.ran))
	}
	if r.priced {
		fmt.Fprintf(&b, " · total ~%.2f %s", r.total, r.currency)
	} else {
		b.WriteString(" · cost unknown")
	}
	fmt.Fprintln(w, b.String())
}

func ttlLabel(d time.Duration) string {
	if d <= 0 {
		return "none"
	}
	return shortDur(d)
}

func projMoney(m provider.Money, window time.Duration) string {
	if !m.Known() || window <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.4f", m.Amount*window.Hours())
}

func estMoney(m provider.Money, age time.Duration) string {
	if !m.Known() {
		return "—"
	}
	return fmt.Sprintf("%.4f", m.Amount*age.Hours())
}

func runDown(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	prov := fs.String("provider", "", "provider (default: read from the cluster's manifest)")
	id := fs.String("id", "demo", "cluster id")
	dryRun := fs.Bool("dry-run", false, "list what would be destroyed; destroy nothing")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	_ = fs.Parse(args)

	// prefer the provider recorded in the cluster's manifest, so `down --id X` needs
	// no --provider; fall back to the flag/config resolution.
	provName := *prov
	if provName == "" {
		provName = manifestProvider(*id)
	}
	if provName == "" {
		provName = resolveProvider("")
	}
	if provName == "" {
		fmt.Fprintln(os.Stderr, "no provider for this cluster (no manifest). Pass --provider=<name>.")
		os.Exit(2)
	}
	p, err := newProvider(provName)
	must(err)
	o := orchestrator.New(p, mustStore())
	initAudit()
	ctx := context.Background()

	// preview against the provider (the reconcile source of truth).
	servers, err := p.ListByTag(ctx, *id)
	must(err)
	if len(servers) > 0 {
		fmt.Printf("cluster %q at %s — %d server(s) to destroy:\n", *id, p.Name(), len(servers))
		for _, s := range servers {
			fmt.Printf("  %-26s %-10s %-8s %s\n", s.Name, dashIfEmpty(s.Type), dashIfEmpty(s.Region), s.IP)
		}
	} else {
		fmt.Printf("cluster %q at %s: no live servers (teardown will also reap keys/firewall/state).\n", *id, p.Name())
	}
	if *dryRun {
		fmt.Println("dry-run: nothing destroyed.")
		return
	}
	// confirm only in a terminal (scripts/CI run non-interactively — proceed).
	if !*yes && len(servers) > 0 && isTTY() {
		fmt.Printf("destroy %d server(s)? this is irreversible. [y/N]: ", len(servers))
		var ans string
		_, _ = fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" {
			fmt.Println("aborted; nothing changed.")
			return
		}
	}
	audit.Event("down", "id", *id, "provider", p.Name(), "servers", len(servers))
	// snapshot the cost receipt BEFORE teardown (needs the live creation times).
	rcpt := buildReceipt(ctx, p, servers)
	must(o.Down(ctx, *id))
	reapShares(*id) // revoke + delete any outstanding debug shares (no leak)
	audit.Event("down.complete", "id", *id, "provider", p.Name())
	fmt.Printf("DOWN (%s): cluster %q reconciled to empty.\n", p.Name(), *id)
	renderReceipt(os.Stdout, rcpt)
}

// isTTY reports whether stdin is an interactive terminal.
func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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
	st, err := state.NewStore(filepath.Join(envHome(), ".pandion", "state"))
	must(err)
	return st
}

func envHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// initAudit wires the structured infra-action trail: always appended to
// ~/.pandion/logs/audit.jsonl, and also echoed to stderr when PANDION_LOG is set
// (L3). Called by the commands that touch infrastructure.
func initAudit() {
	dir := filepath.Join(envHome(), ".pandion", "logs")
	_ = os.MkdirAll(dir, 0o700)
	var w io.Writer = io.Discard
	if f, err := os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		w = f // intentionally kept open for the process lifetime (a short-lived CLI)
	}
	level, verbose := audit.LevelFromEnv()
	if verbose {
		w = io.MultiWriter(w, os.Stderr)
	}
	audit.Init(w, level)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  pandion [--profile NAME] <cmd> …   ($PANDION_PROFILE also accepted)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  pandion init   (set up a default provider + credentials so bare commands work)")
	fmt.Fprintln(os.Stderr, "  pandion up   [--provider mock|hetzner|digitalocean] [--id ID] [--node NAME] [--size TYPE] [--region R] [--gpu MODEL[:N]] [--dry-run] [--no-run] [--lock FILE] [--encrypt-workspace] -- <run cmd>")
	fmt.Fprintln(os.Stderr, "  pandion build [dir] [up-flags…] [-- <run cmd>]   (auto-detect toolchain, upload + build the project in the cloud)")
	fmt.Fprintln(os.Stderr, "  pandion down [--provider mock|hetzner|digitalocean] [--id ID] [--dry-run] [--yes]")
	fmt.Fprintln(os.Stderr, "  pandion validate [-f cluster.yaml]")
	fmt.Fprintln(os.Stderr, "  pandion lockdown --id ID   (public deny-all; SSH over overlay only)")
	fmt.Fprintln(os.Stderr, "  pandion reap [--older-than DUR] [--yes]   (destroy orphaned Pandion nodes)")
	fmt.Fprintln(os.Stderr, "  pandion attach --id ID   (reconnect to a running cluster's streams)")
	fmt.Fprintln(os.Stderr, "  pandion start --id ID [--node NAME] [--detach]   (launch run commands on a deployed cluster/node)")
	fmt.Fprintln(os.Stderr, "  pandion ssh --id ID [--node NAME] [--overlay] [-- CMD]   (SSH into a node, host-key pinned)")
	fmt.Fprintln(os.Stderr, "  pandion cp --id ID [--node NAME] SRC DST   (scp to/from a node; prefix a node path with ':')")
	fmt.Fprintln(os.Stderr, "  pandion code --id ID [--node NAME] [--print]   (pinned SSH config for VS Code Remote-SSH)")
	fmt.Fprintln(os.Stderr, "  pandion debug --id ID [--node NAME] [--public] [--pid N] [--print]   (attach your local debugger to a remote process over the overlay)")
	fmt.Fprintln(os.Stderr, "  pandion debug share --id ID [--node NAME] [--expires 2h]   (grant a teammate a scoped, expiring remote-debug token)")
	fmt.Fprintln(os.Stderr, "  pandion debug join <token>   (accept a shared debug grant: scoped overlay peer + launch.json)")
	fmt.Fprintln(os.Stderr, "  pandion debug unshare --id ID [--share SID | --all]   (revoke a shared debug grant)")
	fmt.Fprintln(os.Stderr, "  pandion relay up --id ID [--node NAME] [--port 8443]   (deploy the browser-SSH relay on a node)")
	fmt.Fprintln(os.Stderr, "  pandion ls | status [--provider …] [--json]   (list live clusters + cost)")
	fmt.Fprintln(os.Stderr, "  pandion login | logout [--provider hetzner|digitalocean]   (store/remove the API token in the OS keychain)")
	fmt.Fprintln(os.Stderr, "  pandion list-gpus [--provider …] [--json]   (list the GPU SKUs a provider can serve, priced)")
	fmt.Fprintln(os.Stderr, "  pandion profiles   (list configured profiles; * = active)")
	fmt.Fprintln(os.Stderr, "  pandion completion bash|zsh|fish   (shell completion script)")
	fmt.Fprintln(os.Stderr, "  pandion demo | version")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
