// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

// Container providers publish sshd on a mapped port: ssh/scp must dial it, and
// the pinned known_hosts entry must be keyed "[ip]:port" or OpenSSH won't match it.
func TestMappedSSHPort(t *testing.T) {
	if got := knownHostsAddr("203.0.113.7", 40123); got != "[203.0.113.7]:40123" {
		t.Errorf("known_hosts addr = %q", got)
	}
	if got := knownHostsAddr("203.0.113.7", 22); got != "203.0.113.7" {
		t.Errorf("port 22 stays bare, got %q", got)
	}
	c := connInfo{addr: "203.0.113.7", port: 40123, keyPath: "k", khPath: "kh"}
	if s := strings.Join(c.opts(), " "); !strings.Contains(s, "-p 40123") {
		t.Errorf("ssh opts = %s", s)
	}
	if s := strings.Join(c.scpOpts(), " "); !strings.Contains(s, "-P 40123") {
		t.Errorf("scp opts = %s", s)
	}
	if s := strings.Join(connInfo{addr: "h"}.opts(), " "); strings.Contains(s, "-p") {
		t.Errorf("no port flag for 22: %s", s)
	}
	if got := (nodeManifest{IP: "203.0.113.7", Port: 40123}).sshAddr(); got != "203.0.113.7:40123" {
		t.Errorf("sshAddr = %q", got)
	}
	if got := (nodeManifest{IP: "198.51.100.2"}).sshAddr(); got != "198.51.100.2:22" {
		t.Errorf("sshAddr default = %q", got)
	}
}

// The container posture note must say plainly what is NOT protected.
func TestContainerPostureNote(t *testing.T) {
	n := containerPostureNote("runpod")
	for _, want := range []string{"egress lockdown", "WireGuard", "idle poweroff", "outbound traffic is unrestricted", "pandion down"} {
		if !strings.Contains(n, want) {
			t.Errorf("note missing %q", want)
		}
	}
}

func TestRunPodRegistered(t *testing.T) {
	if !isCloudProvider("runpod") {
		t.Fatal("runpod must take the hardened up path")
	}
	if name, env, ok := providerEnv("runpod"); !ok || name != "runpod" || env != "RUNPOD_API_KEY" {
		t.Fatalf("providerEnv = %q %q %v", name, env, ok)
	}
}
