// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
	"time"
)

func TestPackToken_RoundTrips(t *testing.T) {
	orig := shareBundle{
		Version: 1, ClusterID: "pipeline", Node: "worker", ShareID: "abc123",
		Expiry: "2026-07-05T23:00:00Z", User: debugUser, NodeOverlayIP: "10.99.0.2",
		Program: "app", WGConfig: "[Interface]\nAddress = 10.99.0.252/32\n",
		SSHKeyPEM: "-----BEGIN-----\nx\n-----END-----\n", KnownHosts: "10.99.0.2 ssh-ed25519 AAAA\n",
	}
	tok, err := packToken(orig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, tokenPrefix) {
		t.Fatalf("token missing prefix: %q", tok[:8])
	}
	got, err := unpackToken(tok)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, orig)
	}
}

func TestUnpackToken_RejectsGarbage(t *testing.T) {
	if _, err := unpackToken("not-a-token"); err == nil {
		t.Fatal("expected error for non-prefixed token")
	}
	if _, err := unpackToken(tokenPrefix + "!!!not-base64!!!"); err == nil {
		t.Fatal("expected error for bad base64")
	}
}

func TestProvisionScript_ScopedGdbserver(t *testing.T) {
	exp := time.Date(2026, 7, 5, 23, 0, 0, 0, time.UTC)
	s := provisionScript("ssh-ed25519 AAAAguest comment", exp, "sid42", "GUESTwgpub==", "10.99.0.251", 4242)

	// non-root debug user.
	if !strings.Contains(s, "useradd -m -s /bin/bash pandion-debug") {
		t.Error("debug user not created for ForceCommand execution")
	}
	// gdbserver available + a locked sudoers rule (reachable only via the wrapper).
	if !strings.Contains(s, "command -v gdbserver") {
		t.Error("gdbserver install guard missing")
	}
	if !strings.Contains(s, "pandion-debug ALL=(root) NOPASSWD: /usr/bin/gdbserver") {
		t.Error("sudoers rule for gdbserver missing")
	}
	// per-share pinned target PID (trusted; the guest cannot choose it).
	if !strings.Contains(s, "echo 4242 > /etc/pandion/shares/sid42") {
		t.Error("per-share pinned PID not written")
	}
	// authorized_keys deduped by marker; scoped WG peer.
	if !strings.Contains(s, "sed -i '/pandion-share-sid42/d'") {
		t.Error("authorized_keys not deduped by share marker")
	}
	if !strings.Contains(s, "wg set wg0 peer GUESTwgpub== allowed-ips 10.99.0.251/32") {
		t.Error("scoped WG peer command missing")
	}
}

func TestAuthorizedKeysLine_ForcesWrapperWithShareID(t *testing.T) {
	exp := time.Date(2026, 7, 5, 23, 0, 0, 0, time.UTC)
	line := authorizedKeysLine("ssh-ed25519 AAAAguest", exp, "sid42")
	for _, want := range []string{
		"restrict",
		`expiry-time="20260705230000"`,
		`command="/usr/local/bin/pandion-debug-forced sid42"`, // wrapper bound to this share
		"ssh-ed25519 AAAAguest",
		"pandion-share-sid42",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("authorized_keys line missing %q: %s", want, line)
		}
	}
}

func TestRevokeScript_RemovesKeyPIDAndPeer(t *testing.T) {
	s := revokeScript(shareRecord{ShareID: "sid42", GuestWGPub: "GUESTwgpub=="})
	for _, want := range []string{
		"sed -i '/pandion-share-sid42/d'",     // key line
		"rm -f /etc/pandion/shares/sid42",     // pinned PID
		"wg set wg0 peer GUESTwgpub== remove", // WG peer
	} {
		if !strings.Contains(s, want) {
			t.Errorf("revoke script missing %q", want)
		}
	}
}

func TestForcedWrapper_GdbserverScoped(t *testing.T) {
	// the wrapper runs gdbserver in stdio mode, bound to the per-share pinned PID,
	// and refuses system/root targets — no shell, no arg injection.
	for _, want := range []string{
		"/etc/pandion/shares/$sid",                           // reads the trusted per-share PID
		`uid" -lt 1000`,                                      // refuses system/root targets
		"refusing to debug system/root process",              // the refusal message
		"exec sudo -n /usr/bin/gdbserver --once --attach - ", // gdbserver stdio as root
	} {
		if !strings.Contains(forcedWrapper, want) {
			t.Errorf("wrapper missing %q", want)
		}
	}
	// the client's requested command is never execed (only $1 = trusted share id).
	if strings.Contains(forcedWrapper, "SSH_ORIGINAL_COMMAND") {
		t.Error("wrapper must ignore the client-supplied command")
	}
}

func TestBuildServerAttachConfig_PinnedTargetRemotePipe(t *testing.T) {
	b := shareBundle{ClusterID: "c", Node: "n", User: debugUser, NodeOverlayIP: "10.99.0.2", Program: "app"}
	cfg := buildServerAttachConfig(b, "/k/id_ed25519", "/k/known_hosts")

	if cfg["type"] != "cppdbg" || cfg["MIMode"] != "gdb" {
		t.Fatalf("wrong debugger identity: %+v", cfg)
	}
	cmds, ok := cfg["customLaunchSetupCommands"].([]map[string]any)
	if !ok || len(cmds) != 1 {
		t.Fatalf("expected one customLaunchSetupCommand, got %+v", cfg["customLaunchSetupCommands"])
	}
	text, _ := cmds[0]["text"].(string)
	for _, want := range []string{
		"target remote | ssh",
		"-i /k/id_ed25519",
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/k/known_hosts",
		"pandion-debug@10.99.0.2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("target-remote pipe missing %q: %s", want, text)
		}
	}
}

func TestAllocGuestIP_Descends(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ip, err := allocGuestIP("nope-no-shares")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.99.0.252" {
		t.Fatalf("first guest IP = %q, want 10.99.0.252", ip)
	}
}
