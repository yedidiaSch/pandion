// SPDX-License-Identifier: AGPL-3.0-or-later

package stream

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPrinter_PrefixAndNoColorAndTee(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	p := NewPrinter(&buf, dir, false)
	defer p.Close()

	p.Print("broker", "out", "hello")
	p.Print("worker", "err", "oops")

	got := buf.String()
	if !strings.Contains(got, "[broker] hello") {
		t.Errorf("missing stdout prefix line:\n%s", got)
	}
	if !strings.Contains(got, "[worker !] oops") {
		t.Errorf("stderr should be marked:\n%s", got)
	}
	// no ANSI when color disabled
	if strings.Contains(got, "\033[") {
		t.Errorf("color escape leaked with color=false:\n%q", got)
	}
	// tee'd raw (no prefix/color) to per-node logs
	p.Close()
	b, err := os.ReadFile(filepath.Join(dir, "broker.log"))
	// the log is prefixed with a session separator (P4.3), then the raw line.
	if err != nil || !strings.Contains(string(b), "hello") || strings.Contains(string(b), "[broker]") {
		t.Fatalf("broker.log = %q err=%v", string(b), err)
	}
}

func TestPrinter_ColorAssignedPerNode(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, "", true)
	p.Print("a", "out", "x")
	p.Print("b", "out", "y")
	p.Print("a", "out", "z")
	if p.colorOf["a"] == p.colorOf["b"] {
		t.Errorf("distinct nodes should get distinct colors")
	}
	if !strings.Contains(buf.String(), "\033[") {
		t.Errorf("expected ANSI color when enabled")
	}
}

func TestPrinter_ConcurrentSafe(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, "", false)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) { defer wg.Done(); p.Print("n", "out", "line") }(i)
	}
	wg.Wait()
	if lines := strings.Count(buf.String(), "[n] line"); lines != 50 {
		t.Fatalf("want 50 lines, got %d (race/interleave)", lines)
	}
}

// TestPrinter_AppendsAcrossSessions covers P4.3: a second Printer over the same
// logDir must APPEND (not truncate) and mark the reconnect with a separator, so the
// first session's captured output survives an inspect-time attach.
func TestPrinter_AppendsAcrossSessions(t *testing.T) {
	dir := t.TempDir()

	p1 := NewPrinter(&bytes.Buffer{}, dir, false)
	p1.Print("node", "out", "first-session-line")
	p1.Close()

	p2 := NewPrinter(&bytes.Buffer{}, dir, false)
	p2.Print("node", "out", "second-session-line")
	p2.Close()

	data, err := os.ReadFile(filepath.Join(dir, "node.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "first-session-line") {
		t.Errorf("first session's output was lost (truncated?):\n%s", s)
	}
	if !strings.Contains(s, "second-session-line") {
		t.Errorf("second session's output missing:\n%s", s)
	}
	if strings.Count(s, "----- attached") < 2 {
		t.Errorf("expected a session separator per attach:\n%s", s)
	}
}

// TestRotateIfLarge covers the size-based rotation: a log past the cap is renamed
// to <node>.log.1 (one generation kept).
func TestRotateIfLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.log")
	big := make([]byte, maxLogBytes+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	rotateIfLarge(path)
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated generation node.log.1: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("original log should have been renamed away")
	}
}
