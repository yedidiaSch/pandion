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

// Path is where the config lives (home is the value of envHome — usually $HOME).
func Path(home string) string {
	return filepath.Join(home, ".pandion", "config.yaml")
}

// Load reads the config, returning an empty (non-nil) Config if the file is absent.
func Load(home string) (*Config, error) {
	b, err := os.ReadFile(Path(home))
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

// Save writes the config (creating ~/.pandion if needed) with 0600 perms — it may
// name a provider but never holds secrets (tokens live in the OS keychain).
func Save(home string, c *Config) error {
	dir := filepath.Join(home, ".pandion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# Pandion operator defaults. Edit freely; tokens are NOT stored here\n" +
		"# (they live in your OS keychain via `pandion login`).\n"
	return os.WriteFile(Path(home), append([]byte(header), b...), 0o600)
}
