// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Opt-in integration test: run the REAL container bootstrap in a local Docker
// container (ubuntu:24.04, port 22 published on a random host port — the shape
// RunPod gives a pod), then drive it with up's own pinned-SSH readiness gate.
//
//	PANDION_DOCKER_POD_TEST=1 go test ./cmd/pandion -run TestPodBootstrapInDocker -v

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yedidiaSch/pandion/internal/harden"
	envssh "github.com/yedidiaSch/pandion/internal/ssh"
	"github.com/yedidiaSch/pandion/internal/sshkeys"
)

func TestPodBootstrapInDocker(t *testing.T) {
	if os.Getenv("PANDION_DOCKER_POD_TEST") == "" {
		t.Skip("set PANDION_DOCKER_POD_TEST=1 (needs docker + network)")
	}
	host, err := sshkeys.Generate("pandion-host")
	if err != nil {
		t.Fatal(err)
	}
	login, err := sshkeys.Generate("pandion-login")
	if err != nil {
		t.Fatal(err)
	}
	script := harden.PodBootstrap(harden.CloudInit{
		HostPrivKeyPEM: host.PrivatePEM, HostPubKey: host.PublicAuthorized, LoginPubKey: login.PublicAuthorized,
		Packages: []string{"build-essential", "not-a-real-package-xyz"}, RunUser: harden.DefaultRunUser,
	})
	ep := harden.PodEntrypoint()
	args := []string{"run", "-d", "--rm", "-p", "127.0.0.1::22",
		"-e", harden.PodBootEnv + "=" + base64.StdEncoding.EncodeToString([]byte(script)),
		"-e", "RUNPOD_API_KEY=must-not-leak", "-e", "CUDA_VERSION=12.8.1",
		"--entrypoint", ep[0], "ubuntu:24.04", ep[1], ep[2]}
	out, err := exec.Command("docker", args...).Output() // stdout only: pull progress goes to stderr
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })
	pout, err := exec.Command("docker", "port", cid, "22/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	hp := strings.TrimSpace(strings.Split(string(pout), "\n")[0])
	_, portStr, _ := strings.Cut(hp, ":")
	port, _ := strconv.Atoi(portStr)
	addr := "127.0.0.1:" + strconv.Itoa(port)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := waitPodReady(ctx, "node-a", addr, login.Signer, host.Public, func(n int, r string) { t.Logf("attempt %d: %s", n, r) }); err != nil {
		logs, _ := exec.Command("docker", "exec", cid, "cat", harden.PodBootLog).CombinedOutput()
		t.Fatalf("waitPodReady: %v\nboot log:\n%s", err, logs)
	}
	run := func(cmd string) string {
		o, err := envssh.Run(ctx, addr, "root", login.Signer, host.Public, cmd)
		if err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, o)
		}
		return strings.TrimSpace(o)
	}
	if got := run("id -u " + harden.DefaultRunUser + " >/dev/null && echo ok"); got != "ok" {
		t.Errorf("run user missing: %q", got)
	}
	if got := run("command -v tmux gcc | wc -l"); got != "2" {
		t.Errorf("tmux + gcc should be installed: %q", got)
	}
	if got := run("grep -c not-a-real-package-xyz " + harden.PodBootLog); got == "0" {
		t.Errorf("a bad package should be logged, not fatal")
	}
	if got := run("cat /etc/environment"); strings.Contains(got, "must-not-leak") || !strings.Contains(got, "CUDA_VERSION") {
		t.Errorf("/etc/environment must carry CUDA vars but never RUNPOD_*: %q", got)
	}
	if got := run("ls /root/.pandion-boot.sh >/dev/null && stat -c %a /root/.pandion-boot.sh"); got != "600" {
		t.Errorf("bootstrap script (holds the host key) must be root-only, mode = %q", got)
	}
	// the exact launch/tail helpers `up` uses work on the container
	if err := launchRun(ctx, addr, login.Signer, host.Public, runAs(harden.DefaultRunUser, "", "echo hi-from-run && whoami", nil)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if got := run("cat " + runLogPath); !strings.Contains(got, "hi-from-run") || !strings.Contains(got, harden.DefaultRunUser) {
		t.Errorf("workload log = %q", got)
	}
	// a wrong host key is refused (pinning holds on the mapped port)
	other, _ := sshkeys.Generate("other")
	if _, err := envssh.Run(ctx, addr, "root", login.Signer, other.Public, "true"); err == nil {
		t.Error("connection with an unpinned host key must fail")
	}
}
