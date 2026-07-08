// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

func TestDockerRun_HardenedFlagsAndMount(t *testing.T) {
	cmd := dockerRun("ubuntu:24.04", "/root/workspace", "./app", nil)
	for _, want := range []string{
		"docker run",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--tmpfs /tmp:exec",
		"-v '/root/workspace':/workspace -w /workspace",
		"'ubuntu:24.04'",
		"sh -c './app'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("dockerRun missing %q\n%s", want, cmd)
		}
	}
	// must NOT mount the docker socket or run privileged (S-D)
	for _, bad := range []string{"docker.sock", "--privileged"} {
		if strings.Contains(cmd, bad) {
			t.Errorf("dockerRun must not contain %q", bad)
		}
	}
}

func TestDockerRun_NoWorkspaceNoMount(t *testing.T) {
	cmd := dockerRun("alpine", "", "echo hi", nil)
	if strings.Contains(cmd, "-v ") || strings.Contains(cmd, "-w ") {
		t.Errorf("no workspace should mean no mount:\n%s", cmd)
	}
}
