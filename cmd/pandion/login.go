// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/yedidiaSch/pandion/internal/secret"
	"golang.org/x/term"
)

// providerEnv maps a provider name (incl. the `do` alias) to its canonical name
// and the environment variable that holds its token.
func providerEnv(provider string) (name, env string, ok bool) {
	switch provider {
	case "hetzner":
		return "hetzner", "HCLOUD_TOKEN", true
	case "digitalocean", "do":
		return "digitalocean", "DIGITALOCEAN_TOKEN", true
	case "vultr":
		return "vultr", "VULTR_API_KEY", true
	case "linode", "akamai":
		return "linode", "LINODE_TOKEN", true
	case "scaleway", "scw":
		// only the secret key is stored in the keychain; SCW_ACCESS_KEY and
		// SCW_DEFAULT_PROJECT_ID are non-secret identifiers kept in the environment.
		return "scaleway", "SCW_SECRET_KEY", true
	}
	return "", "", false
}

// providerToken resolves a provider's API token: the environment variable first
// (for scripting/CI — unchanged behavior), then the OS keychain (`pandion login`).
// A keychain error is ignored (headless hosts fall back to env-only).
func providerToken(provider, env string) (string, error) {
	if t := strings.TrimSpace(os.Getenv(env)); t != "" {
		return t, nil
	}
	if t, err := secret.Get(provider); err == nil && t != "" {
		return t, nil
	}
	base := fmt.Errorf("%s not set and no stored token — set the env var or run `pandion login --provider %s`", env, provider)
	// If the user likely has no account yet, offer a signup pointer (a disclosed
	// referral link when one is configured — see referral.go).
	if s := signupSuggestion(provider, resolveDORefcode()); s != "" {
		return "", fmt.Errorf("%w\n%s", base, s)
	}
	return "", base
}

// readToken obtains a token WITHOUT it appearing in argv/history: the env var if
// set (so `export HCLOUD_TOKEN=…; pandion login` just moves it to the keychain),
// else a no-echo TTY prompt, else a single line from stdin (`… | pandion login`).
func readToken(env string) string {
	if t := strings.TrimSpace(os.Getenv(env)); t != "" {
		return t
	}
	if isTTY() {
		fmt.Fprintf(os.Stderr, "Paste %s (input hidden): ", env)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

// runLogin stores a provider's API token in the OS keychain, so it need not sit
// in an environment variable (H6). The token is never taken from argv.
func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	provider := fs.String("provider", "hetzner", "provider: hetzner|digitalocean|vultr|linode|scaleway")
	_ = fs.Parse(args)
	name, env, ok := providerEnv(*provider)
	if !ok {
		fmt.Fprintf(os.Stderr, "login: unknown provider %q (use hetzner|digitalocean|vultr|linode|scaleway)\n", *provider)
		os.Exit(2)
	}
	token := readToken(env)
	if token == "" {
		fmt.Fprintln(os.Stderr, "login: no token provided")
		printSignupSuggestion(name)
		os.Exit(2)
	}
	if err := secret.Set(name, token); err != nil {
		fmt.Fprintf(os.Stderr, "login: could not store the token in the OS keychain: %v\n", err)
		fmt.Fprintf(os.Stderr, "  (no keyring available here? keep using the env var: export %s=…)\n", env)
		os.Exit(3)
	}
	fmt.Printf("stored %s token in the OS keychain — pandion will use it automatically.\n", name)
}

// runLogout removes a provider's token from the OS keychain.
func runLogout(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	provider := fs.String("provider", "hetzner", "provider: hetzner|digitalocean|vultr|linode|scaleway")
	_ = fs.Parse(args)
	name, _, ok := providerEnv(*provider)
	if !ok {
		fmt.Fprintf(os.Stderr, "logout: unknown provider %q (use hetzner|digitalocean|vultr|linode|scaleway)\n", *provider)
		os.Exit(2)
	}
	if err := secret.Delete(name); err != nil {
		fmt.Fprintf(os.Stderr, "logout: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("removed %s token from the OS keychain.\n", name)
}
