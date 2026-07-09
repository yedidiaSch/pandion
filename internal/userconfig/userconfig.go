// SPDX-License-Identifier: AGPL-3.0-or-later

// Package userconfig loads and saves the operator's personal defaults from
// ~/.pandion/config.yaml — the layer that lets bare one-liners work without flags
// (e.g. a default provider). It sits between environment variables and built-in
// defaults in Pandion's resolution order:
//
//	flag > env > cluster.yaml > ~/.pandion/config.yaml > (infer / prompt) > built-in
//
// A missing file is not an error — it yields an empty Config.
package userconfig

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the operator's personal defaults. All fields are optional.
type Config struct {
	// DefaultProvider is used by `up` (and friends) when --provider is not given.
	DefaultProvider string `yaml:"default_provider,omitempty"`
	// Defaults seed per-node settings when a topology/flag does not specify them.
	Defaults Defaults `yaml:"defaults,omitempty"`
}

// Defaults are optional per-node defaults.
type Defaults struct {
	Region string `yaml:"region,omitempty"`
	Size   string `yaml:"size,omitempty"`
	TTL    string `yaml:"ttl,omitempty"`
}

// Path is where the default profile's config lives (home is the value of
// envHome — usually $HOME).
func Path(home string) string { return PathFor(home, "") }

// PathFor is Path for a named profile: the default profile ("") is
// ~/.pandion/config.yaml; a named profile is ~/.pandion/profiles/<name>.yaml,
// so `--profile work` and `--profile personal` keep separate defaults.
func PathFor(home, profile string) string {
	if profile == "" {
		return filepath.Join(home, ".pandion", "config.yaml")
	}
	return filepath.Join(home, ".pandion", "profiles", profile+".yaml")
}

// Load reads the default profile's config (empty, non-nil, if absent).
func Load(home string) (*Config, error) { return LoadProfile(home, "") }

// LoadProfile reads a named profile's config, returning an empty (non-nil)
// Config if the file is absent.
func LoadProfile(home, profile string) (*Config, error) {
	b, err := os.ReadFile(PathFor(home, profile))
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the default profile's config.
func Save(home string, c *Config) error { return SaveProfile(home, "", c) }

// SaveProfile writes a named profile's config (creating the parent dir if
// needed) with 0600 perms — it may name a provider but never holds secrets
// (tokens live in the OS keychain).
func SaveProfile(home, profile string, c *Config) error {
	path := PathFor(home, profile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# Pandion operator defaults. Edit freely; tokens are NOT stored here\n" +
		"# (they live in your OS keychain via `pandion login`).\n"
	return os.WriteFile(path, append([]byte(header), b...), 0o600)
}

// List returns the names of the named profiles under ~/.pandion/profiles (the
// default profile is not included). A missing dir yields an empty list.
func List(home string) ([]string, error) {
	dir := filepath.Join(home, ".pandion", "profiles")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".yaml") {
			continue
		}
		out = append(out, strings.TrimSuffix(n, ".yaml"))
	}
	return out, nil
}
