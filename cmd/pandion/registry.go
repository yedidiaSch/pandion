// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// command is one CLI verb. The single source of truth (P1.2): dispatch, top-level
// usage(), per-command `-h`, and shell completion are all derived from this table,
// so the three lists that used to drift are now one. Subcommands (e.g. `relay up`)
// carry a Parent so they stay out of the top-level list but still get real help.
type command struct {
	Name     string
	Aliases  []string            // alternative verbs that dispatch the same handler (e.g. status→ls)
	Parent   string              // non-empty for subcommands (relay/debug); hidden from top-level usage
	Synopsis string              // one-line description
	Args     string              // argument/flag sketch shown after the verb in usage
	Example  string              // one runnable example
	Flags    []string            // long flag names (no dashes) this command accepts — drives completion
	Handler  func(args []string) // nil for pure command groups whose subcommands do the work
}

// commands is the registry. Order here is the order shown in `pandion help`. It is
// populated in init() (not a var initializer) so the handler function values it
// holds don't create a package initialization cycle with the registry helpers they
// transitively reach (newCmdFlagSet → printCommandHelp → lookupCommand → commands).
var commands []command

// commandIndex maps every top-level verb + alias to its command; built in init().
var commandIndex map[string]*command

func init() {
	commands = []command{
		{Name: "init", Synopsis: "set up a default provider + credentials so bare commands work",
			Args: "[--cluster [PATH]]", Example: "pandion init", Flags: []string{"cluster", "force"}, Handler: runInit},
		{Name: "up", Synopsis: "provision a hardened node/cluster and run a command on it",
			Args:    "[--provider N] [--id ID] [--node NAME] [--size TYPE] [--region R] [--gpu M[:N]] [--dry-run] [--no-run] [-f cluster.yaml] -- <run cmd>",
			Example: "pandion up --provider hetzner --id demo -- ./run.sh",
			Flags:   []string{"provider", "id", "node", "size", "region", "gpu", "gpu-idle-util", "ttl", "no-ttl", "dry-run", "json", "no-run", "no-firewall", "firewall-audit", "no-overlay", "no-toolchain", "packages", "setup", "egress-allow", "workspace", "remote-path", "build", "sync-mode", "run-as", "max-cost", "lock", "encrypt-workspace", "engine", "container-image", "cap-add", "f", "file"}, Handler: runUp},
		{Name: "build", Synopsis: "auto-detect the toolchain, upload + build the project in the cloud",
			Args: "[dir] [up-flags…] [-- <run cmd>]", Example: "pandion build . --provider hetzner", Flags: []string{"provider", "id", "size", "region", "dry-run"}, Handler: runBuild},
		{Name: "down", Synopsis: "tear a cluster down and reconcile the provider to empty",
			Args: "[--provider N] [--id ID] [--dry-run] [--yes] [--json]", Example: "pandion down --id demo", Flags: []string{"provider", "id", "dry-run", "yes", "json"}, Handler: runDown},
		{Name: "ls", Aliases: []string{"status"}, Synopsis: "list live clusters with uptime + cost",
			Args: "[--provider N] [--json] [--gpu-util]", Example: "pandion ls --json", Flags: []string{"provider", "json", "gpu-util"}, Handler: runLs},
		{Name: "start", Synopsis: "launch the run commands on an already-deployed cluster/node",
			Args: "--id ID [--node NAME] [--detach]", Example: "pandion start --id demo", Flags: []string{"id", "node", "detach"}, Handler: runStart},
		{Name: "attach", Synopsis: "reconnect to a running cluster's multiplexed streams",
			Args: "--id ID", Example: "pandion attach --id demo", Flags: []string{"id"}, Handler: runAttach},
		{Name: "ssh", Synopsis: "SSH into a node (host-key pinned)",
			Args: "--id ID [--node NAME] [--overlay|--public] [-- CMD]", Example: "pandion ssh --id demo -- uptime", Flags: []string{"id", "node", "overlay", "public"}, Handler: runSSH},
		{Name: "cp", Synopsis: "scp to/from a node (prefix a node path with ':')",
			Args: "--id ID [--node NAME] SRC DST", Example: "pandion cp --id demo ./a :/tmp/a", Flags: []string{"id", "node", "overlay", "public"}, Handler: runCP},
		{Name: "code", Synopsis: "print a pinned SSH config for VS Code Remote-SSH",
			Args: "--id ID [--node NAME] [--print]", Example: "pandion code --id demo --print", Flags: []string{"id", "node", "overlay", "public", "print"}, Handler: runCode},
		{Name: "debug", Synopsis: "remote-debug tools (attach/share/join/unshare)",
			Args: "<subcommand> …", Example: "pandion debug --id demo --pid 1234", Flags: []string{"id", "node", "overlay", "public", "pid", "print"}, Handler: runDebugDispatch},
		{Name: "relay", Synopsis: "browser-SSH relay (up/status/share/unshare/recordings)",
			Args: "<subcommand> …", Example: "pandion relay up --id demo", Handler: runRelayDispatch},
		{Name: "validate", Synopsis: "validate a cluster.yaml against the schema",
			Args: "[-f cluster.yaml] [--show-effective]", Example: "pandion validate -f cluster.yaml", Flags: []string{"f", "file", "show-effective"}, Handler: runValidate},
		{Name: "lockdown", Synopsis: "public deny-all firewall; SSH over the overlay only",
			Args: "--id ID [--audit]", Example: "pandion lockdown --id demo", Flags: []string{"id", "audit"}, Handler: runLockdown},
		{Name: "reap", Synopsis: "destroy orphaned Pandion nodes at the provider",
			Args: "[--older-than DUR] [--yes] [--json]", Example: "pandion reap --older-than 2h --yes", Flags: []string{"provider", "older-than", "yes", "json"}, Handler: runReap},
		{Name: "doctor", Synopsis: "report where local state diverges from the provider (stale/leaked)",
			Args: "", Example: "pandion doctor", Handler: runDoctor},
		{Name: "login", Synopsis: "store a provider API token in the OS keychain",
			Args: "[--provider N]", Example: "pandion login --provider hetzner", Flags: []string{"provider"}, Handler: runLogin},
		{Name: "logout", Synopsis: "remove a stored provider API token",
			Args: "[--provider N]", Example: "pandion logout --provider hetzner", Flags: []string{"provider"}, Handler: runLogout},
		{Name: "list-gpus", Synopsis: "list the GPU SKUs a provider can serve, priced",
			Args: "[--provider N] [--json]", Example: "pandion list-gpus --json", Flags: []string{"provider", "json", "refresh"}, Handler: runListGPUs},
		{Name: "profiles", Synopsis: "list configured operator profiles (* = active)",
			Args: "", Example: "pandion profiles", Handler: runProfiles},
		{Name: "completion", Synopsis: "print a shell completion script",
			Args: "bash|zsh|fish", Example: "pandion completion bash", Handler: runCompletion},
		{Name: "demo", Synopsis: "run the offline mock end-to-end demo",
			Args: "", Example: "pandion demo", Handler: func([]string) { runDemo() }},
		{Name: "version", Synopsis: "print the pandion version",
			Args: "", Example: "pandion version", Handler: func([]string) { fmt.Println("pandion", version) }},

		// Subcommands — hidden from the top-level list, but they get real `-h` help.
		{Name: "relay up", Parent: "relay", Synopsis: "deploy the browser-SSH relay on a node", Args: "--id ID [--node NAME] [--port 8443] [--domain DNS]", Example: "pandion relay up --id demo", Flags: []string{"id", "node", "port", "domain", "relay-binary"}},
		{Name: "relay status", Parent: "relay", Synopsis: "list live relay grants for a cluster", Args: "--id ID", Example: "pandion relay status --id demo", Flags: []string{"id"}},
		{Name: "relay share", Parent: "relay", Synopsis: "grant a scoped, expiring browser-SSH link", Args: "--id ID --node NAME [--expires 4h] [--read-only] [--record]", Example: "pandion relay share --id demo --node node-a", Flags: []string{"id", "node", "expires", "user", "read-only", "record"}},
		{Name: "relay unshare", Parent: "relay", Synopsis: "revoke a relay grant", Args: "--id ID [--share SID | --all]", Example: "pandion relay unshare --id demo --all", Flags: []string{"id", "share", "all"}},
		{Name: "relay recordings", Parent: "relay", Synopsis: "list/fetch recorded relay sessions", Args: "--id ID [--fetch DIR]", Example: "pandion relay recordings --id demo", Flags: []string{"id", "fetch"}},
		{Name: "debug share", Parent: "debug", Synopsis: "grant a teammate a scoped, expiring remote-debug token", Args: "--id ID [--node NAME] [--expires 2h]", Example: "pandion debug share --id demo", Flags: []string{"id", "node", "expires"}},
		{Name: "debug join", Parent: "debug", Synopsis: "accept a shared debug grant", Args: "<token>", Example: "pandion debug join PDBG1-…"},
		{Name: "debug unshare", Parent: "debug", Synopsis: "revoke a shared debug grant", Args: "--id ID [--share SID | --all]", Example: "pandion debug unshare --id demo --all", Flags: []string{"id", "share", "all"}},
	}

	// index every top-level verb + alias (subcommands dispatch internally).
	commandIndex = map[string]*command{}
	for i := range commands {
		c := &commands[i]
		if c.Parent != "" {
			continue
		}
		commandIndex[c.Name] = c
		for _, a := range c.Aliases {
			commandIndex[a] = c
		}
	}
}

