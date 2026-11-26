// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// fakeNode is a minimal in-process SSH server that records every exec'd command
// in order (draining stdin) and exits 0 — enough to assert what `up` runs on a
// node, and in which order, without a cloud.
type fakeNode struct {
	addr   string
	hostPK gossh.PublicKey
	signer gossh.Signer // client login key

	mu       sync.Mutex
	commands []string
	failOn   string // exit 127 for a command containing this substring
}

func (f *fakeNode) record(cmd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
}

func (f *fakeNode) cmds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func startFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := gossh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, loginPriv, _ := ed25519.GenerateKey(rand.Reader)
	loginSigner, err := gossh.NewSignerFromKey(loginPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &gossh.ServerConfig{
		PublicKeyCallback: func(_ gossh.ConnMetadata, k gossh.PublicKey) (*gossh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeNode{addr: ln.Addr().String(), hostPK: hostSigner.PublicKey(), signer: loginSigner}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, cfg)
		}
	}()
	return f
}

func (f *fakeNode) serve(c net.Conn, cfg *gossh.ServerConfig) {
	_, chans, reqs, err := gossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go gossh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range creqs {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				var p struct{ Cmd string }
				_ = gossh.Unmarshal(req.Payload, &p)
				_ = req.Reply(true, nil)
				f.record(p.Cmd)
				_, _ = io.Copy(io.Discard, ch) // drain stdin (tar stream) until EOF
				code := uint32(0)
				if f.failOn != "" && strings.Contains(p.Cmd, f.failOn) {
					_, _ = io.WriteString(ch.Stderr(), "bash: no such file\n")
					code = 127
				}
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, code)
				_, _ = ch.SendRequest("exit-status", false, status)
				return
			}
		}()
	}
}

func indexOf(cmds []string, sub string) int {
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// #1: a setup script that lives IN the workspace must work — so the workspace is
// synced BEFORE setup runs, and the build runs AFTER setup (it may need what
// setup installed).
func TestBuildWindow_SyncsBeforeSetupBeforeBuild(t *testing.T) {
	node := startFakeNode(t)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "scripts", "node_setup.sh"), []byte("echo hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	wd, err := runBuildWindow(context.Background(), buildWindow{
		node: "node-a", addr: node.addr, signer: node.signer, pinned: node.hostPK,
		setup:   []string{"bash /srv/gpu-lab/scripts/node_setup.sh"},
		sync:    &syncSpec{LocalPath: ws, RemotePath: "/srv/gpu-lab", Build: "make all"},
		runUser: "root", engine: "native",
	})
	if err != nil {
		t.Fatalf("build window: %v", err)
	}
	if wd != "/srv/gpu-lab" {
		t.Errorf("workdir = %q, want /srv/gpu-lab", wd)
	}
	cmds := node.cmds()
	extract := indexOf(cmds, "tar -xzf - -C '/srv/gpu-lab'")
	setup := indexOf(cmds, "node_setup.sh")
	build := indexOf(cmds, "make all")
	if extract < 0 || setup < 0 || build < 0 {
		t.Fatalf("missing a step; commands run: %q", cmds)
	}
	if !(extract < setup && setup < build) {
		t.Fatalf("want sync < setup < build, got sync=%d setup=%d build=%d in %q", extract, setup, build, cmds)
	}
}

// A failing setup stops the window (no build) and returns the error, which the
// callers turn into exit codeInfraDegraded (#2).
func TestBuildWindow_SetupFailureStopsBeforeBuild(t *testing.T) {
	node := startFakeNode(t)
	node.failOn = "node_setup.sh"
	_, err := runBuildWindow(context.Background(), buildWindow{
		node: "node-a", addr: node.addr, signer: node.signer, pinned: node.hostPK,
		setup:   []string{"bash /srv/x/node_setup.sh"},
		sync:    &syncSpec{LocalPath: t.TempDir(), RemotePath: "/srv/x", Build: "make all"},
		runUser: "root", engine: "native",
	})
	if err == nil || !strings.Contains(err.Error(), "setup command failed") {
		t.Fatalf("want a setup failure, got %v", err)
	}
	if indexOf(node.cmds(), "make all") >= 0 {
		t.Fatalf("build must not run after a failed setup: %q", node.cmds())
	}
}

// docker engine: sync (owned by root) → setup → pull → build in the container.
func TestBuildWindow_DockerOrder(t *testing.T) {
	node := startFakeNode(t)
	_, err := runBuildWindow(context.Background(), buildWindow{
		node: "node-a", addr: node.addr, signer: node.signer, pinned: node.hostPK,
		setup:   []string{"echo setup-step"},
		sync:    &syncSpec{LocalPath: t.TempDir(), RemotePath: "/w", Build: "make all"},
		runUser: "pandion", engine: "docker", containerImg: "ubuntu:24.04",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmds := node.cmds()
	order := []int{indexOf(cmds, "tar -xzf"), indexOf(cmds, "setup-step"), indexOf(cmds, "docker pull"), indexOf(cmds, "docker run")}
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] < 0 || order[i-1] > order[i] {
			t.Fatalf("want sync < setup < pull < build, got %v in %q", order, cmds)
		}
	}
	if indexOf(cmds, "chown") >= 0 {
		t.Errorf("docker workspaces are root-owned; no chown expected: %q", cmds)
	}
}
