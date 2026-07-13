// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"encoding/json"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// unappliedField is a schema property that VALIDATES but has no backend consumer
// yet. Rather than silently ignore it (the worst config bug — the file is "valid"
// and wrong, P2.2), the loader warns when it's present, and the drift test below
// asserts this list exactly matches the schema-minus-consumers set, so a future
// schema field can't land without either a consumer or an entry here.
type unappliedField struct {
	Path string // slash path into the schema, e.g. "provider/private_network"
	Note string // what actually happens instead
}

var unappliedFields = []unappliedField{
	{Path: "firewall", Note: "accepted but not yet applied — the hardened default-deny firewall is used instead (see `pandion lockdown`)"},
	{Path: "provider/private_network", Note: "accepted but not yet applied — nodes use the WireGuard overlay for private connectivity"},
}

// Warnings inspects a cluster.yaml and returns a message for each accepted-but-
// unapplied field actually present in it (P2.2). Empty when the file uses only
// implemented fields. Callers (validate/up) print these so the user is never
// silently misled.
func Warnings(data []byte) []string {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var out []string
	for _, uf := range unappliedFields {
		if _, ok := lookupPath(raw, strings.Split(uf.Path, "/")); ok {
			out = append(out, "\""+uf.Path+":\" — "+uf.Note)
		}
	}
	return out
}

// lookupPath reports whether a slash path exists in a decoded YAML map.
func lookupPath(m map[string]any, seg []string) (any, bool) {
	var cur any = m
	for _, s := range seg {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[s]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// schemaPropertyPaths walks the embedded schema and returns every property path
// (array items are transparent, so "nodes[].name" becomes "nodes/name"). Used by
// the drift test.
func schemaPropertyPaths() []string {
	var doc any
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		return nil
	}
	var out []string
	var walk func(node any, path string)
	walk = func(node any, path string) {
		node = deref(doc, node)
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for k, v := range props {
				p := k
				if path != "" {
					p = path + "/" + k
				}
				out = append(out, p)
				walk(v, p)
			}
		}
		if items, ok := m["items"]; ok {
			walk(items, path) // array items are transparent in the path
		}
		if allOf, ok := m["allOf"].([]any); ok {
			for _, a := range allOf {
				walk(a, path) // merge allOf members (e.g. node/defaults ← nodeCommon)
			}
		}
	}
	walk(doc, "")
	// dedupe (allOf merges can repeat a path).
	seen := map[string]bool{}
	uniq := out[:0]
	for _, p := range out {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

// consumedYAMLPaths reflects over the Cluster struct and returns the set of YAML
// paths it actually unmarshals (so the drift test can tell "consumed" from
// "ignored"). Slice/pointer element types are followed; ",inline" adds no segment.
func consumedYAMLPaths() map[string]bool {
	out := map[string]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("yaml")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if strings.Contains(opts, "inline") || (name == "" && strings.Contains(tag, "inline")) {
				walk(f.Type, path) // inline: same path level
				continue
			}
			if name == "" {
				continue
			}
			p := name
			if path != "" {
				p = path + "/" + name
			}
			out[p] = true
			walk(f.Type, p)
		}
	}
	walk(reflect.TypeOf(Cluster{}), "")
	return out
}
