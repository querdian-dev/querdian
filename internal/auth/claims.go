package auth

import (
	"fmt"
	"strings"
)

// lookupPath resolves a dotted claim path such as "realm_access.roles". At
// each level a literal key containing dots (e.g. "a.b") is tried before
// splitting, so claims whose names contain dots stay addressable.
func lookupPath(claims map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	if v, ok := claims[path]; ok {
		return v, true
	}
	parts := strings.Split(path, ".")
	for i := 1; i < len(parts); i++ {
		head := strings.Join(parts[:i], ".")
		m, ok := claims[head].(map[string]any)
		if !ok {
			continue
		}
		if r, ok := lookupPath(m, strings.Join(parts[i:], ".")); ok {
			return r, true
		}
	}
	return nil, false
}

// extractUsername returns the non-empty string at path.
func extractUsername(claims map[string]any, path string) (string, error) {
	v, ok := lookupPath(claims, path)
	if !ok {
		return "", newError(ReasonMissingUsername, nil)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", newError(ReasonMissingUsername, nil)
	}
	return s, nil
}

// extractGroups returns the groups at path. The claim may be a string (one
// group) or an array of strings. A missing claim yields no groups; a claim of
// any other shape is an error (fail closed).
func extractGroups(claims map[string]any, path string) ([]string, error) {
	v, ok := lookupPath(claims, path)
	if !ok || v == nil {
		return nil, nil
	}
	switch g := v.(type) {
	case string:
		if g == "" {
			return nil, nil
		}
		return []string{g}, nil
	case []any:
		out := make([]string, 0, len(g))
		for _, e := range g {
			s, ok := e.(string)
			if !ok {
				return nil, newError(ReasonInvalidToken, fmt.Errorf("groups claim %q has non-string element", path))
			}
			if s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	default:
		return nil, newError(ReasonInvalidToken, fmt.Errorf("groups claim %q has unsupported type %T", path, v))
	}
}
