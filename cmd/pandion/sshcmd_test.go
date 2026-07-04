package main

import (
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
