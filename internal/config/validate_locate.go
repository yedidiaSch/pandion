// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strconv"

	"gopkg.in/yaml.v3"
)

// nodeAt walks the parsed YAML document to the value node at an instance path
// (["nodes","0","run"]). Returns nil if the path can't be resolved.
func nodeAt(root *yaml.Node, seg []string) *yaml.Node {
	cur := root
	if cur != nil && cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	for _, s := range seg {
		if cur == nil {
			return nil
		}
		switch cur.Kind {
		case yaml.MappingNode:
			cur = mapValue(cur, s)
		case yaml.SequenceNode:
			idx, err := strconv.Atoi(s)
			if err != nil || idx < 0 || idx >= len(cur.Content) {
				return nil
			}
			cur = cur.Content[idx]
		default:
			return nil
		}
	}
	return cur
}

// mapValue returns the value node for key in a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapKeyNode returns the key (scalar) node for key in a mapping node, or nil.
func mapKeyNode(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i]
		}
	}
	return nil
}

// locateValue returns the 1-based line/col of the value at an instance path.
func locateValue(root *yaml.Node, seg []string) (line, col int) {
	// Point at the KEY (more intuitive than the value) when the last segment is a
	// map field; fall back to the value node otherwise.
	if len(seg) > 0 {
		parent := nodeAt(root, seg[:len(seg)-1])
		if parent != nil && parent.Kind == yaml.MappingNode {
			if k := mapKeyNode(parent, seg[len(seg)-1]); k != nil {
				return k.Line, k.Column
			}
		}
	}
	if n := nodeAt(root, seg); n != nil {
		return n.Line, n.Column
	}
	return 0, 0
}

// locateKey returns the 1-based line/col of an offending child key `bad` inside
// the mapping at instance path seg (used for unknown-field errors).
func locateKey(root *yaml.Node, seg []string, bad string) (line, col int) {
	m := nodeAt(root, seg)
	if m != nil && m.Kind == yaml.MappingNode {
		if k := mapKeyNode(m, bad); k != nil {
			return k.Line, k.Column
		}
	}
	// fall back to the containing object's location.
	return locateValue(root, seg)
}

// schemaKeysAt resolves the schema document down an instance path and returns the
// legal property names at that location. It follows "properties", array "items",
// and local "$ref" (#/…) — enough for this project's self-contained schema.
func schemaKeysAt(schemaDoc any, seg []string) []string {
	cur := deref(schemaDoc, schemaDoc)
	for _, s := range seg {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if _, err := strconv.Atoi(s); err == nil {
			// array index → descend into items
			cur = deref(schemaDoc, m["items"])
			continue
		}
		props, _ := m["properties"].(map[string]any)
		if props == nil {
			return nil
		}
		cur = deref(schemaDoc, props[s])
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil
	}
	props, _ := m["properties"].(map[string]any)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	return keys
}

// deref follows a local "$ref" ("#/$defs/node") one or more levels within root.
func deref(root, v any) any {
	for {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref, ok := m["$ref"].(string)
		if !ok || len(ref) < 2 || ref[0] != '#' {
			return v
		}
		v = resolvePointer(root, ref[1:]) // strip leading '#'
		if v == nil {
			return nil
		}
	}
}

// resolvePointer resolves a JSON pointer ("/$defs/node") within root.
func resolvePointer(root any, ptr string) any {
	cur := root
	for _, part := range splitPointer(ptr) {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func splitPointer(ptr string) []string {
	var out []string
	for _, p := range splitSlash(ptr) {
		if p == "" {
			continue
		}
		// unescape ~1 -> / and ~0 -> ~ per RFC6901
		p = replaceAll(p, "~1", "/")
		p = replaceAll(p, "~0", "~")
		out = append(out, p)
	}
	return out
}

// small local helpers (avoid pulling strings into this file's import set twice).
func splitSlash(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func replaceAll(s, old, new string) string {
	var b []byte
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			b = append(b, new...)
			i += len(old)
		} else {
			b = append(b, s[i])
			i++
		}
	}
	return string(b)
}

// nearestKey returns the legal key closest to bad by Levenshtein distance, within
// a small threshold (so a wild typo suggests nothing). "" if none is close enough.
func nearestKey(bad string, legal []string) string {
	best, bestDist := "", 1<<30
	for _, k := range legal {
		d := levenshtein(bad, k)
		if d < bestDist {
			best, bestDist = k, d
		}
	}
	// suggest only when reasonably close: within a third of the longer length, min 2.
	limit := len(bad)/3 + 1
	if limit < 2 {
		limit = 2
	}
	if bestDist <= limit {
		return best
	}
	return ""
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
