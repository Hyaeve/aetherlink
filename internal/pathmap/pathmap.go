// Package pathmap translates media paths reported by an upstream server into
// paths that exist inside the AetherLink container, and enforces that resolved
// local files stay inside an allow-listed root.
//
// The upstream (Audiobookshelf or Emby) and AetherLink usually mount the same
// library under different container paths, so a pointer such as
// /audiobooks/Book/001.strm may live at /NetDisk/115-Strm/Book/001.strm here.
package pathmap

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Rule is a single from -> to prefix rewrite.
type Rule struct {
	From string
	To   string
}

// Mapper applies rewrite rules and root containment checks.
type Mapper struct {
	rules []Rule
	roots []string
}

// New builds a Mapper. Rules are sorted longest-prefix-first so that nested
// mappings resolve deterministically regardless of config order.
func New(rules []Rule, roots []string) *Mapper {
	cleanedRules := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		from := Normalize(rule.From)
		to := Normalize(rule.To)
		if from == "" || to == "" {
			continue
		}
		cleanedRules = append(cleanedRules, Rule{From: from, To: to})
	}
	sort.SliceStable(cleanedRules, func(i, j int) bool {
		return len(cleanedRules[i].From) > len(cleanedRules[j].From)
	})

	cleanedRoots := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		normalized := Normalize(root)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		cleanedRoots = append(cleanedRoots, normalized)
	}
	return &Mapper{rules: cleanedRules, roots: cleanedRoots}
}

// Rules returns the active rewrite rules.
func (m *Mapper) Rules() []Rule { return append([]Rule(nil), m.rules...) }

// Roots returns the allow-listed container roots.
func (m *Mapper) Roots() []string { return append([]string(nil), m.roots...) }

// Normalize converts a Windows or POSIX path into a cleaned POSIX path.
func Normalize(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	slashed := strings.ReplaceAll(trimmed, "\\", "/")
	cleaned := path.Clean(slashed)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// Translate rewrites an upstream path to a container path. When no rule
// matches, the normalized input is returned unchanged so that identical mounts
// need no configuration.
func (m *Mapper) Translate(upstreamPath string) string {
	normalized := Normalize(upstreamPath)
	if normalized == "" {
		return ""
	}
	for _, rule := range m.rules {
		if normalized == rule.From {
			return rule.To
		}
		if strings.HasPrefix(normalized, rule.From+"/") {
			return path.Join(rule.To, strings.TrimPrefix(normalized, rule.From+"/"))
		}
	}
	return normalized
}

// IsWithinRoots reports whether candidate lives inside one of the allow-listed
// roots. With no roots configured every path is allowed, which matches a
// single-purpose container that only mounts library data.
func (m *Mapper) IsWithinRoots(candidate string) bool {
	if len(m.roots) == 0 {
		return true
	}
	normalized := Normalize(candidate)
	if normalized == "" {
		return false
	}
	for _, root := range m.roots {
		if normalized == root || strings.HasPrefix(normalized, strings.TrimRight(root, "/")+"/") {
			return true
		}
	}
	return false
}

// Check translates and validates in one step.
func (m *Mapper) Check(upstreamPath string) (string, error) {
	translated := m.Translate(upstreamPath)
	if translated == "" {
		return "", fmt.Errorf("empty media path")
	}
	if strings.Contains(translated, "..") {
		return "", fmt.Errorf("path %q contains a parent traversal segment", translated)
	}
	if !m.IsWithinRoots(translated) {
		return "", fmt.Errorf("path %q is outside the allowed strm roots %v", translated, m.roots)
	}
	return translated, nil
}
