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
	if err != nil || strings.TrimSpace(string(b)) != "hello" {
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
