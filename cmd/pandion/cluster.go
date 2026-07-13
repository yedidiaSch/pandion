// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/yedidiaSch/pandion/internal/audit"
	"github.com/yedidiaSch/pandion/internal/config"
	"github.com/yedidiaSch/pandion/internal/discovery"
	"github.com/yedidiaSch/pandion/internal/firewall"
	"github.com/yedidiaSch/pandion/internal/harden"
	"github.com/yedidiaSch/pandion/internal/lockfile"
	"github.com/yedidiaSch/pandion/internal/orchestrator"
	"github.com/yedidiaSch/pandion/internal/overlay"
	"github.com/yedidiaSch/pandion/internal/provider"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	"github.com/yedidiaSch/pandion/internal/sshkeys"
	"github.com/yedidiaSch/pandion/internal/stream"
	"github.com/yedidiaSch/pandion/internal/workspace"
)

// syncSpec describes a workspace to push to a node and an optional remote build.
type syncSpec struct {
	LocalPath  string
	RemotePath string
	Build      string
	// Binaries selects "upload as-is" mode: no remote build, and .gitignore is NOT
	// used to filter (so gitignored build output can be shipped). See resolveSync.
	Binaries bool
}

// capsFor normalizes declared capabilities (needs_caps) plus any capability
// implied by a privileged (<1024) port, into cap names WITHOUT the CAP_ prefix
// (e.g. "NET_RAW", "NET_BIND_SERVICE") — the form docker --cap-add wants.
func capsFor(needsCaps, privPorts []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		c = strings.ToUpper(strings.TrimSpace(c))
		c = strings.TrimPrefix(c, "CAP_")
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, c := range needsCaps {
		add(c)
	}
	for _, p := range privPorts {
		n := p
		if i := strings.IndexByte(p, '/'); i >= 0 {
			n = p[:i]
		}
		if v, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && v > 0 && v < 1024 {
			add("NET_BIND_SERVICE")
		}
	}
	return out
}

// ambientCapList renders caps for setpriv, e.g. "+net_raw,+net_bind_service".
// setpriv's cap parser uses names WITHOUT the "cap_" prefix (finding F17).
func ambientCapList(caps []string) string {
	parts := make([]string, len(caps))
	for i, c := range caps {
		parts[i] = "+" + strings.ToLower(c)
	}
	return strings.Join(parts, ",")
}

// runAs wraps a command to run as `user` (login shell, so /etc/profile.d
// discovery is sourced), optionally from workdir. root runs directly; a non-root
// user is dropped into via runuser (least privilege, S-C). If caps are requested
// for a non-root user, setpriv grants exactly those ambient caps (P1b).
func runAs(user, workdir, cmd string, caps []string) string {
	inner := cmd
	if strings.TrimSpace(workdir) != "" {
		inner = "cd " + shellQuote(workdir) + " && " + cmd
	}
	loginShell := "bash -lc " + shellQuote(inner)
	if user == "" || user == "root" {
		return loginShell // root already holds all capabilities
	}
	if len(caps) == 0 {
		return "runuser -u " + shellQuote(user) + " -- " + loginShell
	}
	cl := ambientCapList(caps)
	return "setpriv --reuid=" + shellQuote(user) + " --regid=" + shellQuote(user) +
		" --init-groups --inh-caps=" + cl + " --ambient-caps=" + cl + " -- " + loginShell
}

// workspaceDir is where a synced workspace lands: the run user's home (so a
// non-root user can read/build/run it), unless overridden.
func workspaceDir(runUser, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	if runUser == "" || runUser == "root" {
		return "/root/workspace"
	}
	return "/home/" + runUser + "/workspace"
}

// syncWorkspace archives LocalPath (honoring .pandionignore/.gitignore), streams
// it to the node over the pinned SSH connection, extracts it, and runs the
// optional Build command. Must run inside the egress build-window (before the
// firewall lockdown) so the build can fetch dependencies.
// syncFiles archives LocalPath (honoring .pandionignore) and extracts it on the
// node, chowning to runUser. Returns the remote workspace dir. No build.
func syncFiles(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, s syncSpec, runUser string) (string, error) {
	remote := workspaceDir(runUser, s.RemotePath)
	// binaries mode ships build output that is usually gitignored, so it must not
	// fall back to .gitignore for filtering (.pandionignore still applies).
	ig := workspace.LoadIgnore(s.LocalPath)
	if s.Binaries {
		ig = workspace.LoadIgnoreStrict(s.LocalPath)
		// arch guard: a prebuilt binary built for the wrong architecture fails at
		// run with "Exec format error" — warn now, while it's fixable.
		warnBinaryArchMismatch(ctx, addr, signer, pinned, s.LocalPath, ig)
	}
	data, files, err := workspace.Archive(s.LocalPath, ig)
	if err != nil {
		return "", fmt.Errorf("archive %s: %w", s.LocalPath, err)
	}
	fmt.Printf("  syncing %d files -> %s ...\n", files, remote)
	extract := "mkdir -p " + shellQuote(remote) + " && tar -xzf - -C " + shellQuote(remote)
	if runUser != "" && runUser != "root" {
		extract += " && chown -R " + shellQuote(runUser) + ":" + shellQuote(runUser) + " " + shellQuote(remote)
	}
	if out, err := envssh.RunWithInput(ctx, addr, "root", signer, pinned, extract, bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("extract on node: %v\n%s", err, out)
	}
	return remote, nil
}

// runSetup runs a node's setup commands (as root) in the egress-open build window,
// in order — for software apt can't install (pip/npm/cargo, a vendor repo, a curl'd
// binary). Each command must succeed; the first failure returns an error (the caller
// leaves the node up for debugging, like a build failure). Egress is still open here,
// so commands can fetch from the network.
func runSetup(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, node string, cmds []string) error {
	for _, c := range cmds {
		if strings.TrimSpace(c) == "" {
			continue
		}
		fmt.Printf("[%s] setup: %s\n", node, c)
		if out, err := envssh.Run(ctx, addr, "root", signer, pinned, c); err != nil {
			return fmt.Errorf("setup command failed: %s\n%v\n%s", c, err, out)
		}
	}
	return nil
}

