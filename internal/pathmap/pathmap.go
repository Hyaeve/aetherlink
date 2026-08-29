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
	"os"
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

// conventionalRoots are the container mount points STRM setups use by
// convention. They are only probed when the translated path does not exist and
// no roots were configured, so a user who mounted the library at the obvious
// place does not have to write a path mapping by hand.
var conventionalRoots = []string{"/NetDisk", "/audiobooks", "/media", "/strm", "/data"}

// Locate returns the readable container path for an upstream media path.
//
// The happy path is an identical mount on both sides, which Translate already
// handles. When that file is not present, the upstream path is re-anchored under
// each candidate root by dropping leading segments one at a time: an upstream
// path of /audiobooks/Set/Read/Book/001.strm will also be found at
// /NetDisk/Book/001.strm. Only stat calls are used — no directory walking — so
// the fallback stays cheap enough to run on a cache miss.
//
// found reports whether an existing file was located. When it is false the
// caller gets the translated path back so error messages still name the path the
// user most likely meant.
func (m *Mapper) Locate(upstreamPath string) (resolved string, found bool, err error) {
	translated := m.Translate(upstreamPath)
	if translated == "" {
		return "", false, fmt.Errorf("empty media path")
	}
	if strings.Contains(translated, "..") {
		return "", false, fmt.Errorf("path %q contains a parent traversal segment", translated)
	}
	// 只有落在白名单内的路径才会被 stat/读取，这一条永不放宽。
	if m.IsWithinRoots(translated) && isReadableFile(translated) {
		return translated, true, nil
	}

	normalized := Normalize(upstreamPath)
	segments := strings.Split(strings.TrimPrefix(normalized, "/"), "/")
	for _, root := range m.candidateRoots() {
		// 从最长的后缀开始，先命中的就是最具体的匹配。
		for start := 0; start < len(segments); start++ {
			candidate := path.Join(root, path.Join(segments[start:]...))
			if !m.IsWithinRoots(candidate) {
				continue
			}
			if isReadableFile(candidate) {
				return candidate, true, nil
			}
		}
	}
	// 找不到不是配置错误：调用方会退回透传，让上游自己去读这个文件。
	return translated, false, nil
}

// candidateRoots lists where a pointer file may live inside this container:
// the configured roots first, the rewrite targets next, and the conventional
// mount points only when nothing was configured at all.
func (m *Mapper) candidateRoots() []string {
	roots := make([]string, 0, len(m.roots)+len(m.rules)+len(conventionalRoots))
	seen := map[string]bool{}
	add := func(candidate string) {
		normalized := Normalize(candidate)
		if normalized == "" || seen[normalized] {
			return
		}
		seen[normalized] = true
		roots = append(roots, normalized)
	}
	for _, root := range m.roots {
		add(root)
	}
	for _, rule := range m.rules {
		add(rule.To)
	}
	if len(m.roots) == 0 {
		for _, root := range conventionalRoots {
			add(root)
		}
	}
	return roots
}

func isReadableFile(candidate string) bool {
	info, err := os.Stat(candidate)
	return err == nil && info.Mode().IsRegular()
}
