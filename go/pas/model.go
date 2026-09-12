package pas

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

// FHIR bases served by the handler: {prefix}/v221/fhir and {prefix}/v201/fhir.
const (
	V221 = "v221"
	V201 = "v201"
)

// Versions lists the served PAS versions in suite order.
var Versions = []string{V221, V201}

const (
	pasSD         = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/"
	x12           = "https://codesystem.x12.org/005010/"
	pasTemp       = "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes"
	loincSystem   = "http://loinc.org"
	npiSystem     = "http://hl7.org/fhir/sid/us-npi"
	darURL        = "http://hl7.org/fhir/StructureDefinition/data-absent-reason"
	pasTopic      = "http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic"
	backportSD    = "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/"
	payerSID      = "https://pas.nirmitee.example/fhir/sid/"
	instantLayout = "2006-01-02T15:04:05Z"
	dateLayout    = "2006-01-02"
)

// object is a generic JSON object. Objects built by this package record their
// key order in fhir.OrderKey so responses serialize in a stable, readable order.
type object = map[string]any

// obj builds an object from alternating keys and values, keeping that order.
func obj(kv ...any) object {
	m := make(object, len(kv)/2+1)
	keys := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		if _, dup := m[k]; !dup {
			keys = append(keys, k)
		}
		m[k] = kv[i+1]
	}
	m[fhir.OrderKey] = keys
	return m
}

// set assigns m[k], appending k to the recorded key order when it is new.
func set(m object, k string, v any) {
	if _, ok := m[k]; !ok {
		m[fhir.OrderKey] = append(orderedKeys(m), k)
	}
	m[k] = v
}

// orderedKeys returns the keys in serialization order: recorded order first,
// then any other keys sorted. The result is always a new slice.
func orderedKeys(m object) []string {
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	if order, ok := m[fhir.OrderKey].([]string); ok {
		for _, k := range order {
			if _, present := m[k]; present && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	var rest []string
	for k := range m {
		if k != fhir.OrderKey && !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func asObj(v any) object {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any { return fhir.List(v) }

func get(v any, path ...string) any { return fhir.Get(v, path...) }

func str(v any, path ...string) string { return fhir.String(v, path...) }

func clone(v any) any { return fhir.Clone(v) }

// present mirrors JSON truthiness: null, "", false, 0, [] and {} are absent.
func present(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case []any:
		return len(t) > 0
	case map[string]any:
		for k := range t {
			if k != fhir.OrderKey {
				return true
			}
		}
		return false
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	default:
		return true
	}
}

// extValue returns the value[x] (key) of the first top-level extension with url.
func extValue(r any, url, key string) any {
	for _, e := range list(get(r, "extension")) {
		if str(e, "url") == url {
			return get(e, key)
		}
	}
	return nil
}

// firstCode returns coding[0].code of a CodeableConcept.
func firstCode(cc any) string {
	codings := list(get(cc, "coding"))
	if len(codings) == 0 {
		return ""
	}
	return str(codings[0], "code")
}

// seqKey normalizes a sequence number for comparisons and map keys.
func seqKey(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case string:
		return "s:" + t
	default:
		return fmt.Sprint(t)
	}
}

// seqText renders a sequence number inside identifiers and templates.
func seqText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case json.Number:
		return t.String()
	case string:
		return t
	default:
		return strings.TrimPrefix(seqKey(t), "s:")
	}
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

var baseURLPattern = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*)://([^/:?#]+)(?::\d+)?([^?#]*)`)

// extractBaseURL mirrors the test kit's base URL derivation from an entry
// fullUrl: scheme and host plus the path without its last two segments.
func extractBaseURL(u string) string {
	m := baseURLPattern.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	base := m[1] + "://" + m[2]
	if segs := strings.Split(m[3], "/"); len(segs) > 2 {
		base += strings.Join(segs[:len(segs)-2], "/")
	}
	return base
}

// outcome builds a single-issue OperationOutcome.
func outcome(severity, code, message, expression string) object {
	issue := obj("severity", severity, "code", code, "details", obj("text", message), "diagnostics", message)
	if expression != "" {
		set(issue, "expression", []any{expression})
	}
	return obj("resourceType", "OperationOutcome", "issue", []any{issue})
}

// outcomeText returns the first issue's diagnostics.
func outcomeText(oo object) string {
	issues := list(oo["issue"])
	if len(issues) == 0 {
		return ""
	}
	return str(issues[0], "diagnostics")
}