// missingPackages returns the requested apt packages that did NOT install on the
// node (best-effort). It turns a SILENT cloud-init package failure — a typo'd or
// unavailable name is logged but boot continues, so the node looks healthy while
// the library is absent — into a loud, actionable signal, checked while the node
// is still in the egress-open build window. Version pins ("pkg=ver") and apt
// alternatives ("pkg/suite") are compared by package name. A transport error
// yields no missing set (never blocks provisioning on the check itself).
func missingPackages(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, pkgs []string) []string {
	var names []string
	for _, p := range pkgs {
		n := strings.TrimSpace(p)
		if i := strings.IndexAny(n, "=/"); i > 0 {
			n = n[:i] // strip a version pin or suite qualifier
		}
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	cmd := "dpkg-query -W -f='${Package} ${db:Status-Status}\\n' " + strings.Join(names, " ") + " 2>/dev/null || true"
	out, err := envssh.Run(ctx, addr, "root", signer, pinned, cmd)
	if err != nil {
		return nil // don't fail provisioning because the check couldn't run
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "installed" {
			installed[f[0]] = true
		}
	}
	var missing []string
	for _, n := range names {
		if !installed[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

// warnMissingPackages prints a prominent warning naming any requested package that
// did not install, so a missing library is visible now rather than as a confusing
// runtime failure later. Best-effort — never blocks the run.
func warnMissingPackages(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, node string, pkgs []string) {
	if m := missingPackages(ctx, addr, signer, pinned, pkgs); len(m) > 0 {
		fmt.Fprintf(os.Stderr, "⚠ [%s] requested package(s) did NOT install: %s\n", node, strings.Join(m, ", "))
		fmt.Fprintf(os.Stderr, "    check the apt name(s); packages install in the build window (egress is locked after).\n")
	}
}

// nodeArch returns the node's Go arch ("amd64", "arm64", ...) via `uname -m`, or
// "" if it can't be determined (in which case the arch guard is skipped).
func nodeArch(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey) string {
	out, err := envssh.Run(ctx, addr, "root", signer, pinned, "uname -m")
	if err != nil {
		return ""
	}
	switch strings.TrimSpace(out) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l", "armhf":
		return "arm"
	case "i386", "i686":
		return "386"
	default:
		return strings.TrimSpace(out)
	}
}

// warnBinaryArchMismatch (binaries mode) warns if any ELF binary being uploaded is
// built for a different CPU architecture than the node — turning a later, cryptic
// "Exec format error" at run time into an actionable message now, while it's still
// fixable. Best-effort: if the node arch can't be read or nothing is an ELF, it is
// silent, and it never blocks the upload.
func warnBinaryArchMismatch(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, localPath string, ig *workspace.Ignore) {
	na := nodeArch(ctx, addr, signer, pinned)
	if na == "" {
		return
	}
	scan, err := workspace.ELFArchScan(localPath, ig)
	if err != nil {
		return
	}
	var bad []string
	for f, a := range scan {
		if a != "" && a != na {
			bad = append(bad, fmt.Sprintf("%s (built for %s)", f, a))
		}
	}
	if len(bad) == 0 {
		return
	}
	sort.Strings(bad)
	fmt.Fprintf(os.Stderr, "⚠ binary arch mismatch: this node is %s, but you're uploading: %s\n", na, strings.Join(bad, ", "))
	fmt.Fprintf(os.Stderr, "    a wrong-architecture binary fails at run with \"Exec format error\".\n")
	fmt.Fprintf(os.Stderr, "    rebuild for linux/%s, or provision a node with %s architecture.\n", na, archNodeHint(scan, na))
}

// archNodeHint names an architecture the uploaded binaries were actually built for
// (for the "provision a <arch> node" suggestion), falling back to the node arch.
func archNodeHint(scan map[string]string, nodeA string) string {
	for _, a := range scan {
		if a != "" && a != nodeA {
			return a
		}
	}
	return nodeA
}

// syncWorkspace syncs files and runs the optional build on the host as runUser
// (native engine).
func syncWorkspace(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, s syncSpec, runUser string) (string, error) {
	remote, err := syncFiles(ctx, addr, signer, pinned, s, runUser)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s.Build) != "" {
		fmt.Printf("  building (as %s): %s\n", orRoot(runUser), s.Build)
		if out, err := envssh.Run(ctx, addr, "root", signer, pinned, runAs(runUser, remote, s.Build, nil)); err != nil {
			return "", fmt.Errorf("remote build failed: %v\n%s", err, out)
		}
	}
	return remote, nil
}

// dockerRun wraps cmd to run in a HARDENED container from image, with the
// workspace mounted (S-D: drop all caps, no-new-privileges, read-only rootfs,
// no docker.sock, no --privileged). --network=host so the node's overlay is
// usable. cmd runs via sh -c for minimal-image compatibility.
func dockerRun(image, workdir, cmd string, caps []string) string {
	flags := "--rm --cap-drop=ALL --security-opt=no-new-privileges --read-only --tmpfs /tmp:exec --network=host"
	for _, c := range caps {
		flags += " --cap-add=" + c // add back only declared caps (P1b)
	}
	mount := ""
	if strings.TrimSpace(workdir) != "" {
		mount = " -v " + shellQuote(workdir) + ":/workspace -w /workspace"
	}
	return "docker run " + flags + mount + " " + shellQuote(image) + " sh -c " + shellQuote(cmd)
}

func orRoot(u string) string {
	if u == "" {
		return "root"
	}
	return u
}

// nodePlan holds the locally-generated identity for one cluster node.
type nodePlan struct {
	name         string
	host         *sshkeys.KeyPair // per-node SSH host key (pinned)
	wg           overlay.Keypair  // per-node WireGuard key
	overlayIP    string           // e.g. 10.99.0.1
	run          string
	ip           string    // public IP, filled after provisioning
	sync         *syncSpec // workspace to push + optional remote build
	runUser      string    // unprivileged user the workload runs as (S-C)
	workdir      string    // remote workspace dir (cwd for run), if synced
	caps         []string  // capabilities to grant back (needs_caps/priv ports, P1b)
	pkgs         []string  // resolved package list (for the reproducibility lockfile, H2)
	image        string    // node image (recorded in the lockfile)
	egressAllow  []string  // per-node outbound allowlist (cluster.yaml egress_allow / security)
	blockMeta    bool      // block the cloud metadata endpoint (security.block_metadata_service)
	engine       string    // "native" | "docker"
	containerImg string    // container image for engine=docker
	setup        []string  // setup commands run (as root) in the build window
	l2IP         string    // L2 overlay IP (no prefix), e.g. 192.168.66.1; empty if no L2
	l2MAC        string    // L2 overlay MAC, e.g. 02:00:00:00:00:01
}

// resolveSync picks the node's sync config, falling back to cluster defaults.
func resolveSync(node config.Node, defaults config.NodeCommon) *syncSpec {
	s := node.Sync
	if s == nil {
		s = defaults.Sync
	}
	if s == nil {
		return nil
	}
	local := s.Path
	if local == "" {
		local = "./"
	}
	// "binaries" uploads the path as-is (no remote build) — for shipping prebuilt
	// artifacts. "source" (default) syncs and runs the build command on the node.
	if strings.EqualFold(s.Mode, "binaries") {
		return &syncSpec{LocalPath: local, RemotePath: s.RemotePath, Binaries: true}
	}
	return &syncSpec{LocalPath: local, RemotePath: s.RemotePath, Build: s.Build}
}

const operatorOverlayIP = "10.99.0.254"

// nodeManifest is the persisted, reconnect-time view of a node (enough to SSH-pin
// and reach it over the overlay for `lockdown`/`attach`).
type nodeManifest struct {
	Name      string `json:"name"`
	IP        string `json:"ip"`
	OverlayIP string `json:"overlay_ip"`
	HostPub   string `json:"host_pub"`         // authorized-keys line, to pin
	L2IP      string `json:"l2_ip,omitempty"`  // Layer-2 overlay IP (security.overlay: l2)
	L2MAC     string `json:"l2_mac,omitempty"` // Layer-2 overlay MAC
	// Run spec — persisted so `pandion start` can launch the workload later
	// (e.g. after `up --no-run`, or a node with no `run:` started on demand) from
	// the manifest alone, without re-reading cluster.yaml.
	Run          string   `json:"run,omitempty"`
	Engine       string   `json:"engine,omitempty"`        // "native" | "docker"
	ContainerImg string   `json:"container_img,omitempty"` // engine=docker image
	Workdir      string   `json:"workdir,omitempty"`       // remote cwd (synced workspace), if any
	RunUser      string   `json:"run_user,omitempty"`      // unprivileged user to run as
	Caps         []string `json:"caps,omitempty"`          // capabilities to grant back
}

// l2Segment records the cluster's Layer-2 overlay (security.overlay: l2).
type l2Segment struct {
	VNI     int    `json:"vni"`
	Subnet  string `json:"subnet"`
	Profile string `json:"profile"`
}

// clusterManifest is written to ~/.pandion/keys/<id>/manifest.json at `up`.
type clusterManifest struct {
	ID       string         `json:"id"`
	Provider string         `json:"provider,omitempty"` // the backend that created it (so `down` needs no --provider)
	Nodes    []nodeManifest `json:"nodes"`
	L2       *l2Segment     `json:"l2,omitempty"`
}

func manifestPath(id string) string {
	return filepath.Join(envHome(), ".pandion", "keys", id, "manifest.json")
}

func saveManifest(id, providerName string, plans []*nodePlan, seg *l2Segment) error {
	nodes := make([]nodeManifest, 0, len(plans))
	for _, p := range plans {
		nodes = append(nodes, nodeManifest{
			Name: p.name, IP: p.ip, OverlayIP: p.overlayIP, HostPub: p.host.PublicAuthorized,
			L2IP: p.l2IP, L2MAC: p.l2MAC,
			Run: p.run, Engine: p.engine, ContainerImg: p.containerImg,
			Workdir: p.workdir, RunUser: p.runUser, Caps: p.caps,
		})
	}
	return writeManifest(id, providerName, nodes, seg)
}

// writeManifest persists the reconnect-time view (nodes to SSH-pin, plus the owning
// provider) so `attach`/`lockdown`/`down` work for both the single-node and cluster
// flows — `down` reads the provider from here, so it needs no --provider.
func writeManifest(id, providerName string, nodes []nodeManifest, seg *l2Segment) error {
	b, err := json.MarshalIndent(clusterManifest{ID: id, Provider: providerName, Nodes: nodes, L2: seg}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath(id), b, 0o600)
}

// manifestProvider returns the provider that created a cluster (from its manifest),
// or "" if unknown (no manifest, or a manifest from before this field existed).
func manifestProvider(id string) string {
	if m, err := loadManifest(id); err == nil {
		return m.Provider
	}
	return ""
}

// l2Tag returns an `ls` label suffix marking a cluster's L2 overlay: a loud
// warning for the attackable lab profile, a quiet marker for safe. Empty when the
// cluster has no L2 segment (or no local manifest).
func l2Tag(id string) string {
	m, err := loadManifest(id)
	if err != nil || m == nil || m.L2 == nil {
		return ""
	}
	if m.L2.Profile == "lab" {
		return " ⚠L2-LAB"
	}
	return " [L2]"
}

// l2HostAddr returns the L2 overlay address for host number h in subnet — e.g.
// (subnet "192.168.66.0/24", h=3) -> ("192.168.66.3", "192.168.66.3/24"). Falls
// back to the default subnet if the configured one is unparseable.
func l2HostAddr(subnet string, h int) (ipOnly, withPrefix string) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		_, ipnet, _ = net.ParseCIDR(config.DefaultL2Subnet)
	}
	base := ipnet.IP.To4()
	var b4 [4]byte
	binary.BigEndian.PutUint32(b4[:], binary.BigEndian.Uint32(base)+uint32(h))
	ip := net.IP(b4[:]).String()
	ones, _ := ipnet.Mask.Size()
	return ip, fmt.Sprintf("%s/%d", ip, ones)
}

