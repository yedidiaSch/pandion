package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/yedidiaSch/pandion/internal/harden"
)

// IDE Tier-2: distributed debug-attach over the overlay.
//
// `pandion debug` generates a VS Code cppdbg *attach* configuration that drives a
// REMOTE gdb through the same host-key-pinned SSH connection Pandion already uses
// (cppdbg pipeTransport), pointed at the node's overlay IP. The GDB/MI protocol
// rides that tunnel — no new port, no gdbserver, no daemon. gdb is already in the
// node toolchain and Pandion logs in as root, so it attaches to the unprivileged
// workload without any node-side change. The result: attach your LOCAL debugger to
// a process on a remote node, across the mesh, with F5.

// pipeTransport is the cppdbg block that runs a remote debugger over a pipe (ssh).
type pipeTransport struct {
	DebuggerPath string   `json:"debuggerPath"`
	PipeProgram  string   `json:"pipeProgram"`
	PipeArgs     []string `json:"pipeArgs"`
	PipeCwd      string   `json:"pipeCwd"`
}

// setupCommand is a gdb/MI command run at debugger startup.
type setupCommand struct {
	Text           string `json:"text"`
	IgnoreFailures bool   `json:"ignoreFailures"`
}

// attachCfg is a VS Code cppdbg "attach" configuration (a single launch.json entry).
type attachCfg struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	Request        string            `json:"request"`
	Program        string            `json:"program"`
	ProcessID      string            `json:"processId"`
	MIMode         string            `json:"MIMode"`
	MiDebuggerPath string            `json:"miDebuggerPath"`
	PipeTransport  pipeTransport     `json:"pipeTransport"`
	SourceFileMap  map[string]string `json:"sourceFileMap"`
	SetupCommands  []setupCommand    `json:"setupCommands"`
}

// pickRemoteProcess is VS Code's built-in remote process picker — it enumerates
// the node's processes over the pinned pipe, so no PID needs to be resolved here.
const pickRemoteProcess = "${command:pickRemoteProcess}"

// remoteGDB is gdb's path on the provisioned node (installed by DefaultToolchain).
const remoteGDB = "/usr/bin/gdb"

// buildAttachConfig assembles the cppdbg attach config for one node. user is the
// remote SSH user (root for the operator's own `debug`; the locked-down debug user
// for a shared grant). addr is the node address to dial (overlay or public);
// keyPath/khPath are the SSH key and pinned known_hosts — identical to `pandion
// ssh`'s posture. gdbPath is the debugger to run on the node. program is the remote
// binary (symbols); processID is a literal PID or the remote picker.
func buildAttachConfig(id, node, user, addr, keyPath, khPath, gdbPath, program, processID string) attachCfg {
	remoteWS := "/home/" + harden.DefaultRunUser + "/workspace"
	return attachCfg{
		Name:           "Pandion attach: " + id + "-" + node,
		Type:           "cppdbg",
		Request:        "attach",
		Program:        program,
		ProcessID:      processID,
		MIMode:         "gdb",
		MiDebuggerPath: gdbPath,
		PipeTransport: pipeTransport{
			DebuggerPath: gdbPath,
			PipeProgram:  "ssh",
			PipeArgs: []string{
				"-i", keyPath,
				"-o", "IdentitiesOnly=yes",
				"-o", "StrictHostKeyChecking=yes",
				"-o", "UserKnownHostsFile=" + khPath,
				"-o", "BatchMode=yes",
				user + "@" + addr,
			},
			PipeCwd: "",
		},
		SourceFileMap: map[string]string{remoteWS: "${workspaceFolder}"},
		SetupCommands: []setupCommand{{Text: "-enable-pretty-printing", IgnoreFailures: true}},
	}
}

