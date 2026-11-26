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
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/config"
	"github.com/yedidiaSch/pandion/internal/firewall"
	"github.com/yedidiaSch/pandion/internal/flock"
	"github.com/yedidiaSch/pandion/internal/gpucache"
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
	"github.com/yedidiaSch/pandion/internal/provider/runpod"
	"github.com/yedidiaSch/pandion/internal/provider/scaleway"
	"github.com/yedidiaSch/pandion/internal/provider/vultr"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	"github.com/yedidiaSch/pandion/internal/sshkeys"
	"github.com/yedidiaSch/pandion/internal/state"
	"github.com/yedidiaSch/pandion/internal/stream"
	"github.com/yedidiaSch/pandion/internal/userconfig"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// version is set at release time via -ldflags "-X main.version=...". Defaults to
// "dev" for local builds.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usageErr()
		os.Exit(2)
	}
	// Strip the global selectors (--profile/--verbose/--quiet) from ANYWHERE ahead of
	// the run command, so they work both before and after the verb (`pandion --quiet
	// up …` and `pandion up --quiet …`). initProfile stops at the first `--`, leaving
	// a run command's own args untouched.
	args := initProfile(os.Args[1:])
	if len(args) == 0 {
		usageErr()
		os.Exit(2)
	}
	verb := args[0]
	rest := args[1:]
	// The universal discovery gestures resolve BEFORE per-command parsing, print to
	// stdout, and exit 0 (P1.1) — `help`/`-h`/`--help` for the command list (or a
	// single command's help), `--version`/`-V` as an alias of the version command.
	switch verb {
	case "help", "-h", "--help":
		if len(rest) > 0 {
			printCommandHelp(strings.TrimLeft(rest[0], "-"))
		} else {
			usage()
		}
		return
	case "--version", "-V":
		fmt.Println("pandion", version)
		return
	}
	if !dispatch(verb, rest) {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", verb)
		usageErr()
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
		// PANDION_MOCK_STATE=<dir> makes the mock durable across processes so
		// offline flows (up → separate ls/up/down) behave like a real provider —
		// used by ci_smoke to regression-test re-up collision etc. (R10). Unset =
		// the in-memory default (fast unit tests; repeated CLI `up` stays free).
		if dir := strings.TrimSpace(os.Getenv("PANDION_MOCK_STATE")); dir != "" {
			return mock.NewPersistent(filepath.Join(dir, "mock-state.json")), nil
		}
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
	case "runpod":
		token, err := providerToken("runpod", "RUNPOD_API_KEY")
		if err != nil {
			return nil, err
		}
		// RUNPOD_CLOUD picks the tier: secure (default) | community. `up`/`list-gpus`
		// set it from --cloud.
		return runpod.New(token, runpod.WithCloud(os.Getenv("RUNPOD_CLOUD")))
	default:
		return nil, fmt.Errorf("unknown provider %q (use mock|hetzner|digitalocean|vultr|linode|scaleway|lambda|runpod)", name)
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
	fs := newCmdFlagSet("up")
	prov := fs.String("provider", "", "provider: mock|hetzner|digitalocean(do)|vultr|linode(akamai)|scaleway(scw)|lambda|runpod (aliases in parens; default: from `pandion init` or your credentials)")
	id := fs.String("id", "demo", "cluster id")
	node := fs.String("node", "node-a", "node name")
	noToolchain := fs.Bool("no-toolchain", false, "skip the built-in C++ toolchain (install only --packages, faster)")
	packages := fs.String("packages", "", "comma-separated apt packages/libraries to install (e.g. libzmq3-dev,libboost-dev); added to the toolchain")
	setup := fs.String("setup", "", "shell command run on the node (as root) in the build window, before your build — for non-apt software (pip/npm/curl; chain with &&)")
	noFirewall := fs.Bool("no-firewall", false, "skip the default-deny firewall lockdown")
	firewallAudit := fs.Bool("firewall-audit", false, "DRY RUN: apply the firewall in audit mode — log what would be dropped but enforce nothing (inspect with `journalctl -k | grep pandion-audit`)")
	noOverlay := fs.Bool("no-overlay", false, "skip the WireGuard management overlay")
	egressAllow := fs.String("egress-allow", "", "comma-separated outbound allowlist: IPv4, CIDR, or hostname (resolved at provision)")
	workspacePath := fs.String("workspace", "", "local dir to sync to the node before running")
	remotePath := fs.String("remote-path", "", "where to place the workspace on the node")
	buildCmd := fs.String("build", "", "build command to run on the node after sync (source mode)")
	syncMode := fs.String("sync-mode", "source", "workspace sync: source (build on node) | binaries (upload prebuilt as-is, no .gitignore filtering, no build)")
	runAsUser := fs.String("run-as", harden.DefaultRunUser, "unprivileged user to run the workload as (or 'root')")
	size := fs.String("size", "", "provider server type/size (e.g. cpx21); default: `pandion init` default, else auto-select")
	region := fs.String("region", "", "region/location; comma-separated to allow fallbacks (e.g. nbg1,fsn1). An explicit --region is honored strictly — it won't silently land in another region on a capacity blip. Default: config default, else provider's choice")
	gpu := fs.String("gpu", "", "provision a GPU node: MODEL[:COUNT], e.g. a100 or a100:2 (the provider must offer GPUs; see `pandion list-gpus`)")
	gpuIdleUtil := fs.Int("gpu-idle-util", harden.DefaultGPUIdleUtil, "GPU node: utilization %% at/below which the GPU counts as idle for the dead-man's-switch (so headless jobs keep the node alive)")
	ttl := fs.Duration("ttl", harden.DefaultIdleTTL, "idle poweroff after no SSH for this long (security)")
	noTTL := fs.Bool("no-ttl", false, "disable the idle dead-man's-switch")
	maxCost := fs.Float64("max-cost", 0, "budget cap: refuse to provision if spend over the idle-TTL window (hourly × TTL) exceeds this (provider currency; 0 = off). Note: the idle TTL powers the node OFF, not down — billing continues until `pandion down`/`reap`")
	dryRun := fs.Bool("dry-run", false, "preview the plan + projected cost and exit; create nothing")
	jsonOut := fs.Bool("json", false, "with --dry-run: emit the plan + projected cost as JSON")
	noRun := fs.Bool("no-run", false, "deploy only: provision + sync + build but do NOT launch the run command (start it later with `pandion start`)")
	lock := fs.String("lock", "", "reproducibility: pin toolchain versions from this lockfile (H2)")
	encWorkspace := fs.Bool("encrypt-workspace", false, "encrypt the workspace at rest with LUKS (ephemeral key; S-E)")
	engine := fs.String("engine", "native", "execution engine: native|docker")
	containerImage := fs.String("container-image", "ubuntu:24.04", "image for --engine=docker")
	capAdd := fs.String("cap-add", "", "comma-separated capabilities to grant the workload (e.g. NET_RAW)")
	provisionTimeout := fs.Duration("provision-timeout", 0, "how long the provider may take to boot the server before up gives up and rolls back (default 10m; 25m with --gpu — Lambda GPU boots take 2–15+ min)")
	setupTimeout := fs.Duration("setup-timeout", 0, "budget for everything after the server boots: cloud-init, sync, --setup, build, firewall (default 30m)")
	image := fs.String("image", "", "base image for the node, where the provider lets you choose one (runpod: the container image; default "+runpod.DefaultImage+")")
	cloudTier := fs.String("cloud", "", "runpod: cloud tier, secure (default; RunPod data centers) or community (cheaper, community-hosted); also RUNPOD_CLOUD")
	fShort, fLong := addFileFlag(fs, "", "cluster.yaml for a multi-node topology")
	_ = fs.Parse(flagArgs)
	file := func() *string { s := resolveFileFlag(fs, fShort, fLong); return &s }()

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
	applyCloudTier(*cloudTier)
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
		if _, ok := p.(provider.GPUProvider); !ok {
			fmt.Fprintf(os.Stderr, "provider %q has no GPU offerings — pick a GPU provider (see `pandion list-gpus`)\n", p.Name())
			os.Exit(2)
		}
	}

	// serialize against a concurrent up/down/reap on the same id (P0.5) for the
	// mutating paths only — a dry-run is read-only and takes no lock.
	if !*dryRun {
		lk := lockClusterOrExit(*id)
		defer lk.Unlock()
		// refuse to provision onto an id that already names a live cluster (F1/R1):
		// with the flock held, this check-then-create can't race. Correct for every
		// provider — an in-memory mock reports nothing across processes (so repeated
		// `up --provider=mock` stays free), while a persistent mock (PANDION_MOCK_STATE)
		// collides just like a cloud provider, which is what makes F1 offline-testable.
		preflightNoExistingCluster(p, *id, p.Name())
	}

	// multi-node path: -f cluster.yaml. A top-level `--gpu` applies as the cluster
	// default (per-node `gpu:` in the topology overrides); GPU nodes are provisioned,
	// hardened, and meshed like any other (M5). An explicitly-passed --size/--region/
	// --engine/--ttl/--no-ttl likewise overrides cluster.yaml's defaults.* (P2.4
	// precedence: flag > env > cluster.yaml > ~/.pandion/config.yaml > built-in
	// default) — only when the flag was actually set on the command line (not merely
	// defaulted from ~/.pandion/config.yaml by applyUpDefaults above), so
	// cluster.yaml still beats an unset flag. A per-node override in the topology
	// (more specific) still wins.
	if *file != "" && provider.IsContainerHost(p) {
		fmt.Fprintf(os.Stderr, "provider %q runs nodes as containers: multi-node clusters (-f, WireGuard mesh) are not supported there — use a single-node `up`\n", p.Name())
		os.Exit(2)
	}
	if *file != "" {
		ov := clusterFlagOverrides{}
		if set["size"] {
			ov.Size = *size
		}
		if set["region"] {
			ov.Region = *region
		}
		if set["engine"] {
			ov.Engine = *engine
		}
		if *noTTL {
			ov.TTL = "false" // parseTTL's disabled sentinel (see parseTTL)
		} else if set["ttl"] {
			ov.TTL = shortDur(*ttl)
		}
		ov.ProvisionTimeout, ov.SetupTimeout = *provisionTimeout, *setupTimeout
		upCluster(o, p.Name(), *file, *id, *maxCost, *dryRun, *lock, *noRun, *gpu, *firewallAudit, *jsonOut, ov)
		return
	}

	// dry-run works for any pricing provider, incl. mock (offline preview).
	if *dryRun {
		dryRunSingle(o, *id, *node, *size, splitCSV(*region), ttlOrZero(*ttl, *noTTL), gpuReq, *jsonOut)
		return
	}

	switch {
	case p.Name() == "mock":
		c, err := o.UpSpec(context.Background(), *id,
			orchestrator.NodeSpec{Name: *node, GPU: gpuReq}, "#cloud-config\n", "")
		must(err)
		fmt.Printf("UP (mock): cluster %q node %q -> %s\n", c.ID, *node, c.Nodes[0].Phase)
		fmt.Println("note: mock provider creates no cloud resources and runs no SSH.")
	case isCloudProvider(p.Name()):
		// the hardened single-node flow is provider-agnostic (it drives the
		// orchestrator + SSH); the provider is injected via `o`. Every provider in
		// cloudProviders takes this path — deriving it from that list (not a
		// hardcoded parallel one) means a newly wired provider can't silently no-op.
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
			firewall: !*noFirewall, firewallAudit: *firewallAudit, overlay: !*noOverlay,
			egressAllow: splitCSV(*egressAllow), sync: ws, runUser: *runAsUser, idleTTL: idleTTL,
			size: *size, regionPref: splitCSV(*region), gpu: gpuReq, gpuIdleUtil: *gpuIdleUtil,
			engine: *engine, containerImage: *containerImage, caps: capsFor(splitCSV(*capAdd), nil),
			maxCost: *maxCost, lockPath: *lock, encryptWorkspace: *encWorkspace, noRun: *noRun,
			provisionTimeout: *provisionTimeout, setupTimeout: *setupTimeout,
			image: *image,
		})
	default:
		// never silently no-op: a provider that resolves + prices but is missing
		// from this dispatch is a wiring bug, not success.
		fmt.Fprintf(os.Stderr, "internal: no `up` path wired for provider %q\n", p.Name())
		os.Exit(2)
	}
}