func loadManifest(id string) (*clusterManifest, error) {
	b, err := os.ReadFile(manifestPath(id))
	if err != nil {
		return nil, err
	}
	var m clusterManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// upClusterHetzner provisions a hardened N-node cluster and forms the WireGuard
// mesh (M3.2b), applying the S3-validated barrier pattern:
//
//	provision all concurrently -> wait all booted -> exchange endpoints (wg set)
//	-> verify mutual reachability -> lock down firewall.
//
// Discovery-IP injection, IPC firewall, and running the per-node commands land
// in M3.3/M3.4.
// queryNodeLock runs the toolchain-version query on a node and returns its
// resolved environment (H2). A query failure yields an empty record rather than
// aborting the provision — the lockfile is best-effort.
func queryNodeLock(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, name, image string, pkgs []string) lockfile.NodeLock {
	out, err := envssh.Run(ctx, addr, "root", signer, pinned, lockfile.QueryCmd(pkgs))
	if err != nil {
		return lockfile.NodeLock{Name: name, Image: image, Packages: map[string]string{}}
	}
	nl := lockfile.ParseQuery(out)
	nl.Name, nl.Image = name, image
	return nl
}

// ensureCloudFirewall applies the provider-level edge firewall if the provider
// supports it (M8). Best-effort: a failure warns but never blocks the run (the
// host nftables is the primary control).
func ensureCloudFirewall(ctx context.Context, o *orchestrator.Orchestrator, id string) {
	fw, ok := o.P.(provider.ClusterFirewaller)
	if !ok {
		return
	}
	if err := fw.EnsureClusterFirewall(ctx, id, overlay.DefaultPort); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cloud firewall not applied: %v\n", err)
		return
	}
	fmt.Println("cloud-edge firewall applied (SSH + WireGuard + ICMP inbound only).")
}