// runDebugDispatch routes `debug share|join|unshare` to the collaborative-debug
// handlers (Tier-2 sharing); a bare `debug` is the operator's own attach.
func runDebugDispatch(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "share":
			runDebugShare(args[1:])
			return
		case "join":
			runDebugJoin(args[1:])
			return
		case "unshare":
			runDebugUnshare(args[1:])
			return
		}
	}
	runDebug(args)
}

// runDebug emits a VS Code cppdbg attach config for a node so a LOCAL debugger can
// attach to a remote process over the pinned SSH pipe (Tier-2). Overlay by default.
//
//	pandion debug --id ID [--node NAME] [--public] [--pid N] [--program PATH] [--print]
func runDebug(args []string) {
	fs := flag.NewFlagSet("debug", flag.ExitOnError)
	id := fs.String("id", "", "cluster id (required)")
	node := fs.String("node", "", "node name (default: the first node)")
	public := fs.Bool("public", false, "attach over the node's PUBLIC IP (default: the WireGuard overlay)")
	pid := fs.Int("pid", 0, "attach to this remote PID (default: VS Code's remote process picker)")
	program := fs.String("program", "", "remote path to the executable, for symbols (default: the workspace dir)")
	printOnly := fs.Bool("print", false, "print the launch config and exit (don't touch ./.vscode/launch.json)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pandion debug --id ID [--node NAME] [--public] [--pid N] [--program PATH] [--print]")
		fmt.Fprintln(os.Stderr, "  Attach your LOCAL VS Code debugger to a RUNNING process on a node, over the overlay.")
		fmt.Fprintln(os.Stderr, "  Requires the VS Code C/C++ extension locally; gdb is already on the node.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "debug: --id is required")
		os.Exit(2)
	}
	man, err := loadManifest(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debug: no manifest for %q: %v\n", *id, err)
		os.Exit(3)
	}
	target, ok := pickNode(man.Nodes, *node)
	if !ok {
		fmt.Fprintf(os.Stderr, "debug: node not found in cluster %q\n", *id)
		os.Exit(3)
	}

	// Overlay by default (debug rides the encrypted mesh); --public forces the
	// public IP. This intentionally differs from `pandion code` (public-first).
	addr := target.OverlayIP
	if *public || addr == "" {
		addr = target.IP
	}
	if addr == "" {
		fmt.Fprintln(os.Stderr, "debug: node has no reachable address")
		os.Exit(3)
	}

	keyDir := filepath.Join(envHome(), ".pandion", "keys", *id)
	keyPath := filepath.Join(keyDir, "login_ed25519")
	if _, err := os.Stat(keyPath); err != nil {
		fmt.Fprintf(os.Stderr, "debug: login key not found (%s): %v\n", keyPath, err)
		os.Exit(3)
	}
	khPath := filepath.Join(keyDir, "known_hosts")
	must(writeClusterKnownHosts(khPath, man.Nodes))

	prog := *program
	if prog == "" {
		prog = "/home/" + harden.DefaultRunUser + "/workspace"
	}
	processID := pickRemoteProcess
	if *pid > 0 {
		processID = strconv.Itoa(*pid)
	}

	cfg := buildAttachConfig(*id, target.Name, "root", addr, keyPath, khPath, remoteGDB, prog, processID)

	// Always print the config block and side-write a copy under ~/.pandion/vscode
	// (the hands-off posture of `pandion code`), regardless of the merge outcome.
	blockJSON, _ := json.MarshalIndent(cfg, "", "  ")
	if *printOnly {
		fmt.Println(string(blockJSON))
		printDebugUsage(cfg.Name, addr, *pid)
		return
	}
	sideDir := filepath.Join(envHome(), ".pandion", "vscode")
	must(os.MkdirAll(sideDir, 0o700))
	sidePath := filepath.Join(sideDir, *id+"-"+target.Name+".launch.json")
	must(os.WriteFile(sidePath, blockJSON, 0o600))

	// Merge into the current project's ./.vscode/launch.json so F5 just works.
	launchPath := filepath.Join(".vscode", "launch.json")
	created, droppedComments, merr := mergeLaunchJSON(launchPath, cfg)
	switch {
	case merr != nil:
		// Never clobber an unparseable launch.json — fall back to manual paste.
		fmt.Printf("could not safely merge %s (%v)\n", launchPath, merr)
		fmt.Printf("wrote the config to %s — paste it into your launch.json \"configurations\".\n", sidePath)
	case created:
		fmt.Printf("created %s with %q\n", launchPath, cfg.Name)
	default:
		fmt.Printf("merged %q into %s\n", cfg.Name, launchPath)
		if droppedComments {
			fmt.Println("note: comments in your launch.json were dropped on rewrite.")
		}
	}
	fmt.Printf("(copy also at %s)\n", sidePath)
	printDebugUsage(cfg.Name, addr, *pid)
}