type hetznerUpOpts struct {
	id, node, runCmd string
	toolchain        bool
	packages         []string // extra apt packages/libraries (--packages), added to the toolchain
	setup            []string // setup commands run (as root) in the build window (--setup)
	firewall         bool
	firewallAudit    bool // dry-run: log what the firewall would drop, enforce nothing
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
	maxCost          float64       // budget cap (projected spend); 0 = off
	lockPath         string        // reproducibility: pin toolchain from this lockfile (H2)
	encryptWorkspace bool          // LUKS-encrypt the workspace at rest (S-E)
	noRun            bool          // deploy only: don't launch the run command (start later)
	provisionTimeout time.Duration // provider boot deadline; 0 = default (CPU/GPU)
	setupTimeout     time.Duration // post-provision budget; 0 = default
	image            string        // --image: node base image (container providers); "" = provider default
}

// clusterFlagOverrides carries the `up` flags that must win over cluster.yaml's
// defaults.* per the documented precedence (P2.4: flag > env > cluster.yaml >
// ~/.pandion/config.yaml > built-in default; see effective.go's resolveKnob doc
// comment). Each field is populated only when the corresponding flag was
// explicitly passed on the command line — a zero value here means "no override",
// letting cluster.yaml (or its own fallback chain) decide as before.
type clusterFlagOverrides struct {
	Size   string // --size
	Region string // --region (note: --region may be comma-separated; only the
	// first preference is applied to cluster.yaml's single provider.region)
	Engine string // --engine
	TTL    string // --ttl (formatted via shortDur) or "false" for --no-ttl
	// ProvisionTimeout / SetupTimeout are the --provision-timeout/--setup-timeout
	// deadlines (0 = default). They are runtime budgets, not cluster.yaml knobs.
	ProvisionTimeout time.Duration
	SetupTimeout     time.Duration
}

// applyClusterFlagOverrides layers explicit `up` CLI flags on top of a loaded
// cluster.yaml's defaults.* (P2.4: flag > env > cluster.yaml > config > default;
// see effective.go's resolveKnob doc comment). Unlike the top-level `--gpu`
// default (which only fills in where cluster.yaml is silent), these
// unconditionally overwrite the cluster.yaml value because ov's fields are only
// populated when the caller (runUp) confirmed the flag was explicitly set — not
// merely defaulted from ~/.pandion/config.yaml by applyUpDefaults. A per-node
// override in the topology (still more specific than a cluster-wide default) is
// left untouched and keeps winning in config.Cluster.Effective.
func applyClusterFlagOverrides(cl *config.Cluster, ov clusterFlagOverrides) {
	if ov.Size != "" {
		cl.Defaults.Size = ov.Size
	}
	if ov.Region != "" {
		cl.Provider.Region = splitCSV(ov.Region)[0]
	}
	if ov.Engine != "" {
		cl.Defaults.Engine = ov.Engine
	}
	if ov.TTL != "" {
		cl.Defaults.TTL = ov.TTL
	}
}

