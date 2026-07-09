// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yedidiaSch/pandion/internal/secret"
	"github.com/yedidiaSch/pandion/internal/userconfig"
)

// runInit is the first-run wizard: pick a default provider, log in if needed, and
// write ~/.pandion/config.yaml so bare one-liners (e.g. `pandion up -- ./app`) work
// with no flags. Interactive on a terminal; fully scriptable with flags
// (--provider/--token/--region) for automation.
//
//	pandion init [--provider NAME] [--token TOK] [--region R] [--force]
func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	provider := fs.String("provider", "", "default cloud provider (hetzner|digitalocean|vultr|linode|scaleway|mock)")
	token := fs.String("token", "", "API token to store (else prompted on a terminal)")
	region := fs.String("region", "", "default region (optional)")
	force := fs.Bool("force", false, "overwrite an existing config without asking")
	_ = fs.Parse(args)

	home := envHome()
	cfg, _ := userconfig.Load(home)
	if cfg.DefaultProvider != "" && !*force && *provider == "" {
		fmt.Printf("Pandion is already configured (default provider: %s).\n", cfg.DefaultProvider)
		if !isTTY() || !yesNo("Reconfigure?", false) {
			fmt.Println("nothing to do. (use --force to overwrite non-interactively)")
			return
		}
	}

	// 1) choose the provider.
	prov := strings.TrimSpace(*provider)
	if prov == "" {
		if !isTTY() {
			fmt.Fprintln(os.Stderr, "init: non-interactive — pass --provider=<name>")
			os.Exit(2)
		}
		prov = chooseProvider()
	}
	canonical, env, ok := providerEnv(prov)
	if prov == "mock" {
		canonical, ok = "mock", true // offline provider, no credentials
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "init: unknown provider %q\n", prov)
		os.Exit(2)
	}

	// 2) credentials (skip for mock).
	if canonical != "mock" && !hasCreds(canonical) {
		fmt.Printf("\nNo %s credentials found yet.\n", canonical)
		printSignupSuggestion(canonical)
		tok := strings.TrimSpace(*token)
		if tok == "" {
			if !isTTY() {
				fmt.Fprintln(os.Stderr, "init: no credentials — pass --token or export "+env)
				os.Exit(2)
			}
			tok = readToken(env) // no-echo prompt / stdin
		}
		if tok == "" {
			fmt.Fprintln(os.Stderr, "init: no token provided")
			os.Exit(2)
		}
		if err := secret.Set(canonical, tok); err != nil {
			fmt.Fprintf(os.Stderr, "init: could not store the token in the OS keychain: %v\n", err)
			fmt.Fprintf(os.Stderr, "  keep using the env var instead: export %s=…\n", env)
		} else {
			fmt.Printf("stored the %s token in your OS keychain.\n", canonical)
		}
		if canonical == "scaleway" {
			fmt.Println("note: Scaleway also needs SCW_ACCESS_KEY and SCW_DEFAULT_PROJECT_ID in your environment.")
		}
	}

	// 3) optional region.
	reg := strings.TrimSpace(*region)
	if reg == "" && isTTY() && canonical != "mock" {
		reg = promptLine("default region (optional, press Enter to skip): ")
	}

	// 4) write the config.
	cfg.DefaultProvider = canonical
	if reg != "" {
		cfg.Defaults.Region = reg
	}
	if err := userconfig.Save(home, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "init: could not write config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✔ configured: default provider = %s", canonical)
	if reg != "" {
		fmt.Printf(", region = %s", reg)
	}
	fmt.Printf("  (%s)\n", userconfig.Path(home))
	fmt.Println("\nTry it:")
	if canonical == "mock" {
		fmt.Println("  pandion demo                      # full lifecycle, offline, zero cost")
	} else {
		fmt.Println("  pandion up -- 'echo hello from the cloud && uname -a'")
		fmt.Println("  pandion down --id demo            # tear it down")
	}
}

// chooseProvider prompts for a provider on a terminal.
func chooseProvider() string {
	opts := append(append([]string{}, cloudProviders...), "mock")
	fmt.Println("Choose your default cloud provider:")
	for i, p := range opts {
		note := ""
		if p == "mock" {
			note = "  (offline, for trying Pandion at zero cost)"
		} else if hasCreds(p) {
			note = "  (credentials found)"
		}
		fmt.Printf("  %d) %s%s\n", i+1, p, note)
	}
	for {
		fmt.Print("choice [1]: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return opts[0]
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(opts) {
			return opts[n-1]
		}
		for _, p := range opts {
			if strings.EqualFold(line, p) {
				return p
			}
		}
		fmt.Println("please pick a number or name.")
	}
}

func promptLine(prompt string) string {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func yesNo(prompt string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	fmt.Printf("%s [%s]: ", prompt, d)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}
