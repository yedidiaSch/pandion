package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildAttachConfig_PinnedPipeAndFields(t *testing.T) {
	cfg := buildAttachConfig("pipeline", "worker", "10.99.0.2",
		"/home/u/.pandion/keys/pipeline/login_ed25519",
		"/home/u/.pandion/keys/pipeline/known_hosts",
		"/home/pandion-run/workspace/app", pickRemoteProcess)

	if cfg.Type != "cppdbg" || cfg.Request != "attach" || cfg.MIMode != "gdb" {
		t.Fatalf("wrong debugger identity: %+v", cfg)
	}
	if cfg.Name != "Pandion attach: pipeline-worker" {
		t.Fatalf("name = %q", cfg.Name)
	}
	if cfg.ProcessID != pickRemoteProcess {
		t.Fatalf("default processId should be the remote picker, got %q", cfg.ProcessID)
	}
	// the pipe must carry the pinned, MITM-proof SSH posture at the overlay IP.
	args := strings.Join(cfg.PipeTransport.PipeArgs, " ")
	for _, want := range []string{
		"login_ed25519",
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/home/u/.pandion/keys/pipeline/known_hosts",
		"IdentitiesOnly=yes",
		"root@10.99.0.2",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("pipeArgs missing %q: %v", want, cfg.PipeTransport.PipeArgs)
		}
	}
	if cfg.PipeTransport.PipeProgram != "ssh" {
		t.Fatalf("pipeProgram = %q, want ssh", cfg.PipeTransport.PipeProgram)
	}
	if cfg.SourceFileMap["/home/pandion-run/workspace"] != "${workspaceFolder}" {
		t.Fatalf("sourceFileMap not wired: %+v", cfg.SourceFileMap)
	}
}

func TestBuildAttachConfig_ExplicitPID(t *testing.T) {
	cfg := buildAttachConfig("c", "n", "203.0.113.7", "k", "kh", "/root/workspace", "4242")
	if cfg.ProcessID != "4242" {
		t.Fatalf("processId = %q, want 4242", cfg.ProcessID)
	}
	if !strings.Contains(strings.Join(cfg.PipeTransport.PipeArgs, " "), "root@203.0.113.7") {
		t.Fatalf("public addr not in pipeArgs: %v", cfg.PipeTransport.PipeArgs)
	}
}

func TestMergeLaunchJSON_CreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vscode", "launch.json")
	cfg := buildAttachConfig("c", "n", "10.0.0.2", "k", "kh", "/w", pickRemoteProcess)

	created, dropped, err := mergeLaunchJSON(path, cfg)
	if err != nil || !created || dropped {
		t.Fatalf("create: created=%v dropped=%v err=%v", created, dropped, err)
	}
	doc := readLaunch(t, path)
	if doc["version"] != "0.2.0" {
		t.Fatalf("version = %v", doc["version"])
	}
	if got := configNames(doc); len(got) != 1 || got[0] != cfg.Name {
		t.Fatalf("configs = %v", got)
	}
}

func TestMergeLaunchJSON_PreservesAndDedupes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launch.json")
	// pre-existing file with an unrelated config AND a stale Pandion one (same name).
	seed := `{
  "version": "0.2.0",
  "configurations": [
    { "name": "My local app", "type": "node", "request": "launch" },
    { "name": "Pandion attach: c-n", "type": "cppdbg", "request": "attach", "processId": "OLD" }
  ]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := buildAttachConfig("c", "n", "10.0.0.2", "k", "kh", "/w", "999")

	created, _, err := mergeLaunchJSON(path, cfg)
	if err != nil || created {
		t.Fatalf("merge: created=%v err=%v", created, err)
	}
	doc := readLaunch(t, path)
	names := configNames(doc)
	// keeps the unrelated one, and there is exactly one Pandion entry (deduped).
	if len(names) != 2 {
		t.Fatalf("expected 2 configs, got %v", names)
	}
	var pandion int
	for _, n := range names {
		if n == "Pandion attach: c-n" {
			pandion++
		}
	}
	if pandion != 1 {
		t.Fatalf("expected exactly one Pandion config, got %d in %v", pandion, names)
	}
	if !hasConfig(doc, "My local app") {
		t.Fatal("unrelated config was dropped")
	}
	// the surviving Pandion entry is the NEW one (processId 999, not OLD).
	if pid := pandionProcessID(doc); pid != "999" {
		t.Fatalf("stale Pandion config not replaced: processId=%q", pid)
	}
}

func TestMergeLaunchJSON_ToleratesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launch.json")
	seed := `{
  // VS Code adds comments like this
  "version": "0.2.0",
  "configurations": [
    /* a block comment with a tricky "// not a comment" string inside */
    { "name": "keep me", "type": "node", "request": "launch", "prog": "a//b" }
  ]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := buildAttachConfig("c", "n", "10.0.0.2", "k", "kh", "/w", pickRemoteProcess)

	created, dropped, err := mergeLaunchJSON(path, cfg)
	if err != nil {
		t.Fatalf("JSONC merge failed: %v", err)
	}
	if created || !dropped {
		t.Fatalf("expected merge into existing (created=%v) with dropped-comments=%v", created, dropped)
	}
	doc := readLaunch(t, path)
	if !hasConfig(doc, "keep me") || !hasConfig(doc, cfg.Name) {
		t.Fatalf("merge lost a config: %v", configNames(doc))
	}
	// the "a//b" string inside a value must survive comment stripping.
	if p := progOf(doc, "keep me"); p != "a//b" {
		t.Fatalf("string with // was corrupted: %q", p)
	}
}

func TestStripJSONComments_PreservesStrings(t *testing.T) {
	in := []byte(`{"url":"http://x//y","x":1 /*c*/, "y":2 // trailing
}`)
	out, had := stripJSONComments(in)
	if !had {
		t.Fatal("expected comments detected")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("stripped output not valid JSON: %v\n%s", err, out)
	}
	if m["url"] != "http://x//y" {
		t.Fatalf("url string corrupted: %v", m["url"])
	}
}

// --- helpers ---

func readLaunch(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, b)
	}
	return doc
}

func configNames(doc map[string]any) []string {
	var out []string
	if cs, ok := doc["configurations"].([]any); ok {
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func hasConfig(doc map[string]any, name string) bool {
	for _, n := range configNames(doc) {
		if n == name {
			return true
		}
	}
	return false
}

func pandionProcessID(doc map[string]any) string {
	if cs, ok := doc["configurations"].([]any); ok {
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok {
				if n, _ := m["name"].(string); strings.HasPrefix(n, "Pandion attach:") {
					if p, ok := m["processId"].(string); ok {
						return p
					}
				}
			}
		}
	}
	return ""
}

func progOf(doc map[string]any, name string) string {
	if cs, ok := doc["configurations"].([]any); ok {
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok {
				if n, _ := m["name"].(string); n == name {
					if p, ok := m["prog"].(string); ok {
						return p
					}
				}
			}
		}
	}
	return ""
}