// writeLock persists the per-cluster reproducibility lockfile (~/.pandion/lock/<id>.json).
func writeLock(id, prov string, nodes []lockfile.NodeLock) error {
	return lockfile.Save(envHome(), &lockfile.Lock{
		ID: id, Provider: prov, PandionVersion: version, Created: time.Now(), Nodes: nodes,
	})
}

// dryRunCluster previews a cluster `up` — per-node size/region/TTL and projected
// cost from cluster.yaml — creating nothing (no keys, no cloud-init, no cloud).
// gpuReqOf parses a node's effective `gpu:` string ("MODEL[:COUNT]") into a
// GPUReq. A parse error aborts (the topology is malformed).
func gpuReqOf(effGPU string) provider.GPUReq {
	g, err := parseGPUFlag(effGPU)
	must(err)
	return g
}

// clusterWantsGPU reports whether any node in the topology resolves to a GPU.
func clusterWantsGPU(cl *config.Cluster) bool {
	for _, n := range cl.Nodes {
		if gpuReqOf(cl.Effective(n).GPU).Wanted() {
			return true
		}
	}
	return false
}

func dryRunCluster(o *orchestrator.Orchestrator, cl *config.Cluster, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	specs := make([]orchestrator.NodeSpec, len(cl.Nodes))
	windows := make([]time.Duration, len(cl.Nodes))
	for i, n := range cl.Nodes {
		eff := cl.Effective(n)
		var region []string
		if eff.Region != "" {
			region = []string{eff.Region}
		}
		specs[i] = orchestrator.NodeSpec{Name: n.Name, Type: eff.Size, RegionPref: region, GPU: gpuReqOf(eff.GPU)}
		windows[i] = parseTTL(eff.TTLRaw)
	}
	nodes, est, err := o.PlanUp(ctx, specs, windows)
	must(err)
	renderDryRun(os.Stdout, o.P.Name(), id, nodes, est)
}

