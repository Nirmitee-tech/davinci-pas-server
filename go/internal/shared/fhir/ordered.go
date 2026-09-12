package fhir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// OrderKey is a hidden map key holding an object's original key order. It is
// set by DecodeOrdered and honored (and never written) by MarshalOrdered.
//
// Echoed resources must keep the client's key order: conformance tools
// compare resources after sorting arrays by each element's serialized JSON,
// so alphabetical keys can reorder array elements and look like a change.
const OrderKey = "\x00order"

// DecodeOrdered decodes JSON into generic values (map[string]any, []any,
// json.Number, string, bool, nil), recording key order in OrderKey.
func DecodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func decodeOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		var keys []string
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, fmt.Errorf("invalid object key %v", kt)
			}
			val, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			if _, dup := m[key]; !dup {
				keys = append(keys, key)
			}
			m[key] = val
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		if len(keys) > 1 {
			m[OrderKey] = keys
		}
		return m, nil
	case '[':
		arr := []any{}
		for dec.More() {
			val, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected delimiter %v", delim)
}

// MarshalOrdered encodes generic JSON values without HTML escaping. Objects
// write their recorded keys first, in order, then any other keys sorted.
func MarshalOrdered(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeOrdered(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeOrdered(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		buf.WriteByte('{')
		first := true
		writeMember := func(k string, val any) error {
			if !first {
				buf.WriteByte(',')
			}
			first = false
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			return writeOrdered(buf, val)
		}
		done := map[string]bool{OrderKey: true}
		if order, ok := t[OrderKey].([]string); ok {
			for _, k := range order {
				if val, present := t[k]; present && !done[k] {
					done[k] = true
					if err := writeMember(k, val); err != nil {
						return err
					}
				}
			}
		}
		rest := make([]string, 0, len(t))
		for k := range t {
			if !done[k] {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range rest {
			if err := writeMember(k, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, val := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeOrdered(buf, val); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case string:
		return writeString(buf, t)
	case json.Number:
		if t == "" {
			buf.WriteString("0")
			return nil
		}
		buf.WriteString(t.String())
		return nil
	default:
		var tmp bytes.Buffer
		enc := json.NewEncoder(&tmp)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(t); err != nil {
			return err
		}
		buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
		return nil
	}
}

func writeString(buf *bytes.Buffer, s string) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	return nil
}
