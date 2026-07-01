// Package stream multiplexes per-node stdout/stderr into one local view:
// color-coded, line-prefixed by node, and tee'd to per-node log files (M4).
//
// The Printer is concurrency-safe and pure w.r.t. transport (it just consumes
// (node, stream, line) events), so it is unit-testable offline.
package stream

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// palette of ANSI foreground colors cycled per node.
var palette = []string{"36", "32", "33", "35", "34", "31", "96", "92"}

// Printer renders multiplexed output and tees raw lines to log files.
type Printer struct {
	mu     sync.Mutex
	out    io.Writer
	color  bool
	logDir string

	colorOf map[string]string
	next    int
	logs    map[string]*os.File
}

// NewPrinter writes to out, colorizing unless color is false. If logDir is
// non-empty, each node's raw lines are also tee'd to logDir/<node>.log.
func NewPrinter(out io.Writer, logDir string, color bool) *Printer {
	return &Printer{
		out: out, color: color, logDir: logDir,
		colorOf: map[string]string{}, logs: map[string]*os.File{},
	}
}

func (p *Printer) colorFor(node string) string {
	if c, ok := p.colorOf[node]; ok {
		return c
	}
	c := palette[p.next%len(palette)]
	p.next++
	p.colorOf[node] = c
	return c
}

func (p *Printer) logFor(node string) *os.File {
	if p.logDir == "" {
		return nil
	}
	if f, ok := p.logs[node]; ok {
		return f
	}
	if err := os.MkdirAll(p.logDir, 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(p.logDir, node+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	p.logs[node] = f
	return f
}

// Print renders one line from node's stdout ("out") or stderr ("err").
func (p *Printer) Print(node, streamName, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	label := "[" + node + "]"
	if streamName == "err" {
		label = "[" + node + " !]" // mark stderr
	}
	if p.color {
		c := p.colorFor(node)
		fmt.Fprintf(p.out, "\033[%sm%s\033[0m %s\n", c, label, line)
	} else {
		fmt.Fprintf(p.out, "%s %s\n", label, line)
	}
	if f := p.logFor(node); f != nil {
		fmt.Fprintln(f, line) // raw (no color) to the log
	}
}

// Close flushes and closes all log files.
func (p *Printer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.logs {
		_ = f.Close()
	}
	p.logs = map[string]*os.File{}
}
