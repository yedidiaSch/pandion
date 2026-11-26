// SPDX-License-Identifier: AGPL-3.0-or-later

package harden

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPodBootstrap(t *testing.T) {
	s := PodBootstrap(CloudInit{
		HostPrivKeyPEM: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n",
		HostPubKey:     "ssh-ed25519 AAAAhost pandion-host",
		LoginPubKey:    "ssh-ed25519 AAAAlogin pandion-login",
		Packages:       []string{"build-essential", "cmake", "libopencv-core-dev", "tmux"},
		RunUser:        "pandion-run",
	})
	for _, want := range []string{
		"BEGIN OPENSSH PRIVATE KEY", // the pinned host key is installed
		"ssh_host_ed25519_key.pub",
		"'ssh-ed25519 AAAAlogin pandion-login' >> /root/.ssh/authorized_keys",
		"PasswordAuthentication no",
		"/usr/sbin/sshd",
		"'libopencv-core-dev'",
		"useradd -m -s /bin/bash 'pandion-run'",
		PodReadyFile, PodFailedFile,
		"exec sleep infinity",
		"/etc/profile.d/pandion-cuda.sh",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("bootstrap missing %q", want)
		}
	}
	if strings.Count(s, "'tmux'") != 1 {
		t.Error("tmux must be installed exactly once")
	}
	// the exported env is an allowlist: RunPod's own vars can carry API keys.
	if strings.Contains(s, "RUNPOD_") {
		t.Error("bootstrap must never export RUNPOD_* vars to SSH sessions")
	}
	// sshd starts before the (slow) toolchain install, so `up` can connect early
	if strings.Index(s, "/usr/sbin/sshd") > strings.Index(s, "PKGS=(") {
		t.Error("sshd should start before the package install")
	}
	bashSyntax(t, s)
	bashSyntax(t, PodReadyCmd())
	bashSyntax(t, PodEntrypoint()[2])
}

// bashSyntax checks a script with `bash -n` (skipped if bash is absent).
func bashSyntax(t *testing.T, script string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	p := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(p, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s\n---\n%s", err, out, script)
	}
}

// The entrypoint really decodes and runs the script from the env var, and the
// readiness command reports a failed bootstrap with the log tail.
func TestPodEntrypointAndReady(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	ep := PodEntrypoint()
	// run against a temp HOME-like root by rewriting /root to a temp dir
	dir := t.TempDir()
	cmdStr := strings.ReplaceAll(ep[2], "/root/", dir+"/")
	c := exec.Command(ep[0], ep[1], cmdStr)
	c.Env = append(os.Environ(), PodBootEnv+"=ZWNobyBoZWxsby1mcm9tLWJvb3QK") // "echo hello-from-boot"
	out, err := c.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "hello-from-boot" {
		t.Fatalf("entrypoint: %v %q", err, out)
	}

	ready := strings.NewReplacer(PodReadyFile, filepath.Join(dir, "ready"), PodFailedFile, filepath.Join(dir, "failed"), PodBootLog, filepath.Join(dir, "boot.log")).Replace(PodReadyCmd())
	_ = os.WriteFile(filepath.Join(dir, "boot.log"), []byte("pandion bootstrap FAILED: install openssh-server\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "failed"), nil, 0o600)
	out, err = exec.Command("bash", "-c", ready).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "FAILED: install openssh-server") {
		t.Fatalf("ready on failure: %v %q", err, out)
	}
	_ = os.Remove(filepath.Join(dir, "failed"))
	_ = os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600)
	if out, err := exec.Command("bash", "-c", ready).CombinedOutput(); err != nil {
		t.Fatalf("ready on success: %v %q", err, out)
	}
}
