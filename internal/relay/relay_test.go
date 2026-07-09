// SPDX-License-Identifier: AGPL-3.0-or-later

package relay

import (
	"strings"
	"testing"
	"time"
)

func TestNewToken_UniqueHighEntropyPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, TokenPrefix) {
			t.Fatalf("token missing prefix: %q", tok)
		}
		body := strings.TrimPrefix(tok, TokenPrefix)
		if len(body) < 40 { // 32 bytes base64url ≈ 43 chars
			t.Fatalf("token body too short (low entropy): %q", body)
		}
		if seen[tok] {
			t.Fatal("duplicate token generated")
		}
		seen[tok] = true
	}
}

func newSession(id, tok string, expiry time.Time) *Session {
	return &Session{
		ID: id, Token: tok, ClusterID: "c1", Node: "victim", Target: "10.99.0.2",
		HostPub: "ssh-ed25519 AAAA host", User: "pandion-lab",
		SSHKeyPEM: "-----BEGIN KEY-----\nx\n-----END KEY-----", Expiry: expiry,
	}
}

func TestStore_PutGetExpiryDeleteReap(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	tok, _ := NewToken()
	live := newSession("s1", tok, now.Add(time.Hour))
	if err := st.Put(live); err != nil {
		t.Fatal(err)
	}

	// a valid token resolves to the session (with the scoped key server-side).
	got, ok := st.Get(tok, now)
	if !ok || got.ID != "s1" || got.SSHKeyPEM == "" || got.Target != "10.99.0.2" {
		t.Fatalf("Get(valid) = %+v, ok=%v", got, ok)
	}

	// an unknown token is absent (no oracle).
	if _, ok := st.Get("PRLY1-nope", now); ok {
		t.Fatal("unknown token must not resolve")
	}

	// an expired session is treated as absent AND swept from disk.
	etok, _ := NewToken()
	exp := newSession("s2", etok, now.Add(-time.Minute))
	if err := st.Put(exp); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get(etok, now); ok {
		t.Fatal("expired token must not resolve")
	}
	if list, _ := st.List(); len(list) != 1 { // s2 swept on Get; only s1 remains
		t.Fatalf("expired session should be swept on Get, list=%d", len(list))
	}

	// delete-by-id revokes.
	found, err := st.Delete("s1")
	if err != nil || !found {
		t.Fatalf("Delete(s1) found=%v err=%v", found, err)
	}
	if _, ok := st.Get(tok, now); ok {
		t.Fatal("token must not resolve after revoke")
	}
	if found, _ := st.Delete("ghost"); found {
		t.Fatal("deleting an unknown id must report not-found")
	}
}

func TestStore_Reap(t *testing.T) {
	st, _ := OpenStore(t.TempDir())
	now := time.Now().UTC()
	for i, d := range []time.Duration{time.Hour, -time.Hour, -2 * time.Hour} {
		tok, _ := NewToken()
		st.Put(newSession(string(rune('a'+i)), tok, now.Add(d)))
	}
	n, err := st.Reap(now)
	if err != nil || n != 2 {
		t.Fatalf("Reap removed %d (want 2), err=%v", n, err)
	}
	if list, _ := st.List(); len(list) != 1 {
		t.Fatalf("after reap, %d sessions remain (want 1)", len(list))
	}
}