func upClusterHetzner(o *orchestrator.Orchestrator, cl *config.Cluster, id string, maxCost float64, lockPath string, noRun bool, auditFW bool) {
	prov := o.P.Name()         // hetzner | digitalocean — used in teardown hints
	var pinLock *lockfile.Lock // reproducibility (H2): pin toolchain from this lock
	if lockPath != "" {
		lk, lerr := lockfile.Load(lockPath)
		must(lerr)
		pinLock = lk
		fmt.Printf("pinning packages from lockfile %s\n", lockPath)
	}
	// Generous ceiling: it only bounds failures — every step succeeds as soon as
	// its node is ready, so fast providers finish early. Slow-boot providers
	// (e.g. Scaleway's two-phase boot) need the extra room to install the login
	// key via cloud-init under concurrent multi-node provisioning.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// Phase-aware Ctrl+C handling (F15): during SETUP an interrupt rolls the
	// cluster back (so an aborted creation never orphans a billing node); once we
	// reach the streaming phase it DETACHES instead, leaving infra up (C3).
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	var streaming atomic.Bool
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			if streaming.Load() {
				fmt.Println("\n^C — detaching from stream; cluster left running.")
				streamCancel()
			} else {
				fmt.Fprintln(os.Stderr, "\n^C — aborting provisioning; rolling back...")
				_ = o.Down(context.Background(), id)
				fmt.Fprintln(os.Stderr, "rolled back (no resources left).")
				os.Exit(5)
			}
		}
	}()

	// shared login key for the whole cluster; operator WG identity
	login, err := sshkeys.Generate("pandion-login")
	must(err)
	opWG, err := overlay.GenerateKeypair()
	must(err)

	// Layer-2 overlay (security.overlay: l2) is cluster-wide: if any node/defaults
	// request it, every node gets a vxlan0 segment. "safe" is spoof-resistant
	// (host DAI); "lab" is a deliberately ATTACKABLE cyber-range.
	var clusterL2 *config.L2Overlay
	for _, n := range cl.Nodes {
		if e := cl.Effective(n).L2; e != nil {
			clusterL2 = e
			break
		}
	}
	if clusterL2 != nil && clusterL2.Profile == "lab" {
		// The lab profile deliberately weakens the L2 segment (ARP-spoof/MITM
		// enabled). It is opt-in and explicit; make it LOUD and audited. The
		// management plane (wg0) stays hardened; attacks are contained to the
		// private overlay (isolated from the provider LAN and the internet).
		fmt.Fprintln(os.Stderr, "┌──────────────────────────────────────────────────────────────┐")
		fmt.Fprintln(os.Stderr, "│  ⚠  L2 LAB PROFILE — this cluster has an ATTACKABLE Layer-2   │")
		fmt.Fprintln(os.Stderr, "│  segment: ARP spoofing / MITM are ENABLED on vxlan0. It is    │")
		fmt.Fprintln(os.Stderr, "│  isolated to the encrypted overlay (not the provider LAN or   │")
		fmt.Fprintln(os.Stderr, "│  the internet) and wg0 stays hardened. Authorized labs only.  │")
		fmt.Fprintln(os.Stderr, "└──────────────────────────────────────────────────────────────┘")
		audit.Event("overlay.l2", "id", id, "profile", "lab", "subnet", clusterL2.Subnet)
	} else if clusterL2 != nil {
		audit.Event("overlay.l2", "id", id, "profile", clusterL2.Profile, "subnet", clusterL2.Subnet)
	}

	// build per-node plans + hardened cloud-init (host key, toolchain, wg iface)
	plans := make([]*nodePlan, len(cl.Nodes))
	specs := make([]orchestrator.NodeSpec, len(cl.Nodes))
	windows := make([]time.Duration, len(cl.Nodes)) // per-node idle-TTL, for --max-cost
	for i, n := range cl.Nodes {
		host, e := sshkeys.Generate("pandion-host-" + n.Name)
		must(e)
		wg, e := overlay.GenerateKeypair()
		must(e)
		oip := fmt.Sprintf("10.99.0.%d", i+1)

		// apply cluster.yaml per-node settings (P0-2), with defaults inheritance.
		eff := cl.Effective(n)
		var pkgs []string
		if eff.Engine == "docker" {
			pkgs = []string{"docker.io"} // the container image provides the toolchain
		} else {
			// built-in toolchain + declared libraries (toolchain.packages), deduped.
			pkgs = harden.ResolveToolchain(eff.Packages, eff.NoDefaultToolchain)
		}
		pkgs = append(append([]string{}, pkgs...), "nftables", "wireguard", "tmux") // always needed (tmux: durable run + attach)
		if pinLock != nil {
			pkgs = pinLock.PinnedPackages(n.Name, pkgs) // H2: reproduce recorded versions
		}
		var region []string
		if eff.Region != "" {
			region = []string{eff.Region}
		}
		runUser := eff.RunUser
		if runUser == "" {
			runUser = harden.DefaultRunUser // least-privilege by default (S-C)
		}

		plans[i] = &nodePlan{name: n.Name, host: host, wg: wg, overlayIP: oip, run: n.Run,
			sync: resolveSync(n, cl.Defaults), runUser: runUser,
			caps: capsFor(n.NeedsCaps, n.PrivilegedPorts), pkgs: pkgs, image: eff.Image,
			egressAllow: eff.EgressAllow, blockMeta: eff.BlockMetadata,
			engine: eff.Engine, containerImg: eff.ContainerImage, setup: eff.Setup}

		// Layer-2 overlay: assign this node a deterministic L2 IP + MAC (host = i+1,
		// matching the wg overlay host number) and render the vxlan0 bring-up script.
		var l2Script string
		if clusterL2 != nil {
			host := i + 1
			l2ip, l2addr := l2HostAddr(clusterL2.Subnet, host)
			mac := fmt.Sprintf("02:00:00:00:00:%02x", host)
			plans[i].l2IP, plans[i].l2MAC = l2ip, mac
			l2Script = strings.Join(overlay.VXLANBringUp(overlay.L2NodeSpec{
				VNI: clusterL2.VNI, LocalWG: oip, Addr: l2addr, MAC: mac,
				MTU: overlay.MTUFor(overlay.DefaultWGMTU), Profile: clusterL2.Profile,
			}), "\n") + "\n"
		}

		windows[i] = parseTTL(eff.TTLRaw)
		ci := harden.CloudInit{
			HostPrivKeyPEM:   host.PrivatePEM,
			HostPubKey:       host.PublicAuthorized,
			LoginPubKey:      login.PublicAuthorized,
			Packages:         pkgs,
			WGConfig:         overlay.InterfaceConfig(wg.Private, oip+"/24", overlay.DefaultPort),
			RunUser:          runUser,
			IdleTTL:          windows[i],
			HasGPU:           gpuReqOf(eff.GPU).Wanted(), // GPU util counts as liveness (§4, M5)
			GPUIdleUtil:      harden.DefaultGPUIdleUtil,
			Fail2ban:         true,               // SSH brute-force protection (P1)
			AuditLog:         eff.AuditLog,       // on-node audit trail (S-F; security.audit_log)
			SysctlHardening:  true,               // CIS-lite kernel network baseline (P1)
			EncryptWorkspace: eff.EncryptVolumes, // LUKS at rest (S-E; security.encrypt_volumes)
			L2Script:         l2Script,           // vxlan0 bring-up (security.overlay: l2)
		}
		specs[i] = orchestrator.NodeSpec{
			Name: n.Name, UserData: harden.Build(ci), LoginPubKey: login.PublicAuthorized,
			Type: eff.Size, Image: eff.Image, RegionPref: region, GPU: gpuReqOf(eff.GPU),
		}
	}

	// --max-cost preflight (before spending a cent): refuse if projected spend
	// (Σ hourly × each node's TTL) exceeds the cap.
	if err := o.CheckBudget(ctx, specs, windows, maxCost); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(6)
	}

	// provision concurrently; BARRIER: returns only when all are RUNNING
	fmt.Printf("UP cluster %q: provisioning %d hardened nodes (concurrent)...\n", id, len(plans))
	c, err := o.UpCluster(ctx, id, specs, orchestrator.DefaultMaxConcurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster provisioning failed: %v — rolling back...\n", err)
		_ = o.Down(context.Background(), id)
		os.Exit(5)
	}
	ipByName := map[string]string{}
	for _, nd := range c.Nodes {
		ipByName[nd.Name] = nd.IP
	}
	for _, p := range plans {
		p.ip = ipByName[p.name]
		fmt.Printf("  %-12s ip=%-15s overlay=%s\n", p.name, p.ip, p.overlayIP)
		audit.Event("provision", "id", id, "node", p.name, "provider", prov, "ip", p.ip, "engine", p.engine)
	}

	// cloud-edge firewall (defense-in-depth in front of host nftables, M8).
	ensureCloudFirewall(ctx, o, id)

	// persist keys for later attach/debug
	keyDir := filepath.Join(envHome(), ".pandion", "keys", id)
	must(login.Save(keyDir, "login_ed25519"))

	// wait for each node's cloud-init to finish (toolchain + wg iface up), pinned
	fmt.Println("waiting for cloud-init on all nodes...")
	for _, p := range plans {
		onAttempt := func(n int, reason string) { fmt.Printf("  %s attempt %d: %s\n", p.name, n, reason) }
		if _, err := envssh.RunWithRetry(ctx, p.ip+":22", "root", login.Signer, p.host.Public,
			"cloud-init status --wait || true", 5*time.Second, onAttempt); err != nil {
			fmt.Fprintf(os.Stderr, "node %s never became ready: %v (cluster left up)\n", p.name, err)
			fmt.Printf("teardown: pandion down --provider=%s --id %s\n", prov, id)
			return
		}
	}

	// reproducibility (H2): record each node's resolved toolchain versions so a
	// later `up -f … --lock ~/.pandion/lock/<id>.json` reproduces the environment.
	locks := make([]lockfile.NodeLock, len(plans))
	for i, p := range plans {
		locks[i] = queryNodeLock(ctx, p.ip+":22", login.Signer, p.host.Public, p.name, p.image, p.pkgs)
	}
	if err := writeLock(id, prov, locks); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write lockfile: %v\n", err)
	} else {
		fmt.Printf("wrote reproducibility lockfile: %s\n", lockfile.Path(envHome(), id))
	}

	// setup commands (non-apt software) run FIRST in the build window — after the
	// apt packages (installed at boot) and before the workspace build, so a build
	// can use what setup installed. Egress is still open here. Fail-fast: a failure
	// leaves the cluster up for debugging and exits NON-ZERO so scripts/CI notice.
	for _, p := range plans {
		if err := runSetup(ctx, p.ip+":22", login.Signer, p.host.Public, p.name, p.setup); err != nil {
			fmt.Fprintf(os.Stderr, "node %s: %v (cluster left up for debugging)\n", p.name, err)
			fmt.Printf("teardown: pandion down --provider=%s --id %s\n", prov, id)
			os.Exit(1)
		}
	}

	// sync workspaces + remote build, in the egress build-window (before the
	// firewall lockdown so builds can fetch dependencies). (C5/H1/H3, P0-1)
	// docker nodes ALSO pull their image here, while egress is still open.
	for _, p := range plans {
		fail := func(err error) {
			fmt.Fprintf(os.Stderr, "node %s: %v (cluster left up for debugging)\n", p.name, err)
			fmt.Printf("teardown: pandion down --provider=%s --id %s\n", prov, id)
		}
		if p.engine == "docker" {
			fmt.Printf("[%s] pulling image %s...\n", p.name, p.containerImg)
			if out, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public,
				"docker pull "+shellQuote(p.containerImg)); err != nil {
				fail(fmt.Errorf("docker pull failed: %v\n%s", err, out))
				return
			}
			if p.sync != nil {
				fmt.Printf("[%s] workspace sync...\n", p.name)
				wd, err := syncFiles(ctx, p.ip+":22", login.Signer, p.host.Public, *p.sync, "root")
				if err != nil {
					fail(err)
					return
				}
				p.workdir = wd
				if b := strings.TrimSpace(p.sync.Build); b != "" {
					fmt.Printf("[%s] building in container (%s)...\n", p.name, p.containerImg)
					if out, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public,
						dockerRun(p.containerImg, wd, b, nil)); err != nil {
						fail(fmt.Errorf("container build failed: %v\n%s", err, out))
						return
					}
				}
			}
			continue
		}
		if p.sync == nil {
			continue
		}
		fmt.Printf("[%s] workspace sync...\n", p.name)
		wd, err := syncWorkspace(ctx, p.ip+":22", login.Signer, p.host.Public, *p.sync, p.runUser)
		if err != nil {
			fail(err)
			return
		}
		p.workdir = wd
	}

	// verify requested packages installed on each node (native), while egress is
	// still open — a missing library is reported loudly now, not as a later crash.
	for _, p := range plans {
		if p.engine != "docker" {
			warnMissingPackages(ctx, p.ip+":22", login.Signer, p.host.Public, p.name, p.pkgs)
		}
	}

	// detect operator public IP (as node 0 sees our SSH source)
	operatorCIDR := ""
	if src, err := envssh.Run(ctx, plans[0].ip+":22", "root", login.Signer, plans[0].host.Public,
		"echo $SSH_CLIENT | awk '{print $1}'"); err == nil {
		if s := trimSpace(src); s != "" {
			operatorCIDR = s + "/32"
		}
	}

	// form the mesh: each node peers with every OTHER node (both directions have
	// public endpoints) + the operator (allowed-ips only; operator roams).
	fmt.Println("forming WireGuard mesh (wg set peers)...")
	for _, p := range plans {
		var cmds []string
		for _, q := range plans {
			if q.name == p.name {
				continue
			}
			cmds = append(cmds, overlay.SetPeerCommand("wg0", q.wg.Public, q.ip, overlay.DefaultPort, q.overlayIP))
		}
		// operator peer (no endpoint; operator initiates)
		cmds = append(cmds, fmt.Sprintf("wg set wg0 peer %s allowed-ips %s/32", opWG.Public, operatorOverlayIP))
		if _, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public, joinAmp(cmds)); err != nil {
			fmt.Fprintf(os.Stderr, "mesh setup failed on %s: %v (cluster left up)\n", p.name, err)
			return
		}
	}

	// Layer-2 overlay: inject the static FDB (unicast MAC->VTEP + BUM flood) so the
	// vxlan0 broadcast domain works without multicast. FDB is kernel state, so it
	// survives the firewall's `flush ruleset` below; the DAI nft rules do NOT, so
	// they are applied AFTER the firewall (see below).
	if clusterL2 != nil {
		fmt.Println("injecting L2 overlay FDB...")
		for _, p := range plans {
			var cmds []string
			for _, q := range plans {
				if q.name == p.name {
					continue
				}
				cmds = append(cmds, overlay.FDBInject(q.l2MAC, q.overlayIP)...)
			}
			if _, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public, joinAmp(cmds)); err != nil {
				fmt.Fprintf(os.Stderr, "L2 FDB setup failed on %s: %v (cluster left up)\n", p.name, err)
				return
			}
		}
	}

	// verify mutual reachability over the overlay (each node pings the next)
	fmt.Println("verifying mesh reachability...")
	meshOK := true
	for i, p := range plans {
		q := plans[(i+1)%len(plans)]
		if q.name == p.name {
			continue
		}
		out, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public,
			fmt.Sprintf("ping -c2 -W3 %s >/dev/null 2>&1 && echo OK || echo FAIL", q.overlayIP))
		if err != nil || trimSpace(out) != "OK" {
			fmt.Printf("  %s -> %s (%s): FAIL\n", p.name, q.name, q.overlayIP)
			meshOK = false
		} else {
			fmt.Printf("  %s -> %s (%s): OK\n", p.name, q.name, q.overlayIP)
		}
	}

	// apply per-node firewall: SSH from operator + WG + overlay-trust, egress deny
	if auditFW {
		fmt.Println("applying per-node firewall in AUDIT mode (nothing enforced; would-be-drops logged)...")
	} else {
		fmt.Println("applying per-node firewall...")
	}
	for _, p := range plans {
		rules := firewall.NFTables(firewall.Spec{
			AllowDNS: true, SSHFromCIDR: operatorCIDR,
			WGPort: overlay.DefaultPort, AllowOverlayInput: true,
			AllowL2Input:   clusterL2 != nil,                  // accept decapsulated vxlan0 frames
			EgressAllowIPs: resolveEgressAllow(p.egressAllow), // egress_allow / security; hostnames resolved (P0-2)
			BlockMetadata:  p.blockMeta,                       // S-F (honors security.block_metadata_service)
			AuditOnly:      auditFW,                           // --firewall-audit: dry-run, log not drop (P2.2)
		})
		cmd := "echo " + b64(rules) + " | base64 -d | nft -f -"
		if _, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "firewall apply failed on %s: %v\n", p.name, err)
		}
		if !auditFW {
			// keep hostname egress-allow rules fresh as CDN IPs rotate (P2.1 follow-up)
			installEgressRefresh(ctx, p.ip+":22", login.Signer, p.host.Public, egressAllowHostnames(p.egressAllow))
		}
	}

	// Layer-2 overlay (safe profile): apply host-side Dynamic ARP Inspection — an
	// nftables arp table pinning each IP<->MAC binding, so a forged ARP cannot
	// poison a neighbor. Applied AFTER the firewall (which flushes the ruleset) as
	// a SEPARATE table, and re-applied idempotently.
	if clusterL2 != nil && clusterL2.Profile == "safe" {
		fmt.Println("applying L2 ARP-inspection (safe profile)...")
		bindings := make([]overlay.L2Binding, 0, len(plans))
		for _, p := range plans {
			bindings = append(bindings, overlay.L2Binding{L2IP: p.l2IP, MAC: p.l2MAC, WGIP: p.overlayIP})
		}
		dai := overlay.DAIRules(bindings)
		cmd := "nft delete table arp pandion_dai 2>/dev/null; echo " + b64(dai) + " | base64 -d | nft -f -"
		for _, p := range plans {
			if _, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public, cmd); err != nil {
				fmt.Fprintf(os.Stderr, "L2 ARP-inspection apply failed on %s: %v\n", p.name, err)
			}
		}
	}

	// inject service discovery (overlay IPs) into every node so run commands can
	// reach siblings via $PANDION_<NODE>_IP with no hardcoded IPs (C5/H1).
	overlayByName := map[string]string{}
	for _, p := range plans {
		overlayByName[p.name] = p.overlayIP
	}
	fmt.Println("injecting service discovery...")
	for _, p := range plans {
		script := discovery.Script(overlayByName, p.name)
		cmd := "echo " + b64(script) + " | base64 -d > " + discovery.Path + " && chmod 0644 " + discovery.Path
		if _, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "discovery injection failed on %s: %v\n", p.name, err)
		}
	}

	// write the operator's mesh config (peers = all nodes)
	peers := make([]overlay.OperatorPeer, len(plans))
	for i, p := range plans {
		peers[i] = overlay.OperatorPeer{
			PubKey: p.wg.Public, Endpoint: fmt.Sprintf("%s:%d", p.ip, overlay.DefaultPort),
			AllowedIP: p.overlayIP + "/32",
		}
	}
	opConf := overlay.OperatorConfigMulti(opWG.Private, operatorOverlayIP+"/32", peers)
	confPath := filepath.Join(keyDir, "wg-"+id+".conf")
	must(os.WriteFile(confPath, []byte(opConf), 0o600))

	// persist the manifest so `lockdown`/`attach` can reconnect (pin + overlay)
	var seg *l2Segment
	if clusterL2 != nil {
		seg = &l2Segment{VNI: clusterL2.VNI, Subnet: clusterL2.Subnet, Profile: clusterL2.Profile}
	}
	if err := saveManifest(id, prov, plans, seg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save cluster manifest: %v\n", err)
	}

	fmt.Println("----------------------------------------------------------------")
	if meshOK {
		fmt.Printf("cluster %q UP: %d nodes hardened + WireGuard mesh verified.\n", id, len(plans))
	} else {
		fmt.Printf("cluster %q UP but mesh verification had failures (see above).\n", id)
	}
	fmt.Printf("operator overlay config: %s\n", confPath)
	fmt.Printf("  join: sudo wg-quick up %s   (reach nodes at 10.99.0.1..%d)\n", confPath, len(plans))

	// run per-node commands with MULTIPLEXED streaming (M4): color-coded, prefixed
	// by node, tee'd to per-node logs. From here Ctrl+C detaches (streaming=true),
	// leaving infra up (C3); a crash (non-zero exit) is reported without auto-
	// restart (§5 fail-fast). With --no-run we DEPLOY only (provision + sync +
	// build) and leave the workloads unstarted — launch them later with `start`.
	streaming.Store(true)
	if noRun {
		audit.Event("up.complete", "id", id, "provider", prov, "nodes", len(plans), "no_run", true)
		fmt.Printf("deployed %d node(s) — nothing started (--no-run).\n", len(plans))
		fmt.Printf("  start:    pandion start --id %s [--node NAME]\n", id)
		fmt.Printf("  teardown: pandion down --provider=%s --id %s\n", prov, id)
		return
	}
	streamCluster(streamCtx, id, plans, login)

	audit.Event("up.complete", "id", id, "provider", prov, "nodes", len(plans))
	fmt.Printf("teardown: pandion down --provider=%s --id %s\n", prov, id)
}