// upCluster provisions a multi-node topology from cluster.yaml. M3.2a: mock
// provider only (concurrent provisioning + barrier). The real Hetzner mesh path
// (per-node hardened cloud-init + WG mesh + discovery) lands in M3.2b.
func upCluster(o *orchestrator.Orchestrator, providerName, file, id string, maxCost float64, dryRun bool, lockPath string, noRun bool, gpuFlag string, auditFW bool, jsonOut bool, ov clusterFlagOverrides) {
	cl, err := config.Load(file)
	must(err)
	warnUnappliedFields(file) // never silently ignore accepted-but-unapplied fields (P2.2)
	if id == "demo" || id == "" {
		id = cl.Name // default the cluster id to the topology name
	}
	// a top-level `--gpu` is the cluster-wide default: apply it where the topology
	// doesn't already set one (per-node `gpu:` and `defaults.gpu:` still win).
	if gpuFlag != "" && cl.Defaults.GPU == "" {
		cl.Defaults.GPU = gpuFlag
	}
	applyClusterFlagOverrides(cl, ov)
	// if any node ends up wanting a GPU, the provider must be GPU-capable.
	if clusterWantsGPU(cl) {
		if _, ok := o.P.(provider.GPUProvider); !ok {
			fmt.Fprintf(os.Stderr, "provider %q has no GPU offerings — a GPU node in %q needs a GPU provider (see `pandion list-gpus`)\n", o.P.Name(), file)
			os.Exit(2)
		}
	}

	// dry-run works for any pricing provider, incl. mock (offline preview).
	if dryRun {
		dryRunCluster(o, cl, id, jsonOut)
		return
	}

	if providerName != "mock" && providerName != "" {
		// the hardened multi-node mesh flow is provider-agnostic (driven through `o`).
		upClusterHetzner(o, cl, id, maxCost, lockPath, noRun, auditFW, ov.ProvisionTimeout, ov.SetupTimeout)
		return
	}

	// mock path: concurrent provisioning + barrier only (no cloud/mesh).
	specs := make([]orchestrator.NodeSpec, len(cl.Nodes))
	for i, n := range cl.Nodes {
		specs[i] = orchestrator.NodeSpec{Name: n.Name, UserData: "#cloud-config\n", GPU: gpuReqOf(cl.Effective(n).GPU)}
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
	fmt.Println("note: mock provider is in-memory only — no cloud resources, no WireGuard mesh, and no remote workload execution. Use a real --provider to run this cluster.")
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
	// Two separate budgets (#11): the provisioning deadline (the provider booting
	// the server — Lambda GPU boots take 2–15+ min) and the post-provision budget
	// (cloud-init, sync, setup, build, lockdown). A shared budget let a slow boot
	// starve setup, and a boot past the deadline orphaned a billing instance.
	provTimeout := provisionTimeoutFor(opt.provisionTimeout, opt.gpu.Wanted())
	setupTimeout := setupTimeoutFor(opt.setupTimeout)

	// Container providers (RunPod) have no cloud-init, systemd or CAP_NET_ADMIN:
	// the node is bootstrapped by a shell script, and the VM-only protections
	// (nftables lockdown, WireGuard overlay, idle poweroff, LUKS) don't apply.
	pod := provider.IsContainerHost(o.P)
	if pod {
		if opt.engine == "docker" {
			fmt.Fprintf(os.Stderr, "--engine docker is not available on %s: the node already IS a container (choose its image with --image)\n", prov)
			os.Exit(2)
		}
		if opt.encryptWorkspace {
			fmt.Fprintf(os.Stderr, "--encrypt-workspace is not available on %s: a container cannot create LUKS volumes\n", prov)
			os.Exit(2)
		}
		if len(opt.egressAllow) > 0 || opt.firewallAudit {
			fmt.Fprintf(os.Stderr, "--egress-allow/--firewall-audit have no effect on %s: a container cannot apply a host firewall\n", prov)
			os.Exit(2)
		}
		opt.firewall, opt.overlay = false, false
	}

	// 1) generate the host key (to inject + pin) and the login key (S1)
	host, err := sshkeys.Generate("pandion-host")
	must(err)
	login, err := sshkeys.Generate("pandion-login")
	must(err)
	keyDir := filepath.Join(pandionDir(), "keys", id)

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
	if pod {
		userData = harden.PodBootstrap(ci)
	}

	// --max-cost preflight (before spending a cent): the single node auto-discovers
	// its type; project its hourly × TTL and refuse if it exceeds the cap.
	budgetCtx, budgetCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := o.CheckBudget(budgetCtx, []orchestrator.NodeSpec{{Name: node, Type: opt.size, RegionPref: opt.regionPref, GPU: opt.gpu}}, []time.Duration{opt.idleTTL}, opt.maxCost); err != nil {
		budgetCancel()
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(6)
	}
	budgetCancel()

	// persist the keys (0600) BEFORE provisioning: if anything fails from here on,
	// the operator can still reach and remove the node (#3).
	must(host.Save(keyDir, "host_ed25519"))
	must(login.Save(keyDir, "login_ed25519"))
	ensureLogsDir(id)

	// the manifest's reconnect view of this node; written NOW (no IP yet) so the
	// cluster's provider is on record even if provisioning dies, then rewritten as
	// soon as the server exists (#3, #4).
	man := nodeManifest{
		Name: node, HostPub: host.PublicAuthorized,
		Run: runCmd, Engine: opt.engine, ContainerImg: opt.containerImage,
		RunUser: opt.runUser, Caps: opt.caps,
	}
	saveMan := func() {
		if err := writeManifest(id, prov, []nodeManifest{man}, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save manifest (ssh/attach won't work): %v\n", err)
		}
	}
	saveMan()

	// 3) provision (tagged; journaled state machine). The login key is registered
	//    with the provider so it lands on root (validated path, S1).
	statusf("provisioning %q on %s (deadline %s)...\n", node, prov, shortDur(provTimeout))
	o.Progress = slowBootPrinter("  ", provTimeout, time.Now)
	provCtx, provCancel := context.WithTimeout(context.Background(), provTimeout)
	stopProvHB := startHeartbeat("provisioning " + node + " at " + prov)
	c, err := o.UpSpec(provCtx, id, orchestrator.NodeSpec{Name: node, Type: opt.size, Image: opt.image, RegionPref: opt.regionPref, GPU: opt.gpu}, userData, login.PublicAuthorized)
	stopProvHB()
	provCancel()
	if err != nil {
		rollbackFailedProvision(o, prov, id, err) // never returns
	}
	ip, sshPort := c.Nodes[0].IP, c.Nodes[0].SSHPort
	man.IP, man.Port = ip, sshPort
	saveMan() // reachable from here on: `pandion ssh/down/ls` work even if setup fails
	addr := provider.SSHAddr(ip, sshPort)
	audit.Event("provision", "id", id, "node", node, "provider", prov, "ip", ip, "engine", opt.engine)
	shown := ip
	if sshPort != 0 && sshPort != 22 {
		shown = addr + " (ssh)"
	}
	fmt.Printf("UP (%s): cluster %q node %q running at %s (host fp %s)\n",
		prov, c.ID, node, shown, host.Fingerprint())
	if pod {
		fmt.Print(containerPostureNote(prov))
	} else {
		// Surface the consequential silent default up front: the node powers ITSELF
		// off after the idle TTL (P2.6) — otherwise a forgotten node just vanishes.
		statusln(idleTTLNotice(opt.idleTTL))
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	// cloud-edge firewall (defense-in-depth, M8) — tied to the firewall posture.
	if opt.firewall {
		ensureCloudFirewall(ctx, o, id)
	}

	// 5) connect with the PINNED host key. RunWithRetry tolerates the cloud-init
	//    window (until our key is installed, pinning rejects and we wait), and
	//    `cloud-init status --wait` blocks until packages + hardening are applied
	//    (S1/F4) — so the toolchain is READY before we run the user command.
	onAttempt := func(n int, reason string) { fmt.Printf("  attempt %d: %s\n", n, reason) }
	if pod {
		if rerr := waitPodReady(ctx, node, addr, login.Signer, host.Public, onAttempt); rerr != nil {
			fmt.Fprintf(os.Stderr, "readiness gate failed: %v (node left running for debugging)\n", rerr)
			stageDeadlineHint("bootstrap", rerr, prov, id)
			liveNodeHints(prov, id)
			os.Exit(codeInfraDegraded)
		}
	} else {
		statusln("connecting (host-key pinned; waiting for cloud-init + toolchain)...")
		stopHB := startHeartbeat("cloud-init readiness on " + node)
		_, rerr := envssh.RunWithRetry(ctx, addr, "root", login.Signer, host.Public,
			"cloud-init status --wait || true", 5*time.Second, onAttempt)
		stopHB()
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "readiness gate failed: %v (node left running for debugging)\n", rerr)
			stageDeadlineHint("cloud-init readiness", rerr, prov, id)
			liveNodeHints(prov, id)
			os.Exit(codeInfraDegraded)
		}
		statusln("node ready (cloud-init complete).")
	}

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

	// 5b) the build window, while egress is still open (before the firewall
	//     lockdown) so every step can fetch dependencies (P0-1): sync the workspace
	//     FIRST, then setup (its script may live in the workspace — #1), then the
	//     docker pull / build (which may need what setup installed).
	//
	// failNode reports a build-window failure, leaves the node up for debugging, and
	// exits NON-ZERO (codeInfraDegraded) so scripts/CI don't read a left-up cluster
	// as success (#2). It never returns — callers need no follow-up `return`.
	failNode := func(err error) {
		fmt.Fprintf(os.Stderr, "%v (node left running for debugging)\n", err)
		stageDeadlineHint("build window", err, prov, id)
		liveNodeHints(prov, id)
		os.Exit(codeInfraDegraded)
	}
	workdir, err := runBuildWindow(ctx, buildWindow{
		node: node, addr: addr, signer: login.Signer, pinned: host.Public,
		setup: opt.setup, sync: opt.sync, runUser: opt.runUser,
		engine: opt.engine, containerImg: opt.containerImage,
	})
	if workdir != "" {
		man.Workdir = workdir
		saveMan()
	}
	if err != nil {
		failNode(err)
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
			EgressAllowIPs:    resolveEgressAllow(opt.egressAllow),
			SSHFromCIDR:       operatorCIDR,
			WGPort:            wgPortIf(opt.overlay),
			AllowOverlayInput: opt.overlay,
			BlockMetadata:     true, // S-F: no workload may read cloud metadata
			AuditOnly:         opt.firewallAudit,
		})
		b64 := base64.StdEncoding.EncodeToString([]byte(rules))
		applyCmd := "echo " + b64 + " | base64 -d | nft -f -"
		if out, err := envssh.Run(ctx, addr, "root", login.Signer, host.Public, applyCmd); err != nil {
			fmt.Fprintf(os.Stderr, "firewall apply failed: %v\n%s(node left running)\n", err, out)
			liveNodeHints(prov, id)
			os.Exit(codeInfraDegraded)
		}
		sshScope := "any source"
		if operatorCIDR != "" {
			sshScope = operatorCIDR
		}
		if opt.firewallAudit {
			fmt.Printf("firewall applied in AUDIT mode: NOTHING enforced; would-be-drops logged.\n")
			fmt.Printf("  inspect:  pandion ssh --id %s -- 'journalctl -k | grep pandion-audit'\n", id)
		} else {
			fmt.Printf("firewall applied: egress default-deny; ingress SSH from %s + WG overlay.\n", sshScope)
			fmt.Print(egressLockdownNote())
			// keep hostname egress-allow rules fresh as CDN IPs rotate (P2.1 follow-up)
			installEgressRefresh(ctx, addr, login.Signer, host.Public, egressAllowHostnames(opt.egressAllow))
		}
	}

	// 8) persist a manifest (node IP + pinned host key) so `attach` can reconnect,
	//    then run the user command: native = as the unprivileged run user (S-C) from
	//    the workspace; docker = in a hardened container (S-D). Both post-lockdown.
	overlayIP := ""
	if opt.overlay {
		overlayIP = nodeOverlayIP
	}
	man.OverlayIP = overlayIP
	man.Workdir = workdir
	saveMan()

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
		failNode(fmt.Errorf("launch failed: %w", err))
	}
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() { <-sigCh; fmt.Println("\n^C — detaching from stream; node left running."); streamCancel() }()

	printer := stream.NewPrinter(os.Stdout, filepath.Join(pandionDir(), "logs", id), colorEnabled())
	defer printer.Close()
	statusf("streaming (Ctrl+C detaches; reattach: pandion attach --id %s)\n", id)
	statusln("----------------------------------------------------------------")
	code := tailLog(streamCtx, addr, login.Signer, host.Public, node, printer)

	audit.Event("up.complete", "id", id, "node", node, "provider", prov, "ip", ip)
	liveNodeHints(prov, id)
	// Propagate a crashed workload's own exit code (mirrors `pandion ssh -- cmd`);
	// a clean exit or a Ctrl+C detach returns 0.
	if code != 0 {
		os.Exit(code)
	}
}

// addFileFlag registers -f and its long alias --file (P1.3), pointing at the same
// concept. Call resolveFileFlag after Parse to get the effective path (an explicit
// --file wins; otherwise -f / its default).
func addFileFlag(fs *flag.FlagSet, def, usage string) (short, long *string) {
	short = fs.String("f", def, usage)
	long = fs.String("file", "", "alias for -f")
	return short, long
}

func resolveFileFlag(fs *flag.FlagSet, short, long *string) string {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["file"] {
		return *long
	}
	return *short
}

func runValidate(args []string) {
	fs := newCmdFlagSet("validate")
	fShort, fLong := addFileFlag(fs, "cluster.yaml", "path to cluster.yaml")
	showEff := fs.Bool("show-effective", false, "also print the effective value + source of each resolved knob (P2.4)")
	_ = fs.Parse(args)
	file := resolveFileFlag(fs, fShort, fLong)
	if _, err := config.Load(file); err != nil {
		fmt.Fprintf(os.Stderr, "invalid: %v\n", err)
		os.Exit(2) // usage/validation failure per the CLI spec
	}
	fmt.Printf("%s: valid\n", file)
	warnUnappliedFields(file)
	if *showEff {
		fmt.Println()
		showEffective(os.Stdout, file)
	}
}

// warnUnappliedFields prints a warning for each schema field that validates but has
// no backend consumer yet (P2.2), so a "valid" config that sets one isn't silently
// ignored. Best-effort: an unreadable file simply warns nothing.
func warnUnappliedFields(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, w := range config.Warnings(data) {
		fmt.Fprintf(os.Stderr, "warning: %s: %s\n", path, w)
	}
}

