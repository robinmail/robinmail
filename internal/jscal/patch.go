package jscal

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ApplyPatch applies a PatchObject (RFC 8620 5.3, RFC 8984 1.4.9) to a
// copy of obj: each key is a path of member names separated by "/" (with
// the escapes of JSON Pointer), the value replaces what is there, null
// removes it. Every path but the last member must exist. A patch that
// sets a path and also a path inside it is invalid.
func ApplyPatch(obj map[string]any, patch map[string]any) (map[string]any, error) {
	out := DeepCopy(obj).(map[string]any)
	for key := range patch {
		for other := range patch {
			if other != key && strings.HasPrefix(other, key+"/") {
				return nil, fmt.Errorf("the patch sets both %q and %q", key, other)
			}
		}
	}
	for key, value := range patch {
		if key == "" {
			return nil, fmt.Errorf("an empty path")
		}
		parts := strings.Split(key, "/")
		for i, p := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
		}
		cur := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("the path %q does not exist", key)
			}
			cur = next
		}
		last := parts[len(parts)-1]
		if value == nil {
			delete(cur, last)
		} else {
			cur[last] = DeepCopy(value)
		}
	}
	return out, nil
}

// DeepCopy copies a JSON value.
func DeepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = DeepCopy(e)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = DeepCopy(e)
		}
		return l
	}
	return v
}

// Normalize gives a JSON value the types encoding/json reads it as, so
// that values built in Go compare equal to values read from JSON.
func Normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}
