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

	conn, cleanup := dialInfo("ssh", *id, target, *useOverlay)
	defer cleanup()

	sshArgs := append(conn.opts(), "root@"+conn.addr)
	if cmd != "" {
		sshArgs = append(sshArgs, cmd)
	}
	runClient("ssh", sshArgs)
}

// connInfo holds the pinned-connection parameters shared by ssh + cp.
type connInfo struct {
	addr    string
	keyPath string
	khPath  string
}

// opts returns the common openssh options (identity + host-key pinning).
func (c connInfo) opts() []string {
	return []string{
		"-i", c.keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + c.khPath,
	}
}

// dialInfo resolves the node address, the persisted login key, and a throwaway
// known_hosts pinning the node's host key. cmdName is used only for error prefixes.
func dialInfo(cmdName, id string, target nodeManifest, useOverlay bool) (connInfo, func()) {
	addr := target.IP
	if useOverlay {
		addr = target.OverlayIP
	}
	if addr == "" {
		fmt.Fprintf(os.Stderr, "%s: node has no reachable address (try/omit --overlay)\n", cmdName)
		os.Exit(3)
	}
	keyPath := filepath.Join(envHome(), ".pandion", "keys", id, "login_ed25519")
	if _, err := os.Stat(keyPath); err != nil {
		fmt.Fprintf(os.Stderr, "%s: login key not found (%s): %v\n", cmdName, keyPath, err)
		os.Exit(3)
	}
	kh, err := os.CreateTemp("", "pandion-known-hosts-*")
	must(err)
	if _, err := kh.WriteString(knownHostsLine(addr, target.HostPub)); err != nil {
		must(err)
	}
	kh.Close()
	return connInfo{addr: addr, keyPath: keyPath, khPath: kh.Name()}, func() { os.Remove(kh.Name()) }
}

// runClient execs an openssh-family client (ssh/scp), wiring stdio through and
// propagating its exit code.
func runClient(bin string, args []string) {
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "%s: %v (is the openssh client installed?)\n", bin, err)
		os.Exit(1)
	}
}

// runCP copies files to/from a node with scp, host-key pinned. A path prefixed
// with ":" is on the node (e.g. ":/var/log/pandion/run.log").
//
//	pandion cp --id ID [--node NAME] [--overlay] SRC DST
func runCP(args []string) {
	fs := flag.NewFlagSet("cp", flag.ExitOnError)
	id := fs.String("id", "", "cluster id (required)")
	node := fs.String("node", "", "node name (default: the first node)")
	useOverlay := fs.Bool("overlay", false, "use the WireGuard overlay IP")
	_ = fs.Parse(args)
	rest := fs.Args()
	if *id == "" || len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: pandion cp --id ID [--node NAME] SRC DST   (prefix a node path with ':')")
		os.Exit(2)
	}
	src, dst := rest[0], rest[1]
	if strings.HasPrefix(src, ":") == strings.HasPrefix(dst, ":") {
		fmt.Fprintln(os.Stderr, "cp: exactly one of SRC/DST must be a node path (prefixed with ':')")
		os.Exit(2)
	}

	man, err := loadManifest(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cp: no manifest for %q: %v\n", *id, err)
		os.Exit(3)
	}
	target, ok := pickNode(man.Nodes, *node)
	if !ok {
		fmt.Fprintf(os.Stderr, "cp: node not found in cluster %q\n", *id)
		os.Exit(3)
	}
	conn, cleanup := dialInfo("cp", *id, target, *useOverlay)
	defer cleanup()

	runClient("scp", append(conn.opts(), "-p", scpEndpoint(conn.addr, src), scpEndpoint(conn.addr, dst)))
}

// scpEndpoint rewrites a ":"-prefixed node path to scp's root@addr:path form;
// a local path is returned unchanged.
func scpEndpoint(addr, p string) string {
	if strings.HasPrefix(p, ":") {
		return "root@" + addr + ":" + p[1:]
	}
	return p
}

