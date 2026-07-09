// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yedidiaSch/pandion/internal/secret"
	"github.com/yedidiaSch/pandion/internal/userconfig"
)

// cloudProviders is the canonical set (mock is offline and always available).
var cloudProviders = []string{"hetzner", "digitalocean", "vultr", "linode", "scaleway"}

// hasCreds reports whether a token for a provider is available (env or keychain).
func hasCreds(canonical string) bool {
	_, env, ok := providerEnv(canonical)
	if !ok {
		return false
	}
	if strings.TrimSpace(os.Getenv(env)) != "" {
		return true
	}
	if t, err := secret.Get(canonical); err == nil && t != "" {
		return true
	}
	return false
}

// credProviders lists the cloud providers that currently have credentials.
func credProviders() []string {
	var out []string
	for _, p := range cloudProviders {
		if hasCreds(p) {
			out = append(out, p)
		}
	}
	return out
}

// pickProvider is the pure resolution core (dependencies passed in, so it is unit-
// testable). It returns the chosen provider, or ("", true) when the caller should
// interactively prompt, or ("", false) when it cannot resolve and must not prompt.
func pickProvider(explicit, configDefault string, creds []string, tty bool) (provider string, prompt bool) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, false // explicit flag wins (including "mock")
	}
	if configDefault != "" {
		return configDefault, false // the operator's default
	}
	if len(creds) == 1 {
		return creds[0], false // unambiguous: the only provider with credentials
	}
	if tty {
		return "", true // ambiguous or none — ask
	}
	return "", false // non-interactive: caller errors with guidance
}

// resolveProvider turns an (possibly empty) --provider flag into a concrete provider
// using the operator config, available credentials, and — on a terminal — a prompt.
// Returns "" if it cannot resolve without a terminal (the caller should error).
func resolveProvider(explicit string) string {
	cfg, _ := userconfig.Load(envHome())
	creds := credProviders()
	prov, prompt := pickProvider(explicit, cfg.DefaultProvider, creds, isTTY())
	if prov != "" {
		// gently note when we INFERRED from a single credential (config default and
		// explicit flags are expected; inference is the surprising-but-helpful case).
		if explicit == "" && cfg.DefaultProvider == "" && len(creds) == 1 {
			fmt.Fprintf(os.Stderr, "using provider %q (the only one with credentials)\n", prov)
		}
		return prov
	}
	if prompt {
		return promptProvider(creds)
	}
	return ""
}

// resolveProviderOrExit resolves the provider and exits with guidance if it cannot
// (used by commands that always need a provider, e.g. ls/reap).
func resolveProviderOrExit(explicit string) string {
	p := resolveProvider(explicit)
	if p == "" {
		fmt.Fprintln(os.Stderr, "no provider set. Run `pandion init`, or pass --provider=<name>.")
		os.Exit(2)
	}
	return p
}

// promptProvider disambiguates among the credentialed providers on a terminal.
// With no credentials at all, prompting to pick one is pointless — it returns the
// "run `pandion init`" guidance (and ""), letting the caller error cleanly.
func promptProvider(creds []string) string {
	if len(creds) == 0 {
		fmt.Fprintln(os.Stderr, "No cloud credentials found. Run `pandion init` to set one up,")
		fmt.Fprintln(os.Stderr, "or pass --provider=<name> after `pandion login`.")
		return ""
	}
	fmt.Fprintln(os.Stderr, "You have credentials for several providers — which one?")
	for i, p := range creds {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, p)
	}
	fmt.Fprint(os.Stderr, "choice [1]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return creds[0]
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(creds) {
		return creds[n-1]
	}
	for _, p := range creds {
		if strings.EqualFold(line, p) {
			return p
		}
	}
	fmt.Fprintf(os.Stderr, "unrecognized choice %q\n", line)
	return ""
}