// lookupCommand returns the registry entry for a full command name (incl.
// subcommands like "relay up"), or nil.
func lookupCommand(name string) *command {
	for i := range commands {
		if commands[i].Name == name {
			return &commands[i]
		}
		for _, a := range commands[i].Aliases {
			if a == name {
				return &commands[i]
			}
		}
	}
	return nil
}

// dispatch runs the command named by argv0 (already stripped of the program name
// and global flags). It returns false if the verb is unknown. help/version are
// handled by the caller before this.
func dispatch(verb string, args []string) bool {
	c, ok := commandIndex[verb]
	if !ok || c.Handler == nil {
		return false
	}
	c.Handler(args)
	return true
}

// newCmdFlagSet builds a FlagSet whose `-h`/`--help` prints a real synopsis +
// example + flag defaults to STDOUT and exits 0 (P1.1), driven by the registry.
// Every command handler uses this instead of flag.NewFlagSet so per-command help
// is uniform and can't drift from the command table.
func newCmdFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() {
		printCommandHelp(name)
		if hasFlags(fs) {
			fmt.Println("\nflags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

func hasFlags(fs *flag.FlagSet) bool {
	any := false
	fs.VisitAll(func(*flag.Flag) { any = true })
	return any
}

// printCommandHelp writes the synopsis + usage line + example for one command to
// stdout. Falls back gracefully for a name with no registry entry.
func printCommandHelp(name string) {
	c := lookupCommand(name)
	if c == nil {
		fmt.Printf("pandion %s\n", name)
		return
	}
	fmt.Printf("%s\n\n", c.Synopsis)
	line := "pandion " + c.Name
	if c.Args != "" {
		line += " " + c.Args
	}
	fmt.Printf("usage: %s\n", line)
	if c.Example != "" {
		fmt.Printf("example: %s\n", c.Example)
	}
}

// usage prints the top-level command list to stdout, generated from the registry
// (P1.2 — no more hand-maintained block). It is used for `pandion help` and, on
// stderr via usageErr, for an unknown/no verb.
func usage() { writeUsage(os.Stdout) }

func usageErr() { writeUsage(os.Stderr) }

func writeUsage(w *os.File) {
	fmt.Fprintln(w, "pandion — provision hardened, ephemeral cloud dev/CI nodes.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "usage: pandion [--profile NAME] [--verbose] [--quiet] <command> [flags]")
	fmt.Fprintln(w, "  global: --profile NAME ($PANDION_PROFILE) · --verbose (audit trail to stderr; = PANDION_LOG=debug) · --quiet (results + errors only)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	width := 0
	for _, c := range commands {
		if c.Parent == "" && len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range commands {
		if c.Parent != "" {
			continue
		}
		name := c.Name
		if len(c.Aliases) > 0 {
			name = c.Name + "|" + strings.Join(c.Aliases, "|")
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width+8, name, c.Synopsis)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "run 'pandion <command> -h' for a command's flags and an example.")
}

// registeredCommandNames returns every top-level verb + alias, sorted — used by
// completion and the drift test.
func registeredCommandNames() []string {
	var out []string
	for _, c := range commands {
		if c.Parent != "" {
			continue
		}
		out = append(out, c.Name)
		out = append(out, c.Aliases...)
	}
	sort.Strings(out)
	return out
}

// commandFlagNames returns the long flag names a top-level command accepts (no
// dashes), for command-aware completion.
func commandFlagNames(name string) []string {
	if c := commandIndex[name]; c != nil {
		return c.Flags
	}
	return nil
}
