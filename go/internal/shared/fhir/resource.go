// Package fhir provides generic JSON helpers for FHIR R4 resources and a
// minimal REST client for reading from a client's FHIR server.
package fhir

import (
	"net/url"
	"sort"
	"strings"
)

// Type returns resourceType.
func Type(r map[string]any) string {
	s, _ := r["resourceType"].(string)
	return s
}

// ID returns the logical id.
func ID(r map[string]any) string {
	s, _ := r["id"].(string)
	return s
}

// AsResource returns v as a resource when it is an object with a resourceType.
func AsResource(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok || Type(m) == "" {
		return nil, false
	}
	return m, true
}

// Get walks object keys and returns the value found, or nil.
func Get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// String walks object keys and returns a string value, or "".
func String(v any, path ...string) string {
	s, _ := Get(v, path...).(string)
	return s
}

// List returns v as a JSON array, or nil.
func List(v any) []any {
	l, _ := v.([]any)
	return l
}

// Codings returns the coding objects of a CodeableConcept.
func Codings(cc any) []map[string]any {
	var out []map[string]any
	for _, c := range List(Get(cc, "coding")) {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// BundleResources returns entry[].resource of a Bundle.
func BundleResources(bundle map[string]any) []map[string]any {
	if Type(bundle) != "Bundle" {
		return nil
	}
	var out []map[string]any
	for _, e := range List(bundle["entry"]) {
		if r, ok := AsResource(Get(e, "resource")); ok {
			out = append(out, r)
		}
	}
	return out
}

// ParseReference splits "Type/id", "base/Type/id" or ".../Type/id/_history/n".
func ParseReference(ref string) (typ, id string, ok bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") || strings.HasPrefix(ref, "urn:") {
		return "", "", false
	}
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" {
		ref = u.Path
	} else if i := strings.IndexAny(ref, "?#"); i >= 0 {
		ref = ref[:i]
	}
	parts := strings.Split(strings.Trim(ref, "/"), "/")
	if n := len(parts); n >= 4 && parts[n-2] == "_history" {
		parts = parts[:n-2]
	}
	n := len(parts)
	if n < 2 {
		return "", "", false
	}
	typ, id = parts[n-2], parts[n-1]
	if typ == "" || id == "" || typ[0] < 'A' || typ[0] > 'Z' {
		return "", "", false
	}
	return typ, id, true
}

// Clone deep-copies a JSON value.
func Clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = Clone(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Clone(val)
		}
		return out
	default:
		return t
	}
}

// CloneResource deep-copies a resource.
func CloneResource(r map[string]any) map[string]any {
	if r == nil {
		return nil
	}
	return Clone(r).(map[string]any)
}

// HasExtension reports whether the resource has a top-level extension with url.
func HasExtension(r map[string]any, extURL string) bool {
	for _, e := range List(r["extension"]) {
		if String(e, "url") == extURL {
			return true
		}
	}
	return false
}

// FindInValue searches a resource or Bundle for a resource of typ and id.
func FindInValue(v any, typ, id string) map[string]any {
	r, ok := AsResource(v)
	if !ok {
		return nil
	}
	if Type(r) == typ && ID(r) == id {
		return r
	}
	for _, e := range BundleResources(r) {
		if Type(e) == typ && ID(e) == id {
			return e
		}
	}
	return nil
}

// SortedKeys returns the keys of m in lexical order, without OrderKey.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != OrderKey {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
