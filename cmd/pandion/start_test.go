package main

import "testing"

func TestSelectStartNodes(t *testing.T) {
	nodes := []nodeManifest{
		{Name: "target"},                  // deploy-only (no run)
		{Name: "worker", Run: "./work"},   // runnable
		{Name: "broker", Run: "./broker"}, // runnable
	}

	t.Run("all runnable, deploy-only skipped", func(t *testing.T) {
		sel, skipped, err := selectStartNodes(nodes, "", "c1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sel) != 2 || sel[0].Name != "worker" || sel[1].Name != "broker" {
			t.Fatalf("want [worker broker], got %v", nodeNames(sel))
		}
		if len(skipped) != 1 || skipped[0] != "target" {
			t.Fatalf("want skipped [target], got %v", skipped)
		}
	})

	t.Run("filter to one runnable node", func(t *testing.T) {
		sel, _, err := selectStartNodes(nodes, "worker", "c1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sel) != 1 || sel[0].Name != "worker" {
			t.Fatalf("want [worker], got %v", nodeNames(sel))
		}
	})

	t.Run("named deploy-only node errors helpfully", func(t *testing.T) {
		_, _, err := selectStartNodes(nodes, "target", "c1")
		if err == nil {
			t.Fatal("expected error naming a deploy-only node")
		}
		if got := err.Error(); !containsSub(got, "deploy-only") || !containsSub(got, "pandion ssh") {
			t.Fatalf("error should explain deploy-only + manual run, got: %v", err)
		}
	})

	t.Run("unknown node errors", func(t *testing.T) {
		_, _, err := selectStartNodes(nodes, "ghost", "c1")
		if err == nil || !containsSub(err.Error(), `no node "ghost"`) {
			t.Fatalf("want unknown-node error, got: %v", err)
		}
	})

	t.Run("nothing runnable errors", func(t *testing.T) {
		_, _, err := selectStartNodes([]nodeManifest{{Name: "a"}, {Name: "b"}}, "", "c1")
		if err == nil || !containsSub(err.Error(), "nothing to start") {
			t.Fatalf("want nothing-to-start error, got: %v", err)
		}
	})
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
