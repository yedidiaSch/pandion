package config

import "testing"

func TestEffectiveL2_StringShorthand(t *testing.T) {
	c := &Cluster{
		Defaults: NodeCommon{Sec: &Security{Overlay: "l2"}},
		Nodes:    []Node{{Name: "a"}},
	}
	e := c.Effective(c.Nodes[0])
	if e.L2 == nil {
		t.Fatal("overlay: l2 should yield an L2 overlay")
	}
	if e.L2.Profile != "safe" || e.L2.Subnet != DefaultL2Subnet || e.L2.VNI != DefaultL2VNI {
		t.Fatalf("string shorthand defaults wrong: %+v", e.L2)
	}
}

func TestEffectiveL2_ObjectForm(t *testing.T) {
	c := &Cluster{
		Defaults: NodeCommon{Sec: &Security{Overlay: map[string]any{
			"l2": map[string]any{"profile": "lab", "subnet": "10.10.0.0/24", "vni": 42},
		}}},
		Nodes: []Node{{Name: "a"}},
	}
	e := c.Effective(c.Nodes[0])
	if e.L2 == nil || e.L2.Profile != "lab" || e.L2.Subnet != "10.10.0.0/24" || e.L2.VNI != 42 {
		t.Fatalf("object form not parsed: %+v", e.L2)
	}
}

func TestEffectiveL2_NodeOverridesDefault(t *testing.T) {
	c := &Cluster{
		Defaults: NodeCommon{Sec: &Security{Overlay: "l2"}}, // safe
		Nodes: []Node{{
			Name:       "a",
			NodeCommon: NodeCommon{Sec: &Security{Overlay: map[string]any{"l2": map[string]any{"profile": "lab"}}}},
		}},
	}
	if e := c.Effective(c.Nodes[0]); e.L2 == nil || e.L2.Profile != "lab" {
		t.Fatalf("node override should win: %+v", e.L2)
	}
}

func TestEffectiveL2_NoneWhenAutoOrAbsent(t *testing.T) {
	for _, ov := range []any{nil, "auto", false, map[string]any{"other": 1}} {
		c := &Cluster{Defaults: NodeCommon{Sec: &Security{Overlay: ov}}, Nodes: []Node{{Name: "a"}}}
		if e := c.Effective(c.Nodes[0]); e.L2 != nil {
			t.Fatalf("overlay %v should not enable L2, got %+v", ov, e.L2)
		}
	}
	// no security block at all → no L2.
	c := &Cluster{Nodes: []Node{{Name: "a"}}}
	if e := c.Effective(c.Nodes[0]); e.L2 != nil {
		t.Fatal("absent security should not enable L2")
	}
}

func TestValidate_L2OverlayForms(t *testing.T) {
	ok := []byte(`apiVersion: pandion/v1
name: lab-cluster
provider: { name: hetzner }
defaults:
  security:
    overlay:
      l2:
        profile: lab
        subnet: 192.168.66.0/24
        vni: 100
nodes:
  - name: a
    run: "echo hi"
`)
	if err := Validate(ok); err != nil {
		t.Fatalf("valid l2 object rejected: %v", err)
	}
	shorthand := []byte("apiVersion: pandion/v1\nname: lab-cluster\nprovider: { name: hetzner }\ndefaults:\n  security:\n    overlay: l2\nnodes:\n  - name: a\n    run: \"echo hi\"\n")
	if err := Validate(shorthand); err != nil {
		t.Fatalf("valid l2 shorthand rejected: %v", err)
	}
	bad := []byte("apiVersion: pandion/v1\nname: lab-cluster\nprovider: { name: hetzner }\ndefaults:\n  security:\n    overlay:\n      l2:\n        profile: bogus\nnodes:\n  - name: a\n    run: \"echo hi\"\n")
	if err := Validate(bad); err == nil {
		t.Fatal("invalid l2 profile should be rejected by the schema")
	}
}
