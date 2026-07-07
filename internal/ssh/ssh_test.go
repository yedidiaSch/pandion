// SPDX-License-Identifier: AGPL-3.0-or-later

package ssh

import (
	"testing"

	"github.com/yedidiaSch/pandion/internal/sshkeys"
)

// Offline encoding of spike S1: the pinned-host-key callback accepts the exact
// key and rejects any other (a MITM / wrong-key node). No live server needed —
// we test the verification callback directly.
func TestPinnedCallback_AcceptsPinned_RejectsOther(t *testing.T) {
	host, err := sshkeys.Generate("host")
	if err != nil {
		t.Fatalf("gen host: %v", err)
	}
	attacker, err := sshkeys.Generate("attacker")
	if err != nil {
		t.Fatalf("gen attacker: %v", err)
	}

	cb := pinnedCallback(host.Public)

	if err := cb("node:22", nil, host.Public); err != nil {
		t.Fatalf("pinned key must be accepted, got: %v", err)
	}
	if err := cb("node:22", nil, attacker.Public); err == nil {
		t.Fatal("a different host key MUST be rejected (MITM defeated)")
	}
}