// streamCluster runs each node's command concurrently and multiplexes output.
// ctx is cancelled by the caller's Ctrl+C handler to detach.
// runLogPath is where each node's run output is captured (so `attach` can
// reconnect and tail it); runExitMarker is the sentinel appended when the
// workload exits, carrying its exit code so a crash is still visible after
// detach (fail-fast survives the tmux hand-off).
const runLogPath = "/var/log/pandion/run.log"
const runTmuxSession = "pandion"
const runExitMarker = "__pandion_exit__"

// launchRun starts cmd in a DETACHED tmux session, so the workload survives
// detach (Ctrl+C) — "detach != destroy" (C3). Idempotent. Output is redirected
// to runLogPath (tailLog streams it live), and the workload's exit code is
// appended as runExitMarker so crashes remain observable through the log.
func launchRun(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, cmd string) error {
	// no pipe: $? is the workload's own exit code (no PIPESTATUS/bash needed).
	// $? is double-quoted (not shellQuote'd) so the innermost tmux shell expands
	// it — the outer shellQuote(inner) delivers this verbatim to tmux.
	inner := cmd + " > " + runLogPath + " 2>&1; echo \"" + runExitMarker + " $?\" >> " + runLogPath
	launch := "mkdir -p /var/log/pandion; tmux kill-session -t " + runTmuxSession + " 2>/dev/null; " +
		"tmux new-session -d -s " + runTmuxSession + " " + shellQuote(inner)
	out, err := envssh.Run(ctx, addr, "root", signer, pinned, launch)
	if err != nil {
		return fmt.Errorf("%v\n%s", err, out)
	}
	return nil
}

