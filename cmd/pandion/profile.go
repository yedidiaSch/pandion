// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/yedidiaSch/pandion/internal/userconfig"
)

// activeProfile is the operator profile selected for this invocation (empty = the
// default profile). It is a cross-cutting selector — set once in main() from the
// global `--profile` flag or $PANDION_PROFILE — so every command reads the right
// ~/.pandion config and the right keychain namespace.
var activeProfile string

var profileNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// initProfile extracts the global `--profile NAME` / `--profile=NAME` selector
// from a command's args (falling back to $PANDION_PROFILE), validates it, sets
// activeProfile, and returns the args with the selector removed so the per-command
// flag sets never see it.
// logVerbose/logQuiet are the global verbosity knobs (P1.6), set by the pre-parse
// below from --verbose/--quiet. --verbose tees the audit stream to stderr (like
// PANDION_LOG=debug); --quiet silences progress chatter on stdout (status* helpers)
// while leaving results and stderr errors intact. Flags win over the env var.
var (
	logVerbose bool
	logQuiet   bool
)

func initProfile(args []string) []string {
	out := make([]string, 0, len(args))
	prof := strings.TrimSpace(os.Getenv("PANDION_PROFILE"))
	for i := 0; i < len(args); i++ {
		a := args[i]
		// stop stripping at the run-command separator so `up … -- cmd --quiet` keeps
		// its own args intact.
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		switch {
		case a == "--profile" || a == "-profile":
			if i+1 < len(args) {
				prof = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--profile=") || strings.HasPrefix(a, "-profile="):
			prof = a[strings.IndexByte(a, '=')+1:]
		case a == "--verbose" || a == "-verbose" || a == "-v":
			logVerbose = true
		case a == "--quiet" || a == "-quiet" || a == "-q":
			logQuiet = true
		default:
			out = append(out, a)
		}
	}
	prof = strings.TrimSpace(prof)
	if prof != "" && !profileNameRe.MatchString(prof) {
		fmt.Fprintf(os.Stderr, "invalid --profile %q (use letters, digits, '-' or '_')\n", prof)
		os.Exit(2)
	}
	activeProfile = prof
	return out
}

// credName namespaces a provider's keychain entry by the active profile, so
// `--profile work` and `--profile personal` can hold different accounts for the
// same provider. The default profile keeps the bare provider name (back-compat:
// existing `pandion login` tokens keep working unchanged).
func credName(canonical string) string {
	if activeProfile == "" {
		return canonical
	}
	return activeProfile + "@" + canonical
}

// profileLabel is a short human suffix for messages, e.g. " (profile: work)".
func profileLabel() string {
	if activeProfile == "" {
		return ""
	}
	return fmt.Sprintf(" (profile: %s)", activeProfile)
}

// runProfiles lists the operator's profiles and marks the active one.
func runProfiles(args []string) {
	home := envHome()
	names, err := userconfig.List(home)
	must(err)
	star := func(p string) string {
		if p == activeProfile {
			return " *"
		}
		return ""
	}
	fmt.Println("profiles (default profile is ~/.pandion/config.yaml; * = active):")
	fmt.Printf("  default%s\n", star(""))
	for _, n := range names {
		fmt.Printf("  %s%s\n", n, star(n))
	}
	if len(names) == 0 {
		fmt.Println("  (no named profiles yet — create one with `pandion init --profile <name>`)")
	}
}
