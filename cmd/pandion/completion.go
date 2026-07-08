// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"strings"
)

// completionCommands is the subcommand list offered by shell completion. Kept
// here (not derived from the main switch) so completion is explicit and stable.
var completionCommands = []string{
	"up", "down", "ls", "status", "validate",
	"lockdown", "reap", "attach", "demo", "version", "completion",
}

// completionProviders are the values suggested after --provider.
var completionProviders = []string{"mock", "hetzner", "digitalocean"}

// runCompletion prints a shell completion script for pandion. Install with e.g.
//
//	pandion completion bash > /etc/bash_completion.d/pandion
//	pandion completion zsh  > "${fpath[1]}/_pandion"
//	pandion completion fish > ~/.config/fish/completions/pandion.fish
func runCompletion(args []string) {
	shell := ""
	if len(args) > 0 {
		shell = args[0]
	}
	switch shell {
	case "bash":
		fmt.Print(bashCompletion())
	case "zsh":
		fmt.Print(zshCompletion())
	case "fish":
		fmt.Print(fishCompletion())
	default:
		fmt.Fprintln(os.Stderr, "usage: pandion completion bash|zsh|fish")
		os.Exit(2)
	}
}

func bashCompletion() string {
	cmds := strings.Join(completionCommands, " ")
	provs := strings.Join(completionProviders, " ")
	return `# bash completion for pandion
_pandion() {
  local cur prev
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  case "$prev" in
    --provider) COMPREPLY=( $(compgen -W "` + provs + `" -- "$cur") ); return ;;
    -f|--f|--lock|--workspace) COMPREPLY=( $(compgen -f -- "$cur") ); return ;;
  esac
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=( $(compgen -W "` + cmds + `" -- "$cur") ); return
  fi
  if [[ "$cur" == -* ]]; then
    COMPREPLY=( $(compgen -W "--provider --id --node --dry-run --lock --max-cost --ttl --no-ttl -f --json --yes --older-than" -- "$cur") )
  fi
}
complete -F _pandion pandion
`
}

func zshCompletion() string {
	cmds := strings.Join(completionCommands, " ")
	provs := strings.Join(completionProviders, " ")
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
    -f|--lock|--workspace) _files ;;
    *) _arguments '--provider[cloud backend]' '--id[cluster id]' '--node[node name]' \
         '--dry-run[preview only]' '--lock[reproducibility lockfile]' '--max-cost[budget cap]' \
         '--ttl[idle poweroff]' '--no-ttl[disable ttl]' '-f[cluster.yaml]' '--json[machine-readable]' ;;
  esac
}
_pandion "$@"
`
}

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# fish completion for pandion\n")
	// subcommands (only when no subcommand yet)
	b.WriteString("complete -c pandion -f -n '__fish_use_subcommand' -a '" +
		strings.Join(completionCommands, " ") + "'\n")
	// --provider values
	b.WriteString("complete -c pandion -l provider -x -a '" +
		strings.Join(completionProviders, " ") + "'\n")
	for _, f := range []struct{ name, desc string }{
		{"id", "cluster id"}, {"node", "node name"}, {"dry-run", "preview only"},
		{"lock", "reproducibility lockfile"}, {"max-cost", "budget cap"},
		{"ttl", "idle poweroff"}, {"no-ttl", "disable ttl"}, {"json", "machine-readable"},
	} {
		b.WriteString(fmt.Sprintf("complete -c pandion -l %s -d '%s'\n", f.name, f.desc))
	}
	return b.String()
}