// printDebugUsage prints the next steps for attaching from VS Code.
func printDebugUsage(name, addr string, pid int) {
	fmt.Println("\n# Attach from VS Code (needs the C/C++ extension):")
	fmt.Printf("#   Run and Debug (Ctrl+Shift+D) → \"%s\" → F5\n", name)
	if pid == 0 {
		fmt.Printf("#   then pick the remote process on %s from the list.\n", addr)
	} else {
		fmt.Printf("#   it attaches to PID %d on %s.\n", pid, addr)
	}
	fmt.Println("# The debug session rides the pinned SSH pipe (over the overlay unless --public).")
}

// mergeLaunchJSON merges cfg into a VS Code launch.json at path. It creates the
// file (and .vscode dir) if absent, otherwise parses the existing file tolerating
// // and /* */ comments (JSONC) and replaces any config with the same name (else
// appends), preserving every other config. Returns whether it created the file and
// whether the source had comments (dropped on rewrite). An unparseable file is left
// untouched and returned as an error, so the caller can fall back to manual paste.
func mergeLaunchJSON(path string, cfg any) (created, droppedComments bool, err error) {
	cfgMap, err := toMap(cfg)
	if err != nil {
		return false, false, err
	}
	cfgName, _ := cfgMap["name"].(string)

	raw, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, false, err
		}
		doc := map[string]any{"version": "0.2.0", "configurations": []any{cfgMap}}
		return true, false, writeJSON(path, doc)
	}
	if rerr != nil {
		return false, false, rerr
	}

	stripped, hadComments := stripJSONComments(raw)
	var doc map[string]any
	if err := json.Unmarshal(stripped, &doc); err != nil {
		return false, false, fmt.Errorf("not valid JSON/JSONC: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	if _, ok := doc["version"]; !ok {
		doc["version"] = "0.2.0"
	}
	var configs []any
	if existing, ok := doc["configurations"].([]any); ok {
		for _, c := range existing {
			if m, ok := c.(map[string]any); ok {
				if n, _ := m["name"].(string); n == cfgName {
					continue // replace our own previous entry
				}
			}
			configs = append(configs, c)
		}
	}
	configs = append(configs, cfgMap)
	doc["configurations"] = configs
	return false, hadComments, writeJSON(path, doc)
}

// toMap round-trips a value through JSON into a generic map (for merging).
func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// writeJSON marshals doc as indented JSON (trailing newline) to path, 0600.
func writeJSON(path string, doc any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// stripJSONComments removes // line and /* */ block comments from JSONC input,
// respecting string literals (and their escapes) so a "//" inside a string is
// preserved. Returns the stripped bytes and whether any comment was removed.
func stripJSONComments(b []byte) (out []byte, had bool) {
	res := make([]byte, 0, len(b))
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			res = append(res, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			res = append(res, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			had = true
			for i < len(b) && b[i] != '\n' {
				i++
			}
			if i < len(b) {
				res = append(res, '\n')
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			had = true
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i++ // land on '/', loop's i++ steps past it
		default:
			res = append(res, c)
		}
	}
	return res, had
}