// tailLog streams a node's run log (multiplexed) until ctx is cancelled (detach)
// or the workload exits. On exit it reports the code — a non-zero code is a
// crash, and the node is left up for GDB/SSH (§5) — then stops tailing this node.
func tailLog(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, node string, printer *stream.Printer) {
	nctx, ncancel := context.WithCancel(ctx)
	defer ncancel()
	var exited bool
	err := envssh.Stream(nctx, addr, "root", signer, pinned, "tail -n +1 -F "+runLogPath,
		func(s, line string) {
			if code, ok := strings.CutPrefix(line, runExitMarker); ok {
				exited = true
				if code = strings.TrimSpace(code); code == "0" {
					printer.Print(node, "out", "process exited cleanly (code 0)")
				} else {
					printer.Print(node, "err", "process exited (code "+code+") — node left up for GDB/SSH")
				}
				ncancel() // workload is done; unwind this node's stream
				return
			}
			printer.Print(node, "out", line)
		})
	if err != nil && ctx.Err() == nil && !exited {
		printer.Print(node, "err", "log stream ended: "+err.Error())
	}
}

func streamCluster(ctx context.Context, id string, plans []*nodePlan, login *sshkeys.KeyPair) {
	var runnable []*nodePlan
	for _, p := range plans {
		if strings.TrimSpace(p.run) != "" {
			runnable = append(runnable, p)
		}
	}
	if len(runnable) == 0 {
		return
	}

	// launch each command in a detached tmux session (survives Ctrl+C) teeing to
	// the node's run log, THEN multiplex by tailing those logs. docker nodes run
	// the workload in a hardened container; native nodes run as the run user.
	for _, p := range runnable {
		var runCmd string
		if p.engine == "docker" {
			runCmd = dockerRun(p.containerImg, p.workdir, p.run, p.caps)
		} else {
			runCmd = runAs(p.runUser, p.workdir, p.run, p.caps)
		}
		if err := launchRun(ctx, p.ip+":22", login.Signer, p.host.Public, runCmd); err != nil {
			fmt.Fprintf(os.Stderr, "launch on %s failed: %v\n", p.name, err)
			return
		}
	}

	logDir := filepath.Join(envHome(), ".pandion", "logs", id)
	printer := stream.NewPrinter(os.Stdout, logDir, colorEnabled())
	defer printer.Close()
	fmt.Printf("streaming %d node command(s) (Ctrl+C detaches; reattach: pandion attach --id %s)\n", len(runnable), id)
	fmt.Println("----------------------------------------------------------------")

	var wg sync.WaitGroup
	for _, p := range runnable {
		wg.Add(1)
		go func(p *nodePlan) {
			defer wg.Done()
			tailLog(ctx, p.ip+":22", login.Signer, p.host.Public, p.name, printer)
		}(p)
	}
	wg.Wait()
}

