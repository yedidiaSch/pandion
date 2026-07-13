// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

// msgPrinter renders a jsonschema ErrorKind's message; the kinds require a
// non-nil *message.Printer.
var msgPrinter = message.NewPrinter(language.English)

// SchemaErrors is a human-friendly rendering of a jsonschema validation failure
// (P2.1): each problem carries a YAML line/column, a dotted path, and a message —
// with a "did you mean" suggestion for unknown fields — instead of the validator's
// raw JSON-pointer output. File is filled in by Load so messages read
// `cluster.yaml:12:5: nodes[0]: unknown field "runn" (did you mean "run"?)`.
type SchemaErrors struct {
	File   string
	Issues []FieldIssue
}

// FieldIssue is one validation problem located in the source YAML.
type FieldIssue struct {
	Line, Col int    // 1-based; 0 when the location couldn't be resolved
	Path      string // dotted/indexed instance path, e.g. "nodes[0].run"
	Message   string
	unknown   bool // true for an "unknown field" issue (used to suppress its descendants)
}

func (e *SchemaErrors) Error() string {
	if len(e.Issues) == 0 {
		return "invalid cluster config"
	}
	var b strings.Builder
	for i, is := range e.Issues {
		if i > 0 {
			b.WriteByte('\n')
		}
		loc := e.File
		if is.Line > 0 {
			if loc != "" {
				loc += ":"
			}
			loc += strconv.Itoa(is.Line)
			if is.Col > 0 {
				loc += ":" + strconv.Itoa(is.Col)
			}
		} else if loc == "" {
			loc = "cluster.yaml"
		}
		path := is.Path
		if path == "" {
			path = "(root)"
		}
		fmt.Fprintf(&b, "%s: %s: %s", loc, path, is.Message)
	}
	return b.String()
}

// translateSchemaError turns a *jsonschema.ValidationError tree into SchemaErrors,
// resolving YAML locations against src and legal-key suggestions against the schema
// document. Returns nil if verr isn't a *jsonschema.ValidationError.
func translateSchemaError(src []byte, err error) *SchemaErrors {
	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return nil
	}
	var root yaml.Node
	_ = yaml.Unmarshal(src, &root)

	var schemaDoc any
	_ = json.Unmarshal(schemaJSON, &schemaDoc)

	var all []FieldIssue
	for _, leaf := range leafErrors(verr) {
		all = append(all, buildIssue(&root, schemaDoc, leaf))
	}
	out := &SchemaErrors{Issues: dedupeIssues(all)}
	if len(out.Issues) == 0 {
		out.Issues = append(out.Issues, FieldIssue{Message: verr.Error()})
	}
	// stable order: by line, then path.
	sort.SliceStable(out.Issues, func(i, j int) bool {
		if out.Issues[i].Line != out.Issues[j].Line {
			return out.Issues[i].Line < out.Issues[j].Line
		}
		return out.Issues[i].Path < out.Issues[j].Path
	})
	return out
}

// leafErrors returns the most specific errors in the tree (those with no nested
// causes) — the ones a human actually needs to fix.
func leafErrors(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leafErrors(c)...)
	}
	return out
}

// buildIssue locates one validation error in the YAML and crafts its message.
func buildIssue(root *yaml.Node, schemaDoc any, e *jsonschema.ValidationError) FieldIssue {
	seg := e.InstanceLocation

	// Unknown field, form 1: `additionalProperties: [names]` — the extra keys are
	// named in the kind and the location is the containing object.
	if ap, ok := e.ErrorKind.(*kind.AdditionalProperties); ok && len(ap.Properties) > 0 {
		bad := ap.Properties[0]
		line, col := locateKey(root, seg, bad)
		return unknownFieldIssue(schemaDoc, seg, bad, line, col)
	}

	// Unknown field, form 2: `additionalProperties: false` — the validator reports a
	// FalseSchema at the offending child path; the last segment IS the unknown key,
	// and its legal siblings come from the parent object's schema.
	if _, ok := e.ErrorKind.(*kind.FalseSchema); ok && len(seg) > 0 {
		bad := seg[len(seg)-1]
		parent := seg[:len(seg)-1]
		line, col := locateKey(root, parent, bad)
		return unknownFieldIssue(schemaDoc, parent, bad, line, col)
	}

	line, col := locateValue(root, seg)
	return FieldIssue{Line: line, Col: col, Path: humanPath(seg), Message: e.ErrorKind.LocalizedString(msgPrinter)}
}

// unknownFieldIssue builds the "unknown field X (did you mean Y?)" issue given the
// containing object's path, the offending key, and its located line/col.
func unknownFieldIssue(schemaDoc any, objPath []string, bad string, line, col int) FieldIssue {
	legal := schemaKeysAt(schemaDoc, objPath)
	msg := fmt.Sprintf("unknown field %q", bad)
	if s := nearestKey(bad, legal); s != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", s)
	}
	return FieldIssue{Line: line, Col: col, Path: humanPath(append(append([]string{}, objPath...), bad)), Message: msg, unknown: true}
}

// dedupeIssues removes exact-duplicate paths and any issue nested UNDER an
// unknown-field issue — reporting "security is unknown" is enough; the cascade of
// errors inside that misplaced/typo'd block is noise.
func dedupeIssues(in []FieldIssue) []FieldIssue {
	var out []FieldIssue
	seen := map[string]bool{}
	for _, is := range in {
		if is.Path != "" && seen[is.Path] {
			continue
		}
		descendant := false
		for _, other := range in {
			if !other.unknown || other.Path == is.Path || other.Path == "" {
				continue
			}
			if strings.HasPrefix(is.Path, other.Path+".") || strings.HasPrefix(is.Path, other.Path+"[") {
				descendant = true
				break
			}
		}
		if descendant {
			continue
		}
		seen[is.Path] = true
		out = append(out, is)
	}
	return out
}

// humanPath renders instance-location segments as a dotted/indexed path:
// ["nodes","0","run"] -> "nodes[0].run".
func humanPath(seg []string) string {
	var b strings.Builder
	for _, s := range seg {
		if _, err := strconv.Atoi(s); err == nil {
			fmt.Fprintf(&b, "[%s]", s)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s)
	}
	return b.String()
}
