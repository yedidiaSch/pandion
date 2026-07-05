package secret

import (
	"testing"

	"github.com/zalando/go-keyring"
)

// MockInit swaps in an in-memory keychain so the store logic is testable
// everywhere (no real keyring daemon needed — CI is headless).
func TestSetGetDelete(t *testing.T) {
	keyring.MockInit()

	// absent -> ("", nil), not an error
	if v, err := Get("hetzner"); err != nil || v != "" {
		t.Fatalf("absent Get = (%q, %v), want (\"\", nil)", v, err)
	}

	if err := Set("hetzner", "tok-abc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, err := Get("hetzner"); err != nil || v != "tok-abc" {
		t.Fatalf("Get after Set = (%q, %v)", v, err)
	}

	// providers are namespaced independently
	if err := Set("digitalocean", "tok-do"); err != nil {
		t.Fatalf("set do: %v", err)
	}
	if v, _ := Get("hetzner"); v != "tok-abc" {
		t.Fatalf("hetzner token clobbered: %q", v)
	}
	if v, _ := Get("digitalocean"); v != "tok-do" {
		t.Fatalf("do token = %q", v)
	}

	// delete, then it's absent again; deleting again is still success
	if err := Delete("hetzner"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if v, err := Get("hetzner"); err != nil || v != "" {
		t.Fatalf("after delete Get = (%q, %v)", v, err)
	}
	if err := Delete("hetzner"); err != nil {
		t.Fatalf("deleting an absent token must be success, got %v", err)
	}
}