// attachCluster reconnects to a running cluster's streams from its persisted
// manifest (node IPs + pinned host keys) — the multiplexed view, again. Ctrl+C
// detaches; the workloads (in tmux) keep running.
func attachCluster(id string) error {
	man, err := loadManifest(id)
	if err != nil {
		return fmt.Errorf("no manifest for %q (is the id correct? manifest lives in ~/.pandion/keys/%s/): %w", id, id, err)
	}
	pemPath := filepath.Join(envHome(), ".pandion", "keys", id, "login_ed25519")
	pem, err := os.ReadFile(pemPath)
	if err != nil {
		return fmt.Errorf("read login key %s: %w", pemPath, err)
	}
	signer, err := gossh.ParsePrivateKey(pem)
	if err != nil {
		return fmt.Errorf("bad login key: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; fmt.Println("\n^C — detaching; cluster left running."); cancel() }()
	defer signal.Stop(sig)

	printer := stream.NewPrinter(os.Stdout, filepath.Join(envHome(), ".pandion", "logs", id), colorEnabled())
	defer printer.Close()
	fmt.Printf("attaching to %d node(s) of cluster %q (Ctrl+C detaches)...\n", len(man.Nodes), id)
	fmt.Println("----------------------------------------------------------------")

	var wg sync.WaitGroup
	for _, n := range man.Nodes {
		pinned, perr := parsePinned(n.HostPub)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "bad host key for %s: %v\n", n.Name, perr)
			continue
		}
		wg.Add(1)
		go func(name, ip string, pk gossh.PublicKey) {
			defer wg.Done()
			tailLog(ctx, ip+":22", signer, pk, name, printer)
		}(n.Name, n.IP, pinned)
	}
	wg.Wait()
	return nil
}

// colorEnabled reports whether to colorize output (respects NO_COLOR).
func colorEnabled() bool {
	return os.Getenv("NO_COLOR") == ""
}
