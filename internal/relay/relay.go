// SPDX-License-Identifier: AGPL-3.0-or-later

// Package relay is the model behind Pandion's browser-SSH relay: scoped, expiring,
// revocable grants that let a participant reach ONE node's shell over the overlay
// from a browser, with no install. A Session pairs an opaque bearer token (the URL
// secret the participant holds) with the server-side material needed to open a
// host-key-pinned SSH PTY to the target node — the scoped SSH key never leaves the
// relay. The Store persists sessions as 0600 JSON on the relay node so the CLI
// (writing over SSH) and the relay server (reading) share state.
//
// This package is pure and offline (no network, no SSH) so the grant lifecycle is
// unit-testable everywhere; the server and CLI wire it to real SSH/WebSocket.
package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TokenPrefix tags relay tokens (versioned, so the format can evolve).
const TokenPrefix = "PRLY1-"

// tokenBytes is the entropy of a token (256-bit) — infeasible to guess/brute-force.
const tokenBytes = 32

// Session is a single scoped browser-SSH grant. The relay holds SSHKeyPEM
// server-side; the participant holds only Token (in the URL).
type Session struct {
	ID        string    `json:"id"`         // share id (stable, for revoke/status)
	Token     string    `json:"token"`      // opaque bearer capability (URL secret)
	ClusterID string    `json:"cluster_id"` // owning cluster
	Node      string    `json:"node"`       // target node name (display)
	Target    string    `json:"target"`     // target overlay IP, e.g. 10.99.0.2
	HostPub   string    `json:"host_pub"`   // target's pinned authorized_keys host line
	User      string    `json:"user"`       // scoped, NON-root user on the target
	SSHKeyPEM string    `json:"ssh_key"`    // scoped private key (reaches only User@Target)
	Expiry    time.Time `json:"expiry"`     // hard expiry (UTC)
}

// Expired reports whether the grant is past its expiry as of now.
func (s *Session) Expired(now time.Time) bool { return now.After(s.Expiry) }

// NewToken returns a high-entropy, URL-safe opaque token.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenKey maps a token to its storage key (a hash), so the token itself is never a
// filename and lookups are constant-work by hashing the presented token — no
// directory scan, no timing oracle on the secret.
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SpoolFilename is the file a session is stored under for a given token — so the
// CLI (writing a session to the relay node over SSH) and the server (reading it)
// agree without either exposing the token as a filename.
func SpoolFilename(token string) string { return tokenKey(token) + ".json" }

// Store persists sessions as one 0600 JSON file per token (named by tokenKey) under
// a 0700 spool dir on the relay node.
type Store struct{ dir string }

// OpenStore opens (creating if needed) a session spool at dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (st *Store) path(token string) string {
	return filepath.Join(st.dir, tokenKey(token)+".json")
}

// Put writes (or replaces) a session.
func (st *Store) Put(s *Session) error {
	if s.Token == "" || s.ID == "" {
		return fmt.Errorf("session needs a token and id")
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(st.path(s.Token), b, 0o600)
}

// Get returns the session for a presented token, or (nil, false) if it is absent,
// unreadable, or EXPIRED (expired sessions are treated as absent and swept).
func (st *Store) Get(token string, now time.Time) (*Session, bool) {
	b, err := os.ReadFile(st.path(token))
	if err != nil {
		return nil, false
	}
	var s Session
	if json.Unmarshal(b, &s) != nil {
		return nil, false
	}
	if s.Expired(now) {
		_ = os.Remove(st.path(token))
		return nil, false
	}
	return &s, true
}

// List returns all stored sessions (including expired — callers filter as needed).
func (st *Store) List() ([]*Session, error) {
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(st.dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(b, &s) == nil {
			out = append(out, &s)
		}
	}
	return out, nil
}

// Delete removes the session with the given share id (revocation). Returns whether
// a matching session was found.
func (st *Store) Delete(id string) (bool, error) {
	sessions, err := st.List()
	if err != nil {
		return false, err
	}
	found := false
	for _, s := range sessions {
		if s.ID == id {
			if rmErr := os.Remove(st.path(s.Token)); rmErr == nil {
				found = true
			}
		}
	}
	return found, nil
}

// Reap removes every expired session and returns how many it removed.
func (st *Store) Reap(now time.Time) (int, error) {
	sessions, err := st.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sessions {
		if s.Expired(now) {
			if os.Remove(st.path(s.Token)) == nil {
				n++
			}
		}
	}
	return n, nil
}
