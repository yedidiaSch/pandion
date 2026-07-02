package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/envcore/envcore/internal/config"
	"github.com/envcore/envcore/internal/discovery"
	"github.com/envcore/envcore/internal/firewall"
	"github.com/envcore/envcore/internal/harden"
	"github.com/envcore/envcore/internal/orchestrator"
	"github.com/envcore/envcore/internal/overlay"
	envssh "github.com/envcore/envcore/internal/ssh"
	"github.com/envcore/envcore/internal/sshkeys"
	"github.com/envcore/envcore/internal/stream"
	"github.com/envcore/envcore/internal/workspace"
)

// syncSpec describes a workspace to push to a node and an optional remote build.
type syncSpec struct {
	LocalPath  string
	RemotePath string
	Build      string
}

// runAs wraps a command to run as `user` (login shell, so /etc/profile.d
// discovery is sourced), optionally from workdir. root runs directly; a non-root
// user is dropped into via runuser (least privilege, S-C).
func runAs(user, workdir, cmd string) string {
	inner := cmd
	if strings.TrimSpace(workdir) != "" {
		inner = "cd " + shellQuote(workdir) + " && " + cmd
	}
	loginShell := "bash -lc " + shellQuote(inner)
	if user == "" || user == "root" {
		return loginShell
	}
	return "runuser -u " + shellQuote(user) + " -- " + loginShell
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

// syncWorkspace archives LocalPath (honoring .envcoreignore/.gitignore), streams
// it to the node over the pinned SSH connection, extracts it, and runs the
// optional Build command. Must run inside the egress build-window (before the
// firewall lockdown) so the build can fetch dependencies.
// syncFiles archives LocalPath (honoring .envcoreignore) and extracts it on the
// node, chowning to runUser. Returns the remote workspace dir. No build.
func syncFiles(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, s syncSpec, runUser string) (string, error) {
	remote := workspaceDir(runUser, s.RemotePath)
	ig := workspace.LoadIgnore(s.LocalPath)
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

// syncWorkspace syncs files and runs the optional build on the host as runUser
// (native engine).
func syncWorkspace(ctx context.Context, addr string, signer gossh.Signer, pinned gossh.PublicKey, s syncSpec, runUser string) (string, error) {
	remote, err := syncFiles(ctx, addr, signer, pinned, s, runUser)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s.Build) != "" {
		fmt.Printf("  building (as %s): %s\n", orRoot(runUser), s.Build)
		if out, err := envssh.Run(ctx, addr, "root", signer, pinned, runAs(runUser, remote, s.Build)); err != nil {
			return "", fmt.Errorf("remote build failed: %v\n%s", err, out)
		}
	}
	return remote, nil
}

// dockerRun wraps cmd to run in a HARDENED container from image, with the
// workspace mounted (S-D: drop all caps, no-new-privileges, read-only rootfs,
// no docker.sock, no --privileged). --network=host so the node's overlay is
// usable. cmd runs via sh -c for minimal-image compatibility.
func dockerRun(image, workdir, cmd string) string {
	flags := "--rm --cap-drop=ALL --security-opt=no-new-privileges --read-only --tmpfs /tmp:exec --network=host"
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
	name      string
	host      *sshkeys.KeyPair // per-node SSH host key (pinned)
	wg        overlay.Keypair  // per-node WireGuard key
	overlayIP string           // e.g. 10.99.0.1
	run       string
	ip        string    // public IP, filled after provisioning
	sync      *syncSpec // workspace to push + optional remote build
	runUser   string    // unprivileged user the workload runs as (S-C)
	workdir   string    // remote workspace dir (cwd for run), if synced
}

// resolveSync picks the node's sync config, falling back to cluster defaults.
func resolveSync(node config.Node, defaults config.NodeCommon) *syncSpec {
	s := node.Sync
	if s == nil {
		s = defaults.Sync
	}
	if s == nil || strings.EqualFold(s.Mode, "binaries") {
		// M-sync-1 supports source sync; binaries mode is a fast-follow (H3).
		return nil
	}
	local := s.Path
	if local == "" {
		local = "./"
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
	HostPub   string `json:"host_pub"` // authorized-keys line, to pin
}

// clusterManifest is written to ~/.envcore/keys/<id>/manifest.json at `up`.
type clusterManifest struct {
	ID    string         `json:"id"`
	Nodes []nodeManifest `json:"nodes"`
}

func manifestPath(id string) string {
	return filepath.Join(envHome(), ".envcore", "keys", id, "manifest.json")
}

func saveManifest(id string, plans []*nodePlan) error {
	m := clusterManifest{ID: id}
	for _, p := range plans {
		m.Nodes = append(m.Nodes, nodeManifest{
			Name: p.name, IP: p.ip, OverlayIP: p.overlayIP, HostPub: p.host.PublicAuthorized,
		})
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath(id), b, 0o600)
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
func upClusterHetzner(o *orchestrator.Orchestrator, cl *config.Cluster, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
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
	login, err := sshkeys.Generate("envcore-login")
	must(err)
	opWG, err := overlay.GenerateKeypair()
	must(err)

	// build per-node plans + hardened cloud-init (host key, toolchain, wg iface)
	plans := make([]*nodePlan, len(cl.Nodes))
	specs := make([]orchestrator.NodeSpec, len(cl.Nodes))
	for i, n := range cl.Nodes {
		host, e := sshkeys.Generate("envcore-host-" + n.Name)
		must(e)
		wg, e := overlay.GenerateKeypair()
		must(e)
		oip := fmt.Sprintf("10.99.0.%d", i+1)

		// apply cluster.yaml per-node settings (P0-2), with defaults inheritance.
		eff := cl.Effective(n)
		pkgs := eff.Packages
		if len(pkgs) == 0 {
			pkgs = harden.DefaultToolchain() // C++ default when none specified
		}
		pkgs = append(append([]string{}, pkgs...), "nftables", "wireguard") // always needed
		var region []string
		if eff.Region != "" {
			region = []string{eff.Region}
		}
		runUser := eff.RunUser
		if runUser == "" {
			runUser = harden.DefaultRunUser // least-privilege by default (S-C)
		}

		plans[i] = &nodePlan{name: n.Name, host: host, wg: wg, overlayIP: oip, run: n.Run,
			sync: resolveSync(n, cl.Defaults), runUser: runUser}

		ci := harden.CloudInit{
			HostPrivKeyPEM: host.PrivatePEM,
			HostPubKey:     host.PublicAuthorized,
			LoginPubKey:    login.PublicAuthorized,
			Packages:       pkgs,
			WGConfig:       overlay.InterfaceConfig(wg.Private, oip+"/24", overlay.DefaultPort),
			RunUser:        runUser,
			IdleTTL:        parseTTL(eff.TTLRaw),
		}
		specs[i] = orchestrator.NodeSpec{
			Name: n.Name, UserData: harden.Build(ci), LoginPubKey: login.PublicAuthorized,
			Type: eff.Size, Image: eff.Image, RegionPref: region,
		}
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
	}

	// persist keys for later attach/debug
	keyDir := filepath.Join(envHome(), ".envcore", "keys", id)
	must(login.Save(keyDir, "login_ed25519"))

	// wait for each node's cloud-init to finish (toolchain + wg iface up), pinned
	fmt.Println("waiting for cloud-init on all nodes...")
	for _, p := range plans {
		onAttempt := func(n int, reason string) { fmt.Printf("  %s attempt %d: %s\n", p.name, n, reason) }
		if _, err := envssh.RunWithRetry(ctx, p.ip+":22", "root", login.Signer, p.host.Public,
			"cloud-init status --wait || true", 5*time.Second, onAttempt); err != nil {
			fmt.Fprintf(os.Stderr, "node %s never became ready: %v (cluster left up)\n", p.name, err)
			fmt.Printf("teardown: envcore down --provider=hetzner --id %s\n", id)
			return
		}
	}

	// sync workspaces + remote build, in the egress build-window (before the
	// firewall lockdown so builds can fetch dependencies). (C5/H1/H3, P0-1)
	for _, p := range plans {
		if p.sync == nil {
			continue
		}
		fmt.Printf("[%s] workspace sync...\n", p.name)
		wd, err := syncWorkspace(ctx, p.ip+":22", login.Signer, p.host.Public, *p.sync, p.runUser)
		if err != nil {
			fmt.Fprintf(os.Stderr, "node %s: %v (cluster left up for debugging)\n", p.name, err)
			fmt.Printf("teardown: envcore down --provider=hetzner --id %s\n", id)
			return
		}
		p.workdir = wd
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
	fmt.Println("applying per-node firewall...")
	for _, p := range plans {
		rules := firewall.NFTables(firewall.Spec{
			AllowDNS: true, SSHFromCIDR: operatorCIDR,
			WGPort: overlay.DefaultPort, AllowOverlayInput: true,
		})
		cmd := "echo " + b64(rules) + " | base64 -d | nft -f -"
		if _, err := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "firewall apply failed on %s: %v\n", p.name, err)
		}
	}

	// inject service discovery (overlay IPs) into every node so run commands can
	// reach siblings via $ENVCORE_<NODE>_IP with no hardcoded IPs (C5/H1).
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
	if err := saveManifest(id, plans); err != nil {
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
	// restart (§5 fail-fast).
	streaming.Store(true)
	streamCluster(streamCtx, id, plans, login)

	fmt.Printf("teardown: envcore down --provider=hetzner --id %s\n", id)
}

// streamCluster runs each node's command concurrently and multiplexes output.
// ctx is cancelled by the caller's Ctrl+C handler to detach.
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

	logDir := filepath.Join(envHome(), ".envcore", "logs", id)
	printer := stream.NewPrinter(os.Stdout, logDir, colorEnabled())
	defer printer.Close()

	fmt.Printf("streaming %d node command(s) (Ctrl+C detaches; logs: %s)\n", len(runnable), logDir)
	fmt.Println("----------------------------------------------------------------")

	var wg sync.WaitGroup
	for _, p := range runnable {
		wg.Add(1)
		go func(p *nodePlan) {
			defer wg.Done()
			err := envssh.Stream(ctx, p.ip+":22", "root", login.Signer, p.host.Public,
				runAs(p.runUser, p.workdir, p.run),
				func(s, line string) { printer.Print(p.name, s, line) })
			if err != nil && ctx.Err() == nil {
				// fail-fast: report the crash, leave the node up for debugging (§5).
				printer.Print(p.name, "err", "process exited: "+err.Error()+" (node left up for GDB/SSH)")
			}
		}(p)
	}
	wg.Wait()
}

// colorEnabled reports whether to colorize output (respects NO_COLOR).
func colorEnabled() bool {
	return os.Getenv("NO_COLOR") == ""
}
