// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yedidiaSch/pandion/internal/config"
	"github.com/yedidiaSch/pandion/internal/secret"
	"github.com/yedidiaSch/pandion/internal/userconfig"
)

//go:embed templates/cluster.starter.yaml
var clusterStarterTemplate []byte

// runInit is the first-run wizard: pick a default provider, log in if needed, and
// write ~/.pandion/config.yaml so bare one-liners (e.g. `pandion up -- ./app`) work
// with no flags. Interactive on a terminal; fully scriptable with flags
// (--provider/--token/--region) for automation.
//
//	pandion init [--provider NAME] [--token TOK] [--region R] [--size S] [--ttl D] [--force]
func runInit(args []string) {
	fs := newCmdFlagSet("init")
	provider := fs.String("provider", "", "default cloud provider (hetzner|digitalocean|vultr|linode|scaleway|mock)")
	token := fs.String("token", "", "API token to store (else prompted on a terminal)")
	region := fs.String("region", "", "default region (optional)")
	size := fs.String("size", "", "default server size/type, e.g. cpx21 (optional)")
	ttl := fs.String("ttl", "", "default idle-poweroff TTL, e.g. 2h (optional)")
	force := fs.Bool("force", false, "overwrite an existing config without asking")
	cluster := fs.Bool("cluster", false, "instead of the operator config, scaffold a starter cluster.yaml (path: optional arg, default ./cluster.yaml)")
	_ = fs.Parse(args)

	if *cluster {
		path := "cluster.yaml"
		if fs.NArg() > 0 {
			path = fs.Arg(0)
		}
		scaffoldCluster(path, *force)
		return
	}

	home := envHome()
	cfg, _ := userconfig.LoadProfile(home, activeProfile)
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
		if err := secret.Set(credName(canonical), tok); err != nil {
			fmt.Fprintf(os.Stderr, "init: could not store the token in the OS keychain: %v\n", err)
			fmt.Fprintf(os.Stderr, "  keep using the env var instead: export %s=…\n", env)
		} else {
			fmt.Printf("stored the %s token in your OS keychain.\n", canonical)
		}
		if canonical == "scaleway" {
			fmt.Println("note: Scaleway also needs SCW_ACCESS_KEY and SCW_DEFAULT_PROJECT_ID in your environment.")
		}
	}

	// 3) optional defaults: region, size, ttl (used to fill `up` when a flag is omitted).
	reg := strings.TrimSpace(*region)
	siz := strings.TrimSpace(*size)
	tl := strings.TrimSpace(*ttl)
	if isTTY() && canonical != "mock" {
		if reg == "" {
			reg = promptLine("default region (optional, press Enter to skip): ")
		}
		if siz == "" {
			siz = promptLine("default server size/type, e.g. cpx21 (optional, Enter to auto-select): ")
		}
		if tl == "" {
			tl = promptLine("default idle-poweroff TTL, e.g. 2h (optional, Enter to skip): ")
		}
	}
	if tl != "" {
		if _, err := time.ParseDuration(tl); err != nil {
			fmt.Fprintf(os.Stderr, "init: invalid --ttl %q (want a Go duration like 90m or 2h): %v\n", tl, err)
			os.Exit(2)
		}
	}

	// 4) write the config.
	cfg.DefaultProvider = canonical
	if reg != "" {
		cfg.Defaults.Region = reg
	}
	if siz != "" {
		cfg.Defaults.Size = siz
	}
	if tl != "" {
		cfg.Defaults.TTL = tl
	}
	if err := userconfig.SaveProfile(home, activeProfile, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "init: could not write config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✔ configured%s: default provider = %s", profileLabel(), canonical)
	if reg != "" {
		fmt.Printf(", region = %s", reg)
	}
	if siz != "" {
		fmt.Printf(", size = %s", siz)
	}
	if tl != "" {
		fmt.Printf(", ttl = %s", tl)
	}
	fmt.Printf("  (%s)\n", userconfig.PathFor(home, activeProfile))
	fmt.Println("\nTry it:")
	if canonical == "mock" {
		fmt.Println("  pandion demo                      # full lifecycle, offline, zero cost")
	} else {
		fmt.Println("  pandion up -- 'echo hello from the cloud && uname -a'")
		fmt.Println("  pandion down --id demo            # tear it down")
	}
}

// scaffoldCluster writes the embedded starter cluster.yaml to path (P2.3). It
// refuses to clobber an existing file without --force, then validates what it wrote
// (a broken template is a bug) and prints the next steps.
func scaffoldCluster(path string, force bool) {
	if _, err := os.Stat(path); err == nil && !force {
		fmt.Fprintf(os.Stderr, "init --cluster: %s already exists (use --force to overwrite)\n", path)
		os.Exit(2)
	}
	if err := os.WriteFile(path, clusterStarterTemplate, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "init --cluster: could not write %s: %v\n", path, err)
		os.Exit(1)
	}
	// self-check: the template we ship must pass validation.
	if _, err := config.Load(path); err != nil {
		fmt.Fprintf(os.Stderr, "init --cluster: wrote %s but it failed validation: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("✔ wrote a starter topology: %s\n", path)
	fmt.Println("\nNext:")
	fmt.Printf("  1) edit %s (nodes, run commands, sync/build)\n", path)
	fmt.Printf("  2) pandion validate -f %s\n", path)
	fmt.Printf("  3) pandion up -f %s\n", path)
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