// runReap finds every Pandion-tagged server at the provider and destroys orphans
// — the no-backend way to prevent billing leaks when local state or the
// controlling laptop is gone (C4). Confirms in a TTY unless --yes.
// runAttach reconnects to a running cluster's multiplexed streams.
func runAttach(args []string) {
	fs := newCmdFlagSet("attach")
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
	fs := newCmdFlagSet("reap")
	prov := fs.String("provider", "", "provider (default: from `pandion init` or your credentials)")
	olderThan := fs.Duration("older-than", 0, "only reap clusters whose oldest node is at least this age (e.g. 2h)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	jsonOut := fs.Bool("json", false, "emit a machine-readable sweep result (implies non-interactive; pass --yes)")
	_ = fs.Parse(args)

	p, err := newProvider(resolveProviderOrExit(*prov))
	must(err)
	o := orchestrator.New(p, mustStore())
	initAudit()

	plan, skipped, err := o.ReapPlan(context.Background(), *olderThan)
	must(err)
	if len(skipped) > 0 && !*jsonOut {
		// never treat an unknown age as "old": that would reap a node launched a
		// minute ago from another machine. Say what was left alone, and how to
		// remove it deliberately.
		fmt.Fprintf(os.Stderr, "reap: skipped %d cluster(s) with an UNKNOWN age (%s reports no creation time and there is no local record): %s\n",
			len(skipped), p.Name(), strings.Join(skipped, ", "))
		fmt.Fprintf(os.Stderr, "  remove deliberately with `pandion down --provider=%s --id ID`, or `pandion reap` without --older-than.\n", p.Name())
	}
	if len(plan) == 0 {
		// nothing to destroy, but stale local journals for this provider (clusters
		// already gone) can still be GC'd (R7c).
		gc := gcOrphanJournals(p)
		if *jsonOut {
			must(renderReapJSON(os.Stdout, p.Name(), plan, 0))
		} else if gc > 0 {
			fmt.Printf("reap: no clusters to remove; cleared %d orphaned local state record(s).\n", gc)
		} else {
			fmt.Println("reap: no Pandion clusters to remove.")
		}
		return
	}
	total := 0
	if !*jsonOut {
		fmt.Printf("reap: %d cluster(s) at %s:\n", len(plan), p.Name())
	}
	for _, c := range plan {
		if !*jsonOut {
			age := "age unknown"
			if c.AgeKnown {
				age = "oldest " + c.OldestAge.Round(time.Second).String()
			}
			fmt.Printf("  %-24s %d node(s)  %s\n", c.ClusterID, c.Servers, age)
		}
		total += c.Servers
	}
	if *jsonOut && !*yes {
		fmt.Fprintln(os.Stderr, "reap --json is non-interactive; pass --yes to confirm.")
		os.Exit(2)
	}
	if !*jsonOut && !*yes {
		// reap can destroy EVERY Pandion cluster at this provider, so — unlike
		// `down` — it refuses to proceed non-interactively without an explicit
		// --yes rather than reading (and misinterpreting) empty piped stdin.
		if !isTTY() {
			fmt.Fprintln(os.Stderr, "reap destroys every matching Pandion cluster at this provider; non-interactive: pass --yes to confirm.")
			os.Exit(2)
		}
		if !confirmYes(fmt.Sprintf("destroy all %d node(s)? this is irreversible. [y/N]: ", total)) {
			return
		}
	}
	// serialize against a concurrent up/down/start on any planned id (P0.5): take
	// every per-cluster lock up front; if one is held, refuse the whole reap rather
	// than racing a live operation on that cluster.
	for _, c := range plan {
		lk := lockClusterOrExit(c.ClusterID)
		defer lk.Unlock()
	}
	n, err := o.Reap(context.Background(), plan)
	audit.Event("reap", "provider", p.Name(), "clusters", n, "planned", len(plan))
	// tombstone the local manifests we know about so reconnect commands fast-fail
	// (no-op for reaped clusters this laptop never created).
	for _, c := range plan {
		tombstoneManifest(c.ClusterID)
	}
	// GC orphaned journals for THIS provider: local state whose cluster no longer
	// exists at the provider (R7c). reap already holds provider truth, so it is the
	// natural place. Journals for other providers are left alone.
	if gc := gcOrphanJournals(p); gc > 0 && !*jsonOut {
		fmt.Printf("cleared %d orphaned local state record(s) (no servers at %s).\n", gc, p.Name())
	}
	if *jsonOut {
		must(renderReapJSON(os.Stdout, p.Name(), plan, n))
	} else {
		fmt.Printf("reaped %d/%d cluster(s).\n", n, len(plan))
	}
	if err != nil {
		if !*jsonOut {
			fmt.Fprintf(os.Stderr, "reap: %v\n", err)
		}
		os.Exit(8)
	}
}

// renderReapJSON emits a stable sweep result (P1.5): the planned clusters (id +
// server count + oldest-age) and how many were reaped. Always emits the envelope,
// even for an empty plan.
func renderReapJSON(w io.Writer, providerName string, plan []orchestrator.ReapCandidate, reaped int) error {
	type jsonCluster struct {
		ID              string `json:"id"`
		Servers         int    `json:"servers"`
		OldestAgeSecond int64  `json:"oldest_age_seconds"` // 0 when age_known is false
		AgeKnown        bool   `json:"age_known"`
	}
	jc := make([]jsonCluster, 0, len(plan))
	for _, c := range plan {
		jc = append(jc, jsonCluster{ID: c.ClusterID, Servers: c.Servers, OldestAgeSecond: int64(c.OldestAge.Seconds()), AgeKnown: c.AgeKnown})
	}
	out := struct {
		Provider string        `json:"provider"`
		Planned  int           `json:"planned"`
		Reaped   int           `json:"reaped"`
		Clusters []jsonCluster `json:"clusters"`
	}{Provider: providerName, Planned: len(plan), Reaped: reaped, Clusters: jc}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// runLs (aka `status`) lists every Pandion cluster alive at the provider with
// uptime and live cost — the fleet-wide view over the reconcile source of truth
// (works with no local state, so orphans from failed runs show up too). Cost is
// shown when the provider prices its types.
//
// With no --provider it queries EVERY provider that has credentials: a GPU node
// on Lambda must not be invisible just because the default provider is Hetzner
// (#4). Servers whose teardown is still in progress are listed with their state
// rather than hidden (#12).
func runLs(args []string) {
	fs := newCmdFlagSet("ls")
	prov := fs.String("provider", "", "provider to query (default: every provider you have credentials for)")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output (for scripting/automation)")
	gpuUtil := fs.Bool("gpu-util", false, "also show live GPU utilization %% per node (SSHes nvidia-smi; needs public SSH reachability)")
	_ = fs.Parse(args)

	var names []string
	if *prov != "" {
		names = []string{resolveProviderOrExit(*prov)}
	} else if names = credProviders(); len(names) == 0 {
		names = []string{resolveProviderOrExit("")}
	}

	results := make([]providerStatus, 0, len(names))
	failed := false
	for _, name := range names {
		ps := providerStatus{Provider: name}
		p, err := newProvider(name)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			ps.Clusters, ps.Currency, err = orchestrator.New(p, mustStore()).Status(ctx)
			cancel()
		}
		if err != nil {
			if len(names) == 1 {
				must(err)
			}
			// one provider's outage must not hide the others — but the listing is
			// incomplete, so say so and exit non-zero at the end.
			fmt.Fprintf(os.Stderr, "ls: could not query %s: %s\n", name, friendlyErr(err))
			failed = true
			continue
		}
		if *gpuUtil {
			fetchGPUUtils(ps.Clusters) // best-effort: SSH each GPU node for live utilization
		}
		results = append(results, ps)
	}

	if *jsonOut {
		must(renderStatusJSONMulti(os.Stdout, results, len(names) > 1))
	} else {
		renderLs(os.Stdout, results, *gpuUtil)
	}
	if failed {
		os.Exit(codeError)
	}
}

// providerStatus is one provider's slice of the fleet view.
type providerStatus struct {
	Provider string
	Clusters []orchestrator.ClusterStatus
	Currency string
}

// renderLs writes the human fleet view: one table per provider that has
// clusters (each provider bills in its own currency), and a single line naming
// the providers that have none.
func renderLs(w io.Writer, results []providerStatus, showGPUUtil bool) {
	var empty []string
	shown := 0
	for _, r := range results {
		if len(r.Clusters) == 0 {
			empty = append(empty, r.Provider)
			continue
		}
		if len(results) > 1 {
			if shown > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "== %s ==\n", r.Provider)
		}
		renderStatus(w, r.Clusters, r.Currency, showGPUUtil)
		shown++
	}
	if len(empty) > 0 {
		if shown > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "no active Pandion clusters at %s.\n", strings.Join(empty, ", "))
	}
}

// fetchGPUUtils fills NodeStatus.GPUUtil for GPU nodes by SSHing nvidia-smi
// (public IP, host-key pinned). Best-effort: an unreachable node stays -1.
func fetchGPUUtils(clusters []orchestrator.ClusterStatus) {
	for ci := range clusters {
		c := &clusters[ci]
		anyGPU := false
		for _, n := range c.Nodes {
			if n.GPU.Present() {
				anyGPU = true
			}
		}
		if !anyGPU {
			continue
		}
		man, err := loadManifest(c.ClusterID)
		if err != nil {
			continue
		}
		signer, err := loadLoginSigner(c.ClusterID)
		if err != nil {
			continue
		}
		// Match manifest → status node by IP, not name: providers differ in what
		// Server.Name is (Lambda encodes the full "pandion-<cluster>--<node>" name,
		// while the manifest keeps the short node name), so a name key silently
		// misses and the util read never runs. IP is unambiguous and in both.
		hostByIP := map[string]string{}
		for _, mn := range man.Nodes {
			hostByIP[mn.IP] = mn.HostPub
		}
		for ni := range c.Nodes {
			n := &c.Nodes[ni]
			if !n.GPU.Present() || n.IP == "" {
				continue
			}
			if hp, ok := hostByIP[n.IP]; ok {
				n.GPUUtil = queryGPUUtil(n.IP, hp, signer)
			}
		}
	}
}

