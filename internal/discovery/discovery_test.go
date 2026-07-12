// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"strings"
	"testing"
)

func TestEnvVarName(t *testing.T) {
	cases := map[string]string{
		"broker":   "PANDION_BROKER_IP",
		"worker-a": "PANDION_WORKER_A_IP",
		"n1.node":  "PANDION_N1_NODE_IP",
	}
	for in, want := range cases {
		if got := EnvVarName(in); got != want {
			t.Errorf("EnvVarName(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestScript_SortedExportsAndSelf(t *testing.T) {
	ips := map[string]string{"broker": "10.99.0.1", "worker-a": "10.99.0.2"}
	out := Script(ips, "worker-a")

	for _, want := range []string{
		"export PANDION_BROKER_IP=10.99.0.1",
		"export PANDION_WORKER_A_IP=10.99.0.2",
		"export PANDION_SELF_NAME=worker-a",
		"export PANDION_SELF_IP=10.99.0.2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("script missing %q\n%s", want, out)
		}
	}
	// deterministic ordering: broker before worker-a
	if strings.Index(out, "BROKER") > strings.Index(out, "WORKER_A") {
		t.Errorf("exports not sorted:\n%s", out)
	}
}

// M5-R6: the rendezvous env (rank/world-size/master) is derived from the sorted
// node order, so torchrun/Ray/etc. can form a group over the mesh.
func TestScript_RendezvousEnv(t *testing.T) {
	ips := map[string]string{"broker": "10.99.0.1", "worker-a": "10.99.0.2", "worker-b": "10.99.0.3"}
	// rank-0 is the first in sorted order ("broker"); worker-b is rank 2.
	out := Script(ips, "worker-b")
	for _, want := range []string{
		"export PANDION_WORLD_SIZE=3",
		"export PANDION_RANK=2",
		"export PANDION_MASTER_ADDR=10.99.0.1", // broker (sorted first)
		"export PANDION_MASTER_PORT=29500",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendezvous env missing %q\n%s", want, out)
		}
	}
	// the rank-0 node itself
	if !strings.Contains(Script(ips, "broker"), "export PANDION_RANK=0") {
		t.Error("broker should be rank 0")
	}
	// no self ⇒ no rendezvous env (operator-side / non-node render)
	if strings.Contains(Script(ips, ""), "PANDION_RANK") {
		t.Error("rendezvous env must be gated on selfName")
	}
}

func TestScript_NoSelf(t *testing.T) {
	out := Script(map[string]string{"a": "10.99.0.1"}, "")
	if strings.Contains(out, "PANDION_SELF") {
		t.Errorf("should omit SELF when unset:\n%s", out)
	}
}
