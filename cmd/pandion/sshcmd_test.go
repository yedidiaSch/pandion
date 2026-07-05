package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestKnownHostsLine(t *testing.T) {
	got := knownHostsLine("203.0.113.7", "ssh-ed25519 AAAAC3NzaC1lZDI1 pandion-host-worker")
	want := "203.0.113.7 ssh-ed25519 AAAAC3NzaC1lZDI1\n" // comment stripped
	if got != want {
		t.Fatalf("knownHostsLine = %q, want %q", got, want)
	}
	if knownHostsLine("1.2.3.4", "garbage") != "" {
		t.Error("malformed host key should yield an empty line")
	}
}

func TestPickNode(t *testing.T) {
	nodes := []nodeManifest{
		{Name: "broker", IP: "1.1.1.1"},
		{Name: "worker", IP: "2.2.2.2"},
	}
	if n, ok := pickNode(nodes, ""); !ok || n.Name != "broker" {
		t.Fatalf("empty name should pick the first node, got %+v ok=%v", n, ok)
	}
	if n, ok := pickNode(nodes, "worker"); !ok || n.IP != "2.2.2.2" {
		t.Fatalf("named pick wrong: %+v ok=%v", n, ok)
	}
	if _, ok := pickNode(nodes, "nope"); ok {
		t.Error("unknown node should not be found")
	}
	if _, ok := pickNode(nil, ""); ok {
		t.Error("no nodes should not be found")
	}
}

func TestScpEndpoint(t *testing.T) {
	if got := scpEndpoint("1.2.3.4", ":/var/log/pandion/run.log"); got != "root@1.2.3.4:/var/log/pandion/run.log" {
		t.Fatalf("remote path = %q", got)
	}
	if got := scpEndpoint("1.2.3.4", "./local.txt"); got != "./local.txt" {
		t.Fatalf("local path should be unchanged, got %q", got)
	}
}

func TestSSHConfigBlock(t *testing.T) {
	got := sshConfigBlock("pandion-pipeline-broker", "203.0.113.7", "/home/u/.pandion/keys/pipeline/login_ed25519", "/home/u/.pandion/keys/pipeline/known_hosts")
	for _, want := range []string{
		"Host pandion-pipeline-broker",
		"HostName 203.0.113.7",
		"User root",
		"IdentityFile /home/u/.pandion/keys/pipeline/login_ed25519",
		"IdentitiesOnly yes",
		"StrictHostKeyChecking yes", // pinned — MITM-proof, like `pandion ssh`
		"UserKnownHostsFile /home/u/.pandion/keys/pipeline/known_hosts",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh config block missing %q:\n%s", want, got)
		}
	}
}

func TestWriteClusterKnownHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "known_hosts") // dir doesn't exist yet
	nodes := []nodeManifest{
		{Name: "broker", IP: "203.0.113.7", OverlayIP: "10.99.0.1", HostPub: "ssh-ed25519 AAAAbroker c"},
		{Name: "worker", IP: "203.0.113.8", HostPub: "ssh-ed25519 AAAAworker c"},
	}
	if err := writeClusterKnownHosts(path, nodes); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(b)
	// both public IPs + the overlay IP get pinned lines (keytype+key, comment stripped)
	for _, want := range []string{
		"203.0.113.7 ssh-ed25519 AAAAbroker",
		"10.99.0.1 ssh-ed25519 AAAAbroker",
		"203.0.113.8 ssh-ed25519 AAAAworker",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("known_hosts missing %q:\n%s", want, out)
		}
	}
	if runtime.GOOS != "windows" { // POSIX perms don't apply on Windows
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("known_hosts perms = %o, want 0600", fi.Mode().Perm())
		}
	}
}