// sshConfigBlock renders a host-key-pinned SSH config entry for a node, suitable
// for VS Code Remote-SSH / JetBrains Gateway (or plain `ssh <alias>`).
func sshConfigBlock(alias, addr, keyPath, khPath string) string {
	return fmt.Sprintf(`Host %s
    HostName %s
    User root
    IdentityFile %s
    IdentitiesOnly yes
    StrictHostKeyChecking yes
    UserKnownHostsFile %s
`, alias, addr, keyPath, khPath)
}

// writeClusterKnownHosts writes a persistent, pinned known_hosts for every node
// in the cluster (public + overlay addresses), so the generated SSH config keeps
// the same MITM-proof posture as `pandion ssh`.
func writeClusterKnownHosts(path string, nodes []nodeManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, n := range nodes {
		if n.IP != "" {
			b.WriteString(knownHostsLine(n.IP, n.HostPub))
		}
		if n.OverlayIP != "" {
			b.WriteString(knownHostsLine(n.OverlayIP, n.HostPub))
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// runCode emits a host-key-pinned SSH config entry for a node so an IDE (VS Code
// Remote-SSH, JetBrains Gateway) can attach over SSH — the ethos-aligned way to
// edit/debug a node "left up for GDB/SSH". No backend, keys stay local.
//
//	pandion code --id ID [--node NAME] [--overlay] [--print]
func runCode(args []string) {
	fs := flag.NewFlagSet("code", flag.ExitOnError)
	id := fs.String("id", "", "cluster id (required)")
	node := fs.String("node", "", "node name (default: the first node)")
	useOverlay := fs.Bool("overlay", false, "use the WireGuard overlay IP")
	printOnly := fs.Bool("print", false, "print the SSH config block and exit (don't write files)")
	_ = fs.Parse(args)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "code: --id is required")
		os.Exit(2)
	}
	man, err := loadManifest(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "code: no manifest for %q: %v\n", *id, err)
		os.Exit(3)
	}
	target, ok := pickNode(man.Nodes, *node)
	if !ok {
		fmt.Fprintf(os.Stderr, "code: node not found in cluster %q\n", *id)
		os.Exit(3)
	}
	addr := target.IP
	if *useOverlay {
		addr = target.OverlayIP
	}
	if addr == "" {
		fmt.Fprintln(os.Stderr, "code: node has no reachable address (try/omit --overlay)")
		os.Exit(3)
	}
	keyDir := filepath.Join(envHome(), ".pandion", "keys", *id)
	keyPath := filepath.Join(keyDir, "login_ed25519")
	if _, err := os.Stat(keyPath); err != nil {
		fmt.Fprintf(os.Stderr, "code: login key not found (%s): %v\n", keyPath, err)
		os.Exit(3)
	}
	khPath := filepath.Join(keyDir, "known_hosts")
	must(writeClusterKnownHosts(khPath, man.Nodes))

	alias := "pandion-" + *id + "-" + target.Name
	block := sshConfigBlock(alias, addr, keyPath, khPath)

	printUsage := func() {
		fmt.Println("\n# One-time: add this line to ~/.ssh/config")
		fmt.Println("#   Include ~/.pandion/ssh/*.config")
		fmt.Println("# Then, to open the node in your IDE:")
		fmt.Printf("#   VS Code  → Remote-SSH: Connect to Host… → %s\n", alias)
		fmt.Printf("#   or:  code --remote ssh-remote+%s /root/workspace\n", alias)
		fmt.Printf("#   or:  ssh %s\n", alias)
	}

	if *printOnly {
		fmt.Print(block)
		printUsage()
		return
	}
	cfgDir := filepath.Join(envHome(), ".pandion", "ssh")
	must(os.MkdirAll(cfgDir, 0o700))
	cfgPath := filepath.Join(cfgDir, *id+"-"+target.Name+".config")
	must(os.WriteFile(cfgPath, []byte(block), 0o600))
	fmt.Printf("wrote pinned SSH config: %s  (Host %s)\n", cfgPath, alias)
	printUsage()
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