// queryGPUUtil SSHes a node (host-key pinned) and returns the busiest GPU's
// utilization %, or -1 if unreachable/unparseable. Best-effort, but retried: the
// read races GPU/CUDA warmup and a fresh SSH can be slow to establish, so one
// transient miss shouldn't blank the column. Two attempts, 12s each, 1s apart.
func queryGPUUtil(ip, hostPub string, signer gossh.Signer) int {
	pinned, err := parsePinned(hostPub)
	if err != nil {
		return -1
	}
	const attempts = 2
	for i := range attempts {
		if i > 0 {
			time.Sleep(time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		out, err := envssh.Run(ctx, ip+":22", "root", signer, pinned,
			"nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits")
		cancel()
		if err != nil {
			continue
		}
		best := -1
		for _, f := range strings.Fields(out) {
			if v, err := strconv.Atoi(f); err == nil && v > best {
				best = v
			}
		}
		return best
	}
	return -1
}

// renderStatusJSON emits the fleet status as stable JSON (durations in seconds,
// prices as numbers) — for scripting, CI, and automation. Always renders, even
// when empty (`{"clusters":[],...}`), so consumers can rely on the shape.
func renderStatusJSON(w io.Writer, clusters []orchestrator.ClusterStatus, currency string) error {
	return renderStatusJSONMulti(w, []providerStatus{{Clusters: clusters, Currency: currency}}, false)
}

// renderStatusJSONMulti is renderStatusJSON over one or more providers. The
// envelope is the same either way; every cluster carries its provider and
// currency, and every node its state. When providers bill in different
// currencies the top-level currency is "" and the totals are omitted (0) with
// "mixed_currency": true — summing EUR and USD would be a lie.
func renderStatusJSONMulti(w io.Writer, results []providerStatus, multi bool) error {
	type jsonNode struct {
		Name          string  `json:"name"`
		Type          string  `json:"type"`
		GPU           string  `json:"gpu,omitempty"`
		GPUUtil       *int    `json:"gpu_util,omitempty"`
		Region        string  `json:"region"`
		IP            string  `json:"ip"`
		State         string  `json:"state"` // "running", or the provider's state, e.g. "terminating"
		UptimeSeconds int64   `json:"uptime_seconds"`
		UptimeKnown   bool    `json:"uptime_known"`
		Hourly        float64 `json:"hourly"`
		Accrued       float64 `json:"accrued"`
	}
	type jsonCluster struct {
		ID       string     `json:"id"`
		Provider string     `json:"provider,omitempty"`
		Currency string     `json:"currency,omitempty"`
		Nodes    []jsonNode `json:"nodes"`
		Hourly   float64    `json:"hourly_total"`
		Accrued  float64    `json:"accrued_total"`
	}
	out := struct {
		Currency      string        `json:"currency"`
		MixedCurrency bool          `json:"mixed_currency,omitempty"`
		Providers     []string      `json:"providers,omitempty"`
		NodeCount     int           `json:"node_count"`
		TotalHourly   float64       `json:"total_hourly"`
		TotalAccrued  float64       `json:"total_accrued"`
		Clusters      []jsonCluster `json:"clusters"`
	}{Clusters: []jsonCluster{}}
	for _, r := range results {
		if multi {
			out.Providers = append(out.Providers, r.Provider)
		}
		if r.Currency != "" {
			if out.Currency == "" && !out.MixedCurrency {
				out.Currency = r.Currency
			} else if out.Currency != r.Currency {
				out.MixedCurrency, out.Currency = true, ""
			}
		}
		for _, c := range r.Clusters {
			jc := jsonCluster{ID: c.ClusterID, Provider: r.Provider, Currency: r.Currency, Nodes: []jsonNode{}, Hourly: c.Hourly, Accrued: c.Accrued}
			for _, n := range c.Nodes {
				gpu := ""
				if n.GPU.Present() {
					gpu = gpuInfoLabel(n.GPU)
				}
				state := n.Status
				if state == "" {
					state = "running"
				}
				jn := jsonNode{
					Name: n.Name, Type: n.Type, GPU: gpu, Region: n.Region, IP: n.IP, State: state,
					UptimeSeconds: int64(n.Age.Seconds()),
					UptimeKnown:   ageKnown(n),
					Hourly:        n.Hourly.Amount,
					Accrued:       n.Hourly.Amount * n.Age.Hours(),
				}
				if n.GPUUtil >= 0 { // measured via --gpu-util
					u := n.GPUUtil
					jn.GPUUtil = &u
				}
				jc.Nodes = append(jc.Nodes, jn)
			}
			out.NodeCount += len(c.Nodes)
			out.TotalHourly += c.Hourly
			out.TotalAccrued += c.Accrued
			out.Clusters = append(out.Clusters, jc)
		}
	}
	if out.MixedCurrency {
		out.TotalHourly, out.TotalAccrued = 0, 0
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ageKnown reports whether a node's uptime is known. A positive Age implies it
// (callers that build NodeStatus by hand set only Age).
func ageKnown(n orchestrator.NodeStatus) bool { return n.AgeKnown || n.Age > 0 }

// renderStatus writes the `ls`/`status` table + totals. Separated from runLs so
// the output format is unit-testable without a live provider.
func renderStatus(w io.Writer, clusters []orchestrator.ClusterStatus, currency string, showGPUUtil bool) {
	// An unpriced provider publishes no rates: show "unpriced" in the money headers
	// (not a bare "?", which reads as broken) and an explanatory footer (P4.4).
	priced := currency != ""
	curLabel := currency
	if !priced {
		curLabel = "unpriced"
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	utilCol := ""
	if showGPUUtil {
		utilCol = "\tUTIL%"
	}
	fmt.Fprintf(tw, "CLUSTER\tNODE\tTYPE\tGPU%s\tREGION\tUPTIME\t%s/hr\t~%s spent\n", utilCol, curLabel, curLabel)
	var totHourly, totAccrued float64
	totNodes := 0
	for _, c := range clusters {
		for i, n := range c.Nodes {
			cid := c.ClusterID + l2Tag(c.ClusterID)
			if i > 0 {
				cid = "" // label the cluster only on its first row
			}
			util := ""
			if showGPUUtil {
				util = "\t" + gpuUtilLabel(n.GPUUtil)
			}
			name := n.Name
			if n.Status != "" {
				name += " [" + n.Status + "]" // e.g. terminating: teardown accepted, still exists (and may bill)
			}
			uptime, spent := "?", "?"
			if ageKnown(n) {
				uptime, spent = shortDur(n.Age), estMoney(n.Hourly, n.Age)
			} else if !n.Hourly.Known() {
				spent = "—"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s%s\t%s\t%s\t%s\t%s\n",
				cid, name, dashIfEmpty(n.Type), gpuInfoLabel(n.GPU), util, dashIfEmpty(n.Region),
				uptime, money(n.Hourly), spent)
		}
		totHourly += c.Hourly
		totAccrued += c.Accrued
		totNodes += len(c.Nodes)
	}
	tw.Flush()
	unknownAge := false
	for _, c := range clusters {
		for _, n := range c.Nodes {
			if !ageKnown(n) {
				unknownAge = true
			}
		}
	}
	if unknownAge {
		fmt.Fprintln(w, "(? = launch time unknown: the provider reports none and this machine has no record of the launch; spend excludes these.)")
	}
	if priced {
		fmt.Fprintf(w, "\n%d cluster(s), %d node(s) — ~%s %s/hr, est. %s %s spent so far.\n",
			len(clusters), totNodes, fmtAmount(totHourly), currency, fmtAmount(totAccrued), currency)
		fmt.Fprintln(w, "(cost is an estimate: server age × hourly rate — your provider invoice is authoritative.)")
	} else {
		fmt.Fprintf(w, "\n%d cluster(s), %d node(s).\n", len(clusters), totNodes)
		fmt.Fprintln(w, "(unpriced: this provider publishes no rates through Pandion — see your provider invoice for cost.)")
	}
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

// fmtAmount is the ONE money-formatting rule used everywhere (ls/receipt/dry-run):
// 2 decimals at or above 0.01, 4 below (so sub-cent hourly rates stay legible).
func fmtAmount(v float64) string {
	if v >= 0.01 || v <= -0.01 || v == 0 {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.4f", v)
}

func money(m provider.Money) string {
	if !m.Known() {
		return "—"
	}
	return fmtAmount(m.Amount)
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
	fs := newCmdFlagSet("list-gpus")
	prov := fs.String("provider", "", "provider to query (default: from `pandion init` or your credentials)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	refresh := fs.Bool("refresh", false, "bypass the local cache and re-fetch the live catalog")
	cloudTier := fs.String("cloud", "", "runpod: cloud tier to list, secure (default) or community; also RUNPOD_CLOUD")
	_ = fs.Parse(args)
	applyCloudTier(*cloudTier)

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
	// Serve from the on-disk cache (fast + offline) unless --refresh or stale (M6-R3).
	// `up` always resolves live at create, so a stale cache can't launch the wrong SKU.
	const gpuCacheTTL = 6 * time.Hour
	var offers []provider.GPUOffering
	// RunPod availability moves minute to minute (and differs per cloud tier), so
	// its list is always live.
	if !*refresh && p.Name() != "runpod" {
		offers, _ = gpucache.Load(envHome(), p.Name(), gpuCacheTTL)
	}
	if offers == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		offers, err = gp.GPUOfferings(ctx)
		must(err)
		_ = gpucache.Save(envHome(), p.Name(), offers)
	}

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
func dryRunSingle(o *orchestrator.Orchestrator, id, node, size string, regionPref []string, window time.Duration, gpu provider.GPUReq, jsonOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, est, err := o.PlanUp(ctx, []orchestrator.NodeSpec{{Name: node, Type: size, RegionPref: regionPref, GPU: gpu}}, []time.Duration{window})
	must(err)
	if jsonOut {
		must(renderDryRunJSON(os.Stdout, o.P.Name(), id, nodes, est))
		return
	}
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
	priced := est.Currency != ""
	cur := est.Currency
	if !priced {
		cur = "unpriced"
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
	if !priced {
		fmt.Fprintf(w, "\n%d node(s) — unpriced: %s publishes no rates through Pandion.\n", len(nodes), providerName)
		return
	}
	proj := fmt.Sprintf("%s %s", fmtAmount(est.Projected), cur)
	if est.Unbounded {
		proj = "unbounded (a node has no TTL)"
	}
	fmt.Fprintf(w, "\n%d node(s): ~%s %s/hr; projected ~%s over TTL.\n", len(nodes), fmtAmount(est.Hourly), cur, proj)
	fmt.Fprintln(w, "(estimate — an auto-selected size may vary with live availability; no resources created.)")
}

// renderDryRunJSON emits the `up --dry-run` plan as a stable JSON envelope (P1.5):
// per-node size/region/gpu/ttl/hourly/projected, plus cluster totals. Auto-chosen
// values are flagged so a script can tell "cpx21 (auto)" from an explicit size.
func renderDryRunJSON(w io.Writer, providerName, clusterID string, nodes []orchestrator.DryRunNode, est orchestrator.CostEstimate) error {
	type jsonNode struct {
		Name         string  `json:"name"`
		Size         string  `json:"size"`
		SizeAuto     bool    `json:"size_auto"`
		GPU          string  `json:"gpu,omitempty"`
		Region       string  `json:"region"`
		RegionAuto   bool    `json:"region_auto"`
		TTLSeconds   int64   `json:"ttl_seconds"`
		Hourly       float64 `json:"hourly"`
		ProjectedTTL float64 `json:"projected_over_ttl"`
	}
	jn := make([]jsonNode, 0, len(nodes))
	for _, n := range nodes {
		gpu := ""
		if n.GPU.Wanted() {
			gpu = gpuLabel(n.GPU)
		}
		jn = append(jn, jsonNode{
			Name: n.Name, Size: autoIf(n.Size), SizeAuto: n.Size == "",
			GPU: gpu, Region: autoIf(n.Region), RegionAuto: n.Region == "",
			TTLSeconds: int64(n.Window.Seconds()),
			Hourly:     n.Hourly.Amount, ProjectedTTL: n.Hourly.Amount * n.Window.Hours(),
		})
	}
	out := struct {
		Provider       string     `json:"provider"`
		ID             string     `json:"id"`
		DryRun         bool       `json:"dry_run"`
		Currency       string     `json:"currency"`
		Nodes          []jsonNode `json:"nodes"`
		TotalHourly    float64    `json:"total_hourly"`
		TotalProjected float64    `json:"total_projected"`
		Unbounded      bool       `json:"unbounded"`
	}{
		Provider: providerName, ID: clusterID, DryRun: true, Currency: est.Currency,
		Nodes: jn, TotalHourly: est.Hourly, TotalProjected: est.Projected, Unbounded: est.Unbounded,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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

// gpuUtilLabel renders a live GPU utilization reading: "—" when not measured
// (-1) or the node is CPU/unreachable, else "NN%".
func gpuUtilLabel(util int) string {
	if util < 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", util)
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
	ranKnown bool          // false when neither the provider nor the journal knows a launch time
	total    float64       // Σ hourly × age (estimate)
	currency string
	priced   bool
	gpus     []string // distinct GPU labels present, e.g. ["a100", "h100×8"]
}

// buildReceipt summarizes a cluster's spend up to now from the servers about to be
// destroyed (see snapshotReceipt/at for teardown, which must price the END). Pricing is
// best-effort: an unpriced provider yields priced=false ("cost unknown").
// launchedAt (normally Orchestrator.LaunchedAt) gives each server's best-known
// launch time — the provider's creation time or the launch Pandion journaled for
// this cluster — so providers that report no creation timestamp (Lambda) still
// get a real cost instead of "cost unknown" (M6-R1), measured from launch (where
// billing starts) rather than from "ready" (#9). nil = provider time only.
func buildReceipt(ctx context.Context, p provider.Provider, servers []provider.Server, launchedAt func(provider.Server) time.Time) receipt {
	return snapshotReceipt(ctx, p, servers, launchedAt).at(time.Now())
}

// receiptBasis is what a receipt needs that teardown destroys: each server's
// launch time and hourly price. Snapshot it BEFORE `down`, then price it with
// .at(when the provider confirmed the servers gone) — billing runs until the
// instance disappears, not until `down` starts, and a Lambda terminate can sit in
// "terminating" for 20+ minutes (#9/#12).
type receiptBasis struct {
	nodes int
	items []receiptItem
	gpus  []string
}

type receiptItem struct {
	launched time.Time // zero = unknown
	hourly   provider.Money
}

func snapshotReceipt(ctx context.Context, p provider.Provider, servers []provider.Server, launchedAt func(provider.Server) time.Time) receiptBasis {
	b := receiptBasis{nodes: len(servers)}
	pricer, _ := p.(provider.Pricer)
	seen := map[string]bool{}
	for _, s := range servers {
		it := receiptItem{launched: s.Created}
		if launchedAt != nil {
			it.launched = launchedAt(s)
		}
		if !it.launched.IsZero() && pricer != nil {
			if m, err := pricer.HourlyPrice(ctx, s.Type, s.Region); err == nil && m.Known() {
				it.hourly = m
			}
		}
		b.items = append(b.items, it)
		if s.GPU.Present() {
			if lbl := gpuInfoLabel(s.GPU); !seen[lbl] {
				seen[lbl] = true
				b.gpus = append(b.gpus, lbl)
			}
		}
	}
	return b
}

// at prices the basis with every server billing until end.
func (b receiptBasis) at(end time.Time) receipt {
	r := receipt{nodes: b.nodes, gpus: b.gpus}
	for _, it := range b.items {
		if it.launched.IsZero() {
			continue
		}
		age := end.Sub(it.launched)
		if age < 0 {
			age = 0
		}
		if age > r.ran {
			r.ran = age
		}
		r.ranKnown = true
		if it.hourly.Known() {
			r.total += it.hourly.Amount * age.Hours()
			r.currency = it.hourly.Currency
			r.priced = true
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
		fmt.Fprintf(&b, " · total ~%s %s", fmtAmount(r.total), r.currency)
	} else {
		b.WriteString(" · cost unknown")
	}
	fmt.Fprintln(w, b.String())
}

// renderReceiptJSON emits a stable teardown receipt envelope (P1.5). dryRun=true
// marks a preview (nothing destroyed). The schema always carries every field so
// scripts can parse it unconditionally, mirroring renderStatusJSON.
func renderReceiptJSON(w io.Writer, r receipt, id, providerName string, servers []provider.Server, dryRun bool) error {
	type jsonServer struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Region string `json:"region"`
		IP     string `json:"ip"`
	}
	js := make([]jsonServer, 0, len(servers))
	for _, s := range servers {
		js = append(js, jsonServer{Name: s.Name, Type: s.Type, Region: s.Region, IP: s.IP})
	}
	out := struct {
		ID         string       `json:"id"`
		Provider   string       `json:"provider"`
		DryRun     bool         `json:"dry_run"`
		Destroyed  int          `json:"destroyed"`
		Servers    []jsonServer `json:"servers"`
		RanSeconds int64        `json:"ran_seconds"`
		RanKnown   bool         `json:"ran_known"`
		Priced     bool         `json:"priced"`
		TotalCost  float64      `json:"total_cost"`
		Currency   string       `json:"currency"`
		GPUs       []string     `json:"gpus"`
	}{
		ID: id, Provider: providerName, DryRun: dryRun,
		Destroyed: len(servers), Servers: js,
		RanSeconds: int64(r.ran.Seconds()), RanKnown: r.ranKnown,
		Priced: r.priced, TotalCost: r.total, Currency: r.currency,
		GPUs: r.gpus,
	}
	if out.GPUs == nil {
		out.GPUs = []string{}
	}
	if dryRun {
		out.Destroyed = 0
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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
	return fmtAmount(m.Amount * window.Hours())
}

func estMoney(m provider.Money, age time.Duration) string {
	if !m.Known() {
		return "—"
	}
	return fmtAmount(m.Amount * age.Hours())
}

func runDown(args []string) {
	fs := newCmdFlagSet("down")
	prov := fs.String("provider", "", "provider (default: the one recorded for this cluster in ~/.pandion — manifest, state journal, or lockfile)")
	id := fs.String("id", "", "cluster id (required, except the mock demo)")
	dryRun := fs.Bool("dry-run", false, "list what would be destroyed; destroy nothing")
	yes := fs.Bool("yes", false, "skip the confirmation prompt (for scripts; a non-terminal stdin never prompts)")
	fs.BoolVar(yes, "y", false, "alias for --yes")
	wait := fs.Duration("wait", orchestrator.DownWait, "how long to wait for the provider to finish terminating before reporting servers as still terminating")
	jsonOut := fs.Bool("json", false, "emit a machine-readable teardown receipt (implies non-interactive; pass --yes)")
	_ = fs.Parse(args)
	orchestrator.DownWait = *wait

	// Resolve a missing --id safely — a bare `pandion down` must never guess at a
	// real cluster. Under the mock provider it still targets the "demo" id (keeps
	// `pandion demo` one-liners working); with a real provider it requires an
	// explicit id, or — as an ergonomic escape hatch — auto-selects the sole local
	// cluster, forcing a named confirmation prompt so the choice is visible.
	autoPicked := false
	if *id == "" {
		tentative := *prov
		if tentative == "" {
			tentative = resolveProvider("")
		}
		if tentative == "mock" {
			*id = "demo"
		} else {
			ids := localClusterIDs()
			switch len(ids) {
			case 1:
				*id = ids[0]
				autoPicked = true
			case 0:
				fmt.Fprintln(os.Stderr, "down: --id is required (no local clusters found). Pass --id <name>.")
				os.Exit(2)
			default:
				fmt.Fprintf(os.Stderr, "down: --id is required; local clusters: %s\n", strings.Join(ids, ", "))
				os.Exit(2)
			}
		}
	}

	// The cluster's OWN provider, from local records, wins over the defaults: a
	// GPU cluster on Lambda must never be "torn down" at the default provider
	// (Hetzner) while it keeps billing (#4).
	provName, provSource := *prov, "flag"
	if provName == "" {
		provName, provSource = localClusterProvider(*id)
	}
	if provName == "" {
		provName, provSource = resolveProvider(""), "default"
	}
	if provName == "" {
		fmt.Fprintln(os.Stderr, "no provider for this cluster (no local record). Pass --provider=<name>.")
		os.Exit(2)
	}
	p, err := newProvider(provName)
	must(err)
	ctx := context.Background()

	// preview against the provider (the reconcile source of truth).
	servers, err := p.ListByTag(ctx, *id)
	must(err)

	// Nothing there, and the provider was only a GUESS (no flag, no local record):
	// the cluster may live at another provider. Look before claiming anything —
	// "reconciled to empty" + clearing local state here is how a billing GPU node
	// became invisible (#4).
	if len(servers) == 0 && provSource == "default" {
		found := findClusterElsewhere(ctx, *id, provName)
		switch len(found) {
		case 0:
			fmt.Fprintf(os.Stderr, "down: no servers for %q at %s", *id, provName)
			if others := otherCredProviders(provName); len(others) > 0 {
				fmt.Fprintf(os.Stderr, " (also checked: %s)", strings.Join(others, ", "))
			}
			fmt.Fprintf(os.Stderr, ", and no local record of this cluster — nothing torn down, nothing cleared.\n")
			fmt.Fprintf(os.Stderr, "  if it lives at a provider without stored credentials, pass --provider=<name>.\n")
			os.Exit(codeNotFound)
		case 1:
			for name, ss := range found {
				fmt.Fprintf(os.Stderr, "down: %q is not at the default provider %s — found it at %s; using %s.\n", *id, provName, name, name)
				provName, servers = name, ss
			}
			p, err = newProvider(provName)
			must(err)
		default:
			names := make([]string, 0, len(found))
			for name := range found {
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Fprintf(os.Stderr, "down: %q exists at several providers (%s) — pass --provider=<name>.\n", *id, strings.Join(names, ", "))
			os.Exit(codeUsage)
		}
	}

	o := orchestrator.New(p, mustStore())
	initAudit()
	// --json is a machine consumer, so it stays silent here and emits one object at the end.
	if !*jsonOut {
		if len(servers) > 0 {
			fmt.Printf("cluster %q at %s — %d server(s) to destroy:\n", *id, p.Name(), len(servers))
			for _, s := range servers {
				st := ""
				if s.Status != "" {
					st = "  [" + s.Status + "]"
				}
				fmt.Printf("  %-26s %-10s %-8s %s%s\n", s.Name, dashIfEmpty(s.Type), dashIfEmpty(s.Region), s.IP, st)
			}
		} else {
			fmt.Printf("cluster %q at %s: no live servers.\n", *id, p.Name())
		}
	}
	if *dryRun {
		if *jsonOut {
			must(renderReceiptJSON(os.Stdout, receipt{}, *id, p.Name(), servers, true))
		} else {
			fmt.Println("dry-run: nothing destroyed.")
		}
		return
	}
	// serialize against a concurrent up/reap on this id (P0.5).
	lk := lockClusterOrExit(*id)
	defer lk.Unlock()
	// confirm only in a terminal (scripts/CI run non-interactively — proceed).
	// --json is non-interactive by contract: require --yes rather than prompt.
	if *jsonOut && !*yes && len(servers) > 0 {
		fmt.Fprintln(os.Stderr, "down --json is non-interactive; pass --yes to confirm.")
		os.Exit(2)
	}
	// An auto-picked id (no --id given) always demands confirmation so the chosen
	// cluster is named out loud; non-interactively that requires an explicit --yes.
	if !*jsonOut && !*yes && len(servers) > 0 {
		if autoPicked && !isTTY() {
			fmt.Fprintf(os.Stderr, "down: no --id given and stdin is not a TTY; pass --id %s and/or --yes.\n", *id)
			os.Exit(2)
		}
		prompt := fmt.Sprintf("destroy %d server(s)? this is irreversible. [y/N]: ", len(servers))
		if autoPicked {
			prompt = fmt.Sprintf("no --id given; destroy the only cluster %q (%d server(s))? [y/N]: ", *id, len(servers))
		}
		if (autoPicked || isTTY()) && !confirmYes(prompt) {
			// declining is not success: nothing was destroyed and the servers still bill.
			os.Exit(codeRefused)
		}
	}
	audit.Event("down", "id", *id, "provider", p.Name(), "servers", len(servers))
	// snapshot the cost receipt BEFORE teardown, from the best-known launch time of
	// each server: the provider's creation time or the launch time journaled for
	// THIS incarnation of the cluster — never the lockfile, which a failed `up`
	// leaves pointing at a previous cluster with the same id (#9).
	basis := snapshotReceipt(ctx, p, servers, o.LaunchedAt)
	if err := o.Down(ctx, *id); err != nil {
		if orchestrator.IsStillTerminating(err) {
			// accepted but not finished: do NOT claim it's gone or tombstone it (#12).
			fmt.Fprintf(os.Stderr, "down: %v\n", err)
			fmt.Fprintf(os.Stderr, "  the provider may still bill for these until they disappear; check with: pandion ls --provider %s\n", p.Name())
			audit.Event("down.pending", "id", *id, "provider", p.Name())
			os.Exit(codeReapFailure)
		}
		fmt.Fprintf(os.Stderr, "down: %s\n", friendlyErr(err))
		os.Exit(codeReapFailure)
	}
	// priced up to NOW — Down returns only once the provider no longer lists the
	// servers, so this includes the time spent waiting out "terminating".
	rcpt := basis.at(time.Now())
	reapShares(*id)        // revoke + delete any outstanding debug shares (no leak)
	tombstoneManifest(*id) // mark the manifest destroyed so attach/ssh/start/lockdown fast-fail
	audit.Event("down.complete", "id", *id, "provider", p.Name())
	if *jsonOut {
		must(renderReceiptJSON(os.Stdout, rcpt, *id, p.Name(), servers, false))
		return
	}
	if len(servers) == 0 {
		fmt.Printf("DOWN (%s): nothing was running for %q; local state cleared.\n", p.Name(), *id)
	} else {
		fmt.Printf("DOWN (%s): cluster %q destroyed — the provider no longer lists any of its servers.\n", p.Name(), *id)
	}
	if kept := keptPaths(*id); len(kept) > 0 {
		fmt.Printf("  kept for post-mortem: %s (reconnect commands now report it torn down).\n", strings.Join(kept, ", "))
	}
	renderReceipt(os.Stdout, rcpt)
}

// localClusterProvider returns the provider that local records say owns cluster
// id, and which record said so. Checked in order of authority: the live
// manifest, the state journal (written before the first server is created, so it
// exists even when provisioning failed), a tombstoned manifest, then the
// lockfile. ("", "") when nothing local knows the cluster.
func localClusterProvider(id string) (prov, source string) {
	if m, err := loadManifest(id); err == nil && m.Provider != "" {
		return m.Provider, "manifest"
	}
	if st, err := state.NewStore(filepath.Join(pandionDir(), "state")); err == nil {
		if c, err := st.Load(id); err == nil && c.Provider != "" {
			return c.Provider, "journal"
		}
	}
	if b, err := os.ReadFile(manifestPath(id)); err == nil {
		var m clusterManifest
		if json.Unmarshal(b, &m) == nil && m.Provider != "" {
			return m.Provider, "manifest"
		}
	}
	if lk, err := lockfile.Load(lockfile.Path(envHome(), id)); err == nil && lk.Provider != "" {
		return lk.Provider, "lockfile"
	}
	return "", ""
}

// otherCredProviders lists the credentialed cloud providers other than skip.
func otherCredProviders(skip string) []string {
	var out []string
	for _, c := range credProviders() {
		if c != skip {
			out = append(out, c)
		}
	}
	return out
}

// findClusterElsewhere asks every OTHER credentialed provider for servers of
// cluster id. Errors are reported and skipped (a dead token must not hide a
// cluster that is found elsewhere). Returns provider → servers, for hits only.
func findClusterElsewhere(ctx context.Context, id, skip string) map[string][]provider.Server {
	found := map[string][]provider.Server{}
	for _, name := range otherCredProviders(skip) {
		p, err := newProvider(name)
		if err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ss, err := p.ListByTag(cctx, id)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "down: could not check %s: %v\n", name, err)
			continue
		}
		if len(ss) > 0 {
			found[name] = ss
		}
	}
	return found
}

// keptPaths lists the local post-mortem directories that actually exist for id,
// so `down` never points at a directory that isn't there (#7).
func keptPaths(id string) []string {
	var out []string
	for _, sub := range []string{"keys", "logs"} {
		if fi, err := os.Stat(filepath.Join(pandionDir(), sub, id)); err == nil && fi.IsDir() {
			out = append(out, filepath.Join("~/.pandion", sub, id))
		}
	}
	return out
}

// isTTY reports whether stdin is an interactive terminal.
// It asks the terminal driver rather than checking for a character device:
// /dev/null IS a character device, so the old check treated `cmd </dev/null`
// (how agents, cron and many CI runners start processes) as interactive and then
// "aborted" on the empty read (#5).
func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// idleTTLNotice renders the idle-poweroff notice for the start of `up` (P2.6). An
// idle node powers ITSELF off after this window (a safety default that otherwise
// bites silently); 0 means the dead-man's-switch is disabled.
func idleTTLNotice(ttl time.Duration) string {
	if ttl <= 0 {
		return "idle poweroff: disabled (--no-ttl) — this node runs until you tear it down."
	}
	return fmt.Sprintf("idle poweroff: node powers off after %s with no SSH (--ttl to change, --no-ttl to disable). Note: power-off is not teardown — it still bills until `pandion down`/`reap`.", shortDur(ttl))
}

// stderrIsTTY reports whether stderr is an interactive terminal — used to gate
// human-only hints (e.g. the completion install one-liner) so redirected/piped
// output stays clean.
func stderrIsTTY() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// lockClusterOrExit takes the per-cluster cross-process advisory lock so two
// mutating commands (up/down/start/lockdown/reap) can't interleave writes to the
// same cluster's state. On contention it prints who holds it and exits
// codeRefused (nothing changed). A non-contention lock error (e.g. an unwritable
// state dir) is a warning, not a hard stop — the command proceeds unlocked, since
// the provider stays the source of truth. Returns nil in that case (Unlock is safe
// on nil). Callers `defer l.Unlock()`.
func lockClusterOrExit(id string) *flock.Lock {
	dir := filepath.Join(pandionDir(), "state")
	_ = os.MkdirAll(dir, 0o755)
	l, err := flock.TryLock(filepath.Join(dir, id+".lock"))
	if err != nil {
		if flock.IsBusy(err) {
			fmt.Fprintf(os.Stderr, "%q: %v\n", id, err)
			os.Exit(codeRefused)
		}
		fmt.Fprintf(os.Stderr, "warning: could not acquire state lock for %q: %v (proceeding)\n", id, err)
		return nil
	}
	return l
}

// upPreflight decides whether `up` may create a cluster with this id on p. It is
// the read-only core of the idempotency guard (F1/R1): `up` has no existence check
// today, so a re-run duplicates servers (providers that allow duplicate names),
// orphans the previous set (its keys/manifest get overwritten), or — on Hetzner —
// fails the name collision after the journal was already rewritten and rolls back
// the healthy original. This returns a user-facing refusal message when a live
// cluster with that id already exists, or "" to proceed. Split out (no exit, no
// I/O beyond the provider call) so it is unit-testable with a seeded mock.
//
// localProvider is the provider recorded in this id's local manifest ("" if none):
// a live manifest naming a DIFFERENT provider is a collision even when the target
// provider has nothing, because the old cluster may still be running and billing,
// invisible to a ListByTag on the new provider (F5). A ListByTag error is treated
// as "proceed" — the per-id flock still prevents concurrent self-collision, and
// the provider stays the source of truth for teardown.
func upPreflight(ctx context.Context, p provider.Provider, id, provName, localProvider string) string {
	if localProvider != "" && localProvider != provName {
		return fmt.Sprintf("cluster %q already exists locally as a %s cluster — its servers may still be running.\n  tear it down first:  pandion down --id %s\n  or choose another --id.", id, localProvider, id)
	}
	servers, err := p.ListByTag(ctx, id)
	if err != nil {
		return ""
	}
	// a server whose teardown is already in progress doesn't block a re-up: it is
	// on its way out (and a later `down` still targets it if it lingers).
	live := 0
	for _, s := range servers {
		if !s.Terminating() {
			live++
		}
	}
	if live > 0 {
		return fmt.Sprintf("cluster %q already exists at %s (%d server(s) running) — `up` would duplicate or orphan it.\n  tear it down first:  pandion down --provider=%s --id %s\n  or choose another --id.", id, provName, live, provName, id)
	}
	return ""
}

// preflightNoExistingCluster runs upPreflight against the live manifest + provider
// and exits codeUsage with the refusal message if the id is already taken. Called
// under the per-id flock (so check-then-provision is race-free), for real providers
// only — mock creates nothing and its default `demo` id is meant to be reusable.
func preflightNoExistingCluster(p provider.Provider, id, provName string) {
	localProv := ""
	if m, err := loadManifest(id); err == nil {
		localProv = m.Provider // a tombstoned (torn-down) manifest yields an error → "" → not a collision
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if msg := upPreflight(ctx, p, id, provName, localProv); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(codeUsage)
	}
}

// gcOrphanJournals removes local state journals for p's provider whose cluster no
// longer exists at the provider (R7c) — reap is the natural caller since it holds
// provider truth. It only touches this provider's journals, skips any id whose
// per-id flock is held (a concurrent up/down owns it), and tombstones the manifest
// so reconnect commands still fast-fail. Returns the number cleared.
func gcOrphanJournals(p provider.Provider) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	servers, err := p.ListAllTagged(ctx)
	cancel()
	if err != nil {
		return 0 // couldn't establish truth — never GC on uncertainty
	}
	live := map[string]bool{}
	for _, s := range servers {
		live[s.ClusterID] = true
	}
	st := mustStore()
	dir := filepath.Join(pandionDir(), "state")
	_ = os.MkdirAll(dir, 0o755)
	n := 0
	for _, id := range localClusterIDs() {
		c, err := st.Load(id)
		if err != nil || c.Provider != p.Name() || live[id] {
			continue
		}
		// don't race a concurrent op on this id: if the flock is held, skip it.
		lk, lerr := flock.TryLock(filepath.Join(dir, id+".lock"))
		if lerr != nil {
			continue
		}
		if st.Close(id) == nil {
			tombstoneManifest(id)
			n++
		}
		lk.Unlock()
	}
	return n
}

// localClusterIDs returns the cluster ids that still have a local state journal
// under ~/.pandion/state. It's the local view of "which clusters exist" and is
// used to resolve a missing --id when exactly one is known (down auto-pick).
func localClusterIDs() []string {
	entries, err := os.ReadDir(filepath.Join(pandionDir(), "state"))
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return ids
}

// confirmYes prints prompt and reads a line from stdin, returning true only on an
// explicit y/Y. A closed/empty stdin (EOF — e.g. piped with nothing to read) is
// called out distinctly so the abort isn't mistaken for a silent no-op; any other
// answer is a normal decline.
func confirmYes(prompt string) bool {
	fmt.Print(prompt)
	var ans string
	if _, err := fmt.Scanln(&ans); err != nil {
		if errors.Is(err, io.EOF) {
			fmt.Println("no confirmation received — aborted; nothing changed (pass --yes to skip the prompt).")
		} else {
			fmt.Println("aborted; nothing changed.")
		}
		return false
	}
	if ans != "y" && ans != "Y" {
		fmt.Println("aborted; nothing changed.")
		return false
	}
	return true
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
	st, err := state.NewStore(filepath.Join(pandionDir(), "state"))
	must(err)
	return st
}

func envHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// pandionDir is the base directory for all Pandion state (state journal, keys,
// logs, config, lockfiles). It is $PANDION_HOME when set — which REPLACES
// ~/.pandion outright (P2.5), handy for tests, shared machines, and XDG layouts —
// otherwise ~/.pandion. Every path under Pandion's control hangs off this.
func pandionDir() string {
	if h := strings.TrimSpace(os.Getenv("PANDION_HOME")); h != "" {
		return h
	}
	return filepath.Join(envHome(), ".pandion")
}

// initAudit wires the structured infra-action trail: always appended to
// ~/.pandion/logs/audit.jsonl, and also echoed to stderr when PANDION_LOG is set
// (L3). Called by the commands that touch infrastructure.
func initAudit() {
	dir := filepath.Join(pandionDir(), "logs")
	_ = os.MkdirAll(dir, 0o700)
	var w io.Writer = io.Discard
	if f, err := os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		w = f // intentionally kept open for the process lifetime (a short-lived CLI)
	}
	level, verbose := audit.LevelFromEnv()
	// --verbose (global flag) wins over the env: force the debug trail to stderr.
	if logVerbose {
		level, verbose = slog.LevelDebug, true
	}
	if verbose {
		w = io.MultiWriter(w, os.Stderr)
	}
	audit.Init(w, level)
}

// startHeartbeat prints an elapsed-time line to stderr every ~30s while a slow
// stage runs, so a user can tell "hung" from "slow" during the multi-minute waits
// in `up` (P4.1). It is silent when stderr isn't a TTY or --quiet is set, and never
// touches stdout (machine output stays clean). The returned func stops it (safe to
// call once; defer it).
func startHeartbeat(stage string) func() {
	if logQuiet || !stderrIsTTY() {
		return func() {}
	}
	stop := make(chan struct{})
	start := time.Now()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				fmt.Fprintf(os.Stderr, "  … still working on %s (%s elapsed)\n", stage, shortDur(time.Since(start)))
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

// deadlineHintText returns a per-stage explanation when err is a context deadline
// (which stage blew the budget, that the cluster is left up, how to retry or tear
// down), or "" for any other error. Split out so it's unit-testable (P4.1).
func deadlineHintText(stage string, err error, prov, id string) string {
	if !errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	return fmt.Sprintf("provisioning exceeded its time budget during %q — the cluster is left up.\n"+
		"  retry the workload:  pandion start --id %s\n"+
		"  or tear it down:     pandion down --provider=%s --id %s", stage, id, prov, id)
}

// stageDeadlineHint prints deadlineHintText to stderr (nothing for a non-deadline
// error), so a blown budget names the stage instead of a bare "context deadline
// exceeded" (P4.1).
func stageDeadlineHint(stage string, err error, prov, id string) {
	if s := deadlineHintText(stage, err, prov, id); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
}

// statusln/statusf print human progress ("chatter") to stdout unless --quiet is
// set (P1.6). Final results and error messages do NOT go through these — they must
// always show — so --quiet leaves exactly results + stderr errors.
func statusln(a ...any) {
	if !logQuiet {
		fmt.Println(a...)
	}
}

func statusf(format string, a ...any) {
	if !logQuiet {
		fmt.Printf(format, a...)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", friendlyErr(err))
		os.Exit(1)
	}
}

// friendlyErr classifies common provider SDK/HTTP errors into an actionable hint,
// then appends the original text (P4.2) — it never HIDES the underlying error. An
// unrecognized error is returned verbatim.
func friendlyErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	low := strings.ToLower(s)
	var hint string
	switch {
	case containsAny(low, "401", "unauthorized", "403", "forbidden", "invalid token", "authentication failed"):
		hint = "provider rejected the credentials — the token is invalid or lacks write scope. Re-run `pandion login` (or check the provider's env var)."
	case containsAny(low, "429", "rate limit", "too many requests"):
		hint = "provider rate limit hit — retry shortly."
	case containsAny(low, "quota", "capacity", "no available", "resource_exhausted", "out of stock", "insufficient"):
		hint = "provider quota/capacity limit — try another --size or --region, or request a quota increase."
	default:
		return s // unrecognized: surface verbatim
	}
	return hint + "\n  underlying error: " + s
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// applyCloudTier exports --cloud as RUNPOD_CLOUD for the provider constructor
// (an explicit flag wins over the environment).
func applyCloudTier(tier string) {
	if t := strings.TrimSpace(tier); t != "" {
		os.Setenv("RUNPOD_CLOUD", t)
	}
}
