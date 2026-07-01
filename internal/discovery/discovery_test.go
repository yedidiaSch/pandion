package discovery

import (
	"strings"
	"testing"
)

func TestEnvVarName(t *testing.T) {
	cases := map[string]string{
		"broker":   "ENVCORE_BROKER_IP",
		"worker-a": "ENVCORE_WORKER_A_IP",
		"n1.node":  "ENVCORE_N1_NODE_IP",
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
		"export ENVCORE_BROKER_IP=10.99.0.1",
		"export ENVCORE_WORKER_A_IP=10.99.0.2",
		"export ENVCORE_SELF_NAME=worker-a",
		"export ENVCORE_SELF_IP=10.99.0.2",
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

func TestScript_NoSelf(t *testing.T) {
	out := Script(map[string]string{"a": "10.99.0.1"}, "")
	if strings.Contains(out, "ENVCORE_SELF") {
		t.Errorf("should omit SELF when unset:\n%s", out)
	}
}
