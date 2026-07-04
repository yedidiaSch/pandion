package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// knownHostsLine renders a pinned known_hosts entry for addr from a stored
// authorized-keys line ("ssh-ed25519 AAAA... comment") — keytype + key only.
// Returns "" if the stored key is malformed.
func knownHostsLine(addr, hostPub string) string {
	f := strings.Fields(hostPub)
	if len(f) < 2 {
		return ""
	}
	return fmt.Sprintf("%s %s %s\n", addr, f[0], f[1])
}

// runSSH opens an SSH session (or runs a command) on a cluster node using the
// persisted manifest + login key, with the host key PINNED (StrictHostKeyChecking
// against a temp known_hosts) — the same MITM-proof posture as the rest of
// Pandion. This is the supported way to reach a node "left up for GDB/SSH" (§5).
//
//	pandion ssh --id ID [--node NAME] [--overlay] [-- <command>]
func runSSH(args []string) {
	flagArgs, cmd := splitRunCmd(args)
	fs := flag.NewFlagSet("ssh", flag.ExitOnError)
	id := fs.String("id", "", "cluster id (required)")
	node := fs.String("node", "", "node name (default: the first node)")
	useOverlay := fs.Bool("overlay", false, "connect over the WireGuard overlay IP (requires the overlay joined)")
	_ = fs.Parse(flagArgs)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "ssh: --id is required")
		os.Exit(2)
	}

	man, err := loadManifest(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: no manifest for %q (is the id correct?): %v\n", *id, err)
		os.Exit(3)
	}
	target, ok := pickNode(man.Nodes, *node)
	if !ok {
		if *node == "" {
			fmt.Fprintf(os.Stderr, "ssh: cluster %q has no nodes\n", *id)
		} else {
			fmt.Fprintf(os.Stderr, "ssh: node %q not found in cluster %q\n", *node, *id)
		}
		os.Exit(3)
	}

	addr := target.IP
	if *useOverlay {
		addr = target.OverlayIP
	}
	if addr == "" {
		fmt.Fprintln(os.Stderr, "ssh: node has no reachable address (try/omit --overlay)")
		os.Exit(3)
	}

	keyPath := filepath.Join(envHome(), ".pandion", "keys", *id, "login_ed25519")
	if _, err := os.Stat(keyPath); err != nil {
		fmt.Fprintf(os.Stderr, "ssh: login key not found (%s): %v\n", keyPath, err)
		os.Exit(3)
	}

	// pin the node's host key via a throwaway known_hosts (MITM-proof).
	kh, err := os.CreateTemp("", "pandion-known-hosts-*")
	must(err)
	defer os.Remove(kh.Name())
	if _, err := kh.WriteString(knownHostsLine(addr, target.HostPub)); err != nil {
		must(err)
	}
	kh.Close()

	sshArgs := []string{
		"-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + kh.Name(),
		"root@" + addr,
	}
	if cmd != "" {
		sshArgs = append(sshArgs, cmd)
	}

	c := exec.Command("ssh", sshArgs...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode()) // propagate the remote/ssh exit code
		}
		fmt.Fprintf(os.Stderr, "ssh: %v (is the openssh client installed?)\n", err)
		os.Exit(1)
	}
}

// pickNode returns the named node (or the first, if name is empty).
func pickNode(nodes []nodeManifest, name string) (nodeManifest, bool) {
	if name == "" {
		if len(nodes) == 0 {
			return nodeManifest{}, false
		}
		return nodes[0], true
	}
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return nodeManifest{}, false
}
