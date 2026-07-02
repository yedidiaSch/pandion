package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestCapsFor_NeedsCapsAndPrivPorts(t *testing.T) {
	got := capsFor([]string{"NET_RAW", "cap_sys_ptrace"}, []string{"443/tcp", "8080/tcp"})
	want := []string{"NET_RAW", "SYS_PTRACE", "NET_BIND_SERVICE"} // 443<1024 -> bind service; 8080 not
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capsFor = %v, want %v", got, want)
	}
}

func TestCapsFor_NoLowPortNoBindCap(t *testing.T) {
	got := capsFor(nil, []string{"8080/tcp"})
	if len(got) != 0 {
		t.Fatalf("no low port should add no caps, got %v", got)
	}
}

func TestDockerRun_AddsDeclaredCaps(t *testing.T) {
	cmd := dockerRun("alpine", "", "id", []string{"NET_RAW"})
	if !strings.Contains(cmd, "--cap-add=NET_RAW") {
		t.Errorf("missing --cap-add=NET_RAW:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--cap-drop=ALL") {
		t.Errorf("base drop-all must remain:\n%s", cmd)
	}
}

func TestRunAs_SetprivGrantsAmbientCaps(t *testing.T) {
	cmd := runAs("pandion-run", "/w", "./app", []string{"NET_BIND_SERVICE"})
	for _, want := range []string{"setpriv", "--reuid='pandion-run'", "--ambient-caps=+net_bind_service"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q:\n%s", want, cmd)
		}
	}
}

func TestRunAs_RootHoldsCapsNoSetpriv(t *testing.T) {
	cmd := runAs("root", "", "x", []string{"NET_RAW"})
	if strings.Contains(cmd, "setpriv") {
		t.Errorf("root should not use setpriv:\n%s", cmd)
	}
}
