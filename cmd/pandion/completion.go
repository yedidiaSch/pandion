// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// completionProviders are the values suggested after --provider — the canonical
// names plus the short aliases advertised by P1.3. Kept explicit (not in the
// command table).
var completionProviders = []string{
	"mock", "hetzner", "digitalocean", "do", "vultr", "linode", "akamai", "scaleway", "scw", "lambda",
}

// runCompletion prints a shell completion script for pandion, derived from the
// command registry (P1.2) so the command list and per-command flags can't drift.
// Install with e.g.
//
//	pandion completion bash > /etc/bash_completion.d/pandion
//	pandion completion zsh  > "${fpath[1]}/_pandion"
//	pandion completion fish > ~/.config/fish/completions/pandion.fish
func runCompletion(args []string) {
	shell := ""
	if len(args) > 0 {
		shell = args[0]
	}
	var script, hint string
	switch shell {
	case "bash":
		script, hint = bashCompletion(), "install: pandion completion bash | sudo tee /etc/bash_completion.d/pandion"
	case "zsh":
		script, hint = zshCompletion(), `install: pandion completion zsh > "${fpath[1]}/_pandion"  (then restart zsh)`
	case "fish":
		script, hint = fishCompletion(), "install: pandion completion fish > ~/.config/fish/completions/pandion.fish"
	default:
		fmt.Fprintln(os.Stderr, "usage: pandion completion bash|zsh|fish")
		os.Exit(2)
	}
	fmt.Print(script)
	// Print the install one-liner on stderr — only on a TTY — so redirecting stdout
	// to a file keeps the script clean while an interactive run gets the hint (P3.3).
	if stderrIsTTY() {
		fmt.Fprintln(os.Stderr, "# "+hint)
	}
}

// topLevelNames returns the top-level verbs + aliases, sorted.
func topLevelNames() []string { return registeredCommandNames() }

// perCommandFlags returns, sorted by command, each top-level command that has
// flags paired with its "--flag …" string — the raw material for command-aware
// completion in every shell.
func perCommandFlags() [][2]string {
	var names []string
	for _, c := range commands {
		if c.Parent == "" && len(c.Flags) > 0 {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, n := range names {
		dashed := make([]string, 0, len(commandFlagNames(n)))
		for _, f := range commandFlagNames(n) {
			dashed = append(dashed, "--"+f)
		}
		out = append(out, [2]string{n, strings.Join(dashed, " ")})
	}
	return out
}

func bashCompletion() string {
	cmds := strings.Join(topLevelNames(), " ")
	provs := strings.Join(completionProviders, " ")
	var cases strings.Builder
	for _, cf := range perCommandFlags() {
		cases.WriteString(fmt.Sprintf("      %s) flags=\"$flags %s\" ;;\n", cf[0], cf[1]))
	}
	return `# bash completion for pandion
_pandion() {
  local cur prev cmd flags
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  cmd="${COMP_WORDS[1]}"
  case "$prev" in
    --provider) COMPREPLY=( $(compgen -W "` + provs + `" -- "$cur") ); return ;;
    --profile) COMPREPLY=( $(compgen -W "$(pandion profiles 2>/dev/null | tail -n +2 | awk '{print $1}')" -- "$cur") ); return ;;
    -f|--f|--file|--lock|--workspace|--cluster|--fetch) COMPREPLY=( $(compgen -f -- "$cur") ); return ;;
  esac
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=( $(compgen -W "` + cmds + `" -- "$cur") ); return
  fi
  if [[ "$cur" == -* ]]; then
    flags="--profile --verbose --quiet"
    case "$cmd" in
` + cases.String() + `    esac
    COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
  fi
}
complete -F _pandion pandion
`
}

func zshCompletion() string {
	cmds := strings.Join(topLevelNames(), " ")
	provs := strings.Join(completionProviders, " ")
	var cases strings.Builder
	for _, cf := range perCommandFlags() {
		cases.WriteString(fmt.Sprintf("    %s) compadd %s ;;\n", cf[0], cf[1]))
	}
	return `#compdef pandion
# zsh completion for pandion
_pandion() {
  local -a cmds
  cmds=(` + cmds + `)
  if (( CURRENT == 2 )); then
    _describe 'command' cmds
    return
  fi
  case "${words[CURRENT-1]}" in
    --provider) compadd ` + provs + ` ;;
    --profile) compadd $(pandion profiles 2>/dev/null | tail -n +2 | awk '{print $1}') ;;
    -f|--file|--lock|--workspace|--cluster|--fetch) _files ;;
    *)
      case "${words[2]}" in
` + cases.String() + `      esac ;;
  esac
}
_pandion "$@"
`
}

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# fish completion for pandion\n")
	b.WriteString("complete -c pandion -f -n '__fish_use_subcommand' -a '" +
		strings.Join(topLevelNames(), " ") + "'\n")
	b.WriteString("complete -c pandion -l provider -x -a '" +
		strings.Join(completionProviders, " ") + "'\n")
	b.WriteString("complete -c pandion -l profile -x -a '(pandion profiles 2>/dev/null | tail -n +2 | string split -f1 \" \")'\n")
	for _, c := range commands {
		if c.Parent != "" {
			continue
		}
		for _, f := range c.Flags {
			b.WriteString(fmt.Sprintf("complete -c pandion -n '__fish_seen_subcommand_from %s' -l %s\n", c.Name, f))
		}
	}
	return b.String()
}
