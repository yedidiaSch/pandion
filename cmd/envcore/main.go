// Command envcore is the M0 walking skeleton CLI.
//
// M0 wires the in-memory MOCK provider so the spine (state machine + reconcile)
// can be exercised end-to-end with NO cloud and NO cost. The real cobra CLI and
// the Hetzner provider arrive in M1; the authoritative M0 proof is the
// orchestrator test suite (go test ./...).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/envcore/envcore/internal/orchestrator"
	"github.com/envcore/envcore/internal/provider/mock"
	"github.com/envcore/envcore/internal/state"
)

const version = "0.0.0-m0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version":
		fmt.Println("envcore", version)

	case "demo": // single-process up->down lifecycle on the mock provider
		o, m := newOrch()
		ctx := context.Background()
		c, err := o.Up(ctx, "demo", "node-a", "#cloud-config\n")
		must(err)
		fmt.Printf("UP    cluster=%q node=%q phase=%s servers=%d\n",
			c.ID, c.Nodes[0].Name, c.Nodes[0].Phase, m.Count())
		must(o.Down(ctx, "demo"))
		fmt.Printf("DOWN  cluster=%q reconciled servers=%d (expect 0)\n", c.ID, m.Count())
		fmt.Println("note: M0 uses the in-memory mock provider — no cloud resources created.")

	case "up":
		fs := flag.NewFlagSet("up", flag.ExitOnError)
		id := fs.String("id", "demo", "cluster id")
		node := fs.String("node", "node-a", "node name")
		_ = fs.Parse(os.Args[2:])
		o, _ := newOrch()
		c, err := o.Up(context.Background(), *id, *node, "#cloud-config\n")
		must(err)
		fmt.Printf("UP: cluster %q node %q -> %s (provider=mock)\n", c.ID, *node, c.Nodes[0].Phase)

	case "down":
		fs := flag.NewFlagSet("down", flag.ExitOnError)
		id := fs.String("id", "demo", "cluster id")
		_ = fs.Parse(os.Args[2:])
		o, _ := newOrch()
		must(o.Down(context.Background(), *id))
		fmt.Printf("DOWN: cluster %q reconciled to empty.\n", *id)

	default:
		usage()
		os.Exit(2)
	}
}

func newOrch() (*orchestrator.Orchestrator, *mock.Mock) {
	home, _ := os.UserHomeDir()
	st, err := state.NewStore(filepath.Join(home, ".envcore", "state"))
	must(err)
	m := mock.New()
	return orchestrator.New(m, st), m
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: envcore [demo|up|down|version] [-id ID] [-node NAME]")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
