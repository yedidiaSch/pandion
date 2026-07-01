package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/envcore/envcore/internal/config"
	"github.com/envcore/envcore/internal/discovery"
	"github.com/envcore/envcore/internal/firewall"
	"github.com/envcore/envcore/internal/harden"
	"github.com/envcore/envcore/internal/orchestrator"
	"github.com/envcore/envcore/internal/overlay"
	envssh "github.com/envcore/envcore/internal/ssh"
	"github.com/envcore/envcore/internal/sshkeys"
)

// nodePlan holds the locally-generated identity for one cluster node.
type nodePlan struct {
	name      string
	host      *sshkeys.KeyPair // per-node SSH host key (pinned)
	wg        overlay.Keypair  // per-node WireGuard key
	overlayIP string           // e.g. 10.99.0.1
	run       string
	ip        string // public IP, filled after provisioning
}

const operatorOverlayIP = "10.99.0.254"

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
		plans[i] = &nodePlan{name: n.Name, host: host, wg: wg, overlayIP: oip, run: n.Run}

		ci := harden.CloudInit{
			HostPrivKeyPEM: host.PrivatePEM,
			HostPubKey:     host.PublicAuthorized,
			LoginPubKey:    login.PublicAuthorized,
			Packages:       append(harden.DefaultToolchain(), "nftables", "wireguard"),
			WGConfig:       overlay.InterfaceConfig(wg.Private, oip+"/24", overlay.DefaultPort),
		}
		specs[i] = orchestrator.NodeSpec{
			Name: n.Name, UserData: harden.Build(ci), LoginPubKey: login.PublicAuthorized,
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

	// run each node's command with discovery in scope (login shell sources the
	// profile.d file). NOTE: M3.3 runs commands synchronously — long-running
	// services are the domain of M3.4's multiplexed streaming.
	fmt.Println("running per-node commands...")
	for _, p := range plans {
		if strings.TrimSpace(p.run) == "" {
			continue
		}
		out, rerr := envssh.Run(ctx, p.ip+":22", "root", login.Signer, p.host.Public,
			"bash -lc "+shellQuote(p.run))
		fmt.Printf("---- %s: %s ----\n%s\n", p.name, p.run, trimSpace(out))
		if rerr != nil {
			fmt.Printf("  (%s exited with error: %v — node left running for debugging)\n", p.name, rerr)
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

	fmt.Println("----------------------------------------------------------------")
	if meshOK {
		fmt.Printf("cluster %q UP: %d nodes hardened + WireGuard mesh verified.\n", id, len(plans))
	} else {
		fmt.Printf("cluster %q UP but mesh verification had failures (see above).\n", id)
	}
	fmt.Printf("operator overlay config: %s\n", confPath)
	fmt.Printf("  join: sudo wg-quick up %s   (reach nodes at 10.99.0.1..%d)\n", confPath, len(plans))
	fmt.Printf("teardown: envcore down --provider=hetzner --id %s\n", id)
	fmt.Println("note: commands ran synchronously; multiplexed streaming of long-running services lands in M3.4.")
}
