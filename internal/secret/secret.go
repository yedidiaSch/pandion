// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secret stores provider API tokens in the OS keychain (macOS Keychain,
// Linux Secret Service / libsecret, Windows Credential Manager) via go-keyring,
// so a token need not sit in an environment variable (H6).
//
// It is a CONVENIENCE layer: an environment variable always takes precedence
// (see the CLI's provider resolution), and a missing/unavailable keychain is
// never fatal — the env-only flow is unchanged. Tokens are never accepted on the
// command line (argv leaks into history/ps).
package secret

import (
	"errors"

	"github.com/zalando/go-keyring"
)

// service is the keychain "service"/"server" name all Pandion secrets live under.
const service = "pandion"

// key namespaces a provider's token, e.g. "hetzner-token".
func key(provider string) string { return provider + "-token" }

// Set stores (or replaces) the token for a provider in the OS keychain.
func Set(provider, token string) error {
	return keyring.Set(service, key(provider), token)
}

// Get returns the stored token for a provider. A not-found is returned as
// ("", nil) — absence is not an error, so callers can fall through to the env.
func Get(provider string) (string, error) {
	v, err := keyring.Get(service, key(provider))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// Delete removes a provider's token. Deleting an absent token is success.
func Delete(provider string) error {
	err := keyring.Delete(service, key(provider))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
