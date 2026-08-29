// Package strm reads .strm pointer files and classifies their targets.
package strm

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aetherlink/aetherlink/internal/pathmap"
	"github.com/aetherlink/aetherlink/internal/urlx"
)

// maxPointerBytes caps how much of a .strm file is read. Real pointers are a
// single line; anything larger is a malformed or hostile file.
const maxPointerBytes = 64 * 1024

// TargetType distinguishes remote URLs from container-local files.
type TargetType string

const (
	TargetRemote TargetType = "remote"
	TargetLocal  TargetType = "local"
)

// Target is a resolved .strm pointer.
type Target struct {
	Type TargetType `json:"type"`
	// URL is the normalized absolute URL for remote targets.
	URL string `json:"url,omitempty"`
	// Path is the container path for local targets.
	Path string `json:"path,omitempty"`
	// Raw is the untouched pointer content, useful for debugging in the UI.
	Raw string `json:"raw"`
	// Kind labels the STRM flavour (115 pick code, openlist, plain http...).
	Kind urlx.Kind `json:"kind"`
	// Filename is the display filename derived from the pointer.
	Filename string `json:"filename"`
}

// IsStrmPath reports whether a path points at a .strm pointer file.
func IsStrmPath(candidate string) bool {
	return strings.EqualFold(filepath.Ext(candidate), ".strm")
}

var errEmptyPointer = errors.New("strm file is empty")

// Read parses the pointer at strmPath. Relative and absolute local targets are
// validated against mapper roots so a hostile pointer cannot turn AetherLink
// into an arbitrary file reader.
func Read(strmPath string, mapper *pathmap.Mapper) (*Target, error) {
	if !IsStrmPath(strmPath) {
		return nil, fmt.Errorf("%q is not a .strm file", strmPath)
	}
	file, err := os.Open(strmPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%q is a directory", strmPath)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxPointerBytes))
	if err != nil {
		return nil, err
	}
	return Parse(string(raw), strmPath, mapper)
}

// ParseURL builds a remote Target from a URL that an upstream已经替我们解析好了
// （Emby 会把 .strm 的内容作为 MediaSource.Path 返回）。它走的是同一套归一化
// 逻辑，所以 115 pick code 与 openlist 的中文、空格、%26 都能得到一致处理。
func ParseURL(rawURL string) (*Target, error) {
	normalized, err := urlx.Normalize(rawURL)
	if err != nil {
		return nil, err
	}
	return &Target{
		Type:     TargetRemote,
		URL:      normalized,
		Raw:      urlx.Clean(rawURL),
		Kind:     urlx.Classify(normalized),
		Filename: remoteFilename(normalized, ""),
	}, nil
}

// Parse turns pointer contents into a Target. strmPath is only used to resolve
// relative local targets and may be empty for remote-only parsing.
func Parse(contents, strmPath string, mapper *pathmap.Mapper) (*Target, error) {
	pointer := firstMeaningfulLine(contents)
	if pointer == "" {
		return nil, errEmptyPointer
	}

	if urlx.HasScheme(pointer) {
		normalized, err := urlx.Normalize(pointer)
		if err != nil {
			return nil, err
		}
		return &Target{
			Type:     TargetRemote,
			URL:      normalized,
			Raw:      pointer,
			Kind:     urlx.Classify(normalized),
			Filename: remoteFilename(normalized, strmPath),
		}, nil
	}

	local := pointer
	if !isAbsoluteLocal(local) {
		if strmPath == "" {
			return nil, fmt.Errorf("relative strm target %q cannot be resolved without a base path", pointer)
		}
		local = path.Join(path.Dir(pathmap.Normalize(strmPath)), pathmap.Normalize(local))
	}
	resolved := pathmap.Normalize(local)
	if mapper != nil {
		checked, err := mapper.Check(resolved)
		if err != nil {
			return nil, err
		}
		resolved = checked
	}
	stat, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if stat.IsDir() {
		return nil, fmt.Errorf("strm target %q is not a file", resolved)
	}
	return &Target{
		Type:     TargetLocal,
		Path:     resolved,
		Raw:      pointer,
		Kind:     urlx.KindLocal,
		Filename: path.Base(resolved),
	}, nil
}

// firstMeaningfulLine returns the first non-empty, non-comment line. Some
// generators prepend "#EXTINF"-style comments copied from m3u templates.
func firstMeaningfulLine(contents string) string {
	for _, line := range strings.Split(contents, "\n") {
		cleaned := urlx.Clean(line)
		if cleaned == "" || strings.HasPrefix(cleaned, "#") {
			continue
		}
		return cleaned
	}
	return ""
}

func isAbsoluteLocal(candidate string) bool {
	if strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "\\\\") {
		return true
	}
	// Windows drive letter, e.g. D:\Media
	if len(candidate) >= 3 && candidate[1] == ':' && (candidate[2] == '\\' || candidate[2] == '/') {
		return true
	}
	return false
}

// remoteFilename picks the best display name for a remote target. Pick-code
// STRM URLs carry the real filename in the query string
// ("/d/abc123.m4a?/001.总序.m4a"), so that wins over the opaque path segment.
func remoteFilename(normalizedURL, strmPath string) string {
	parsed, err := urlx.Parse(normalizedURL)
	if err == nil {
		if query := parsed.RawQuery; query != "" {
			decoded, decodeErr := decodeQueryFilename(query)
			if decodeErr == nil && decoded != "" {
				return decoded
			}
		}
		if base := path.Base(parsed.Path); base != "" && base != "/" && base != "." {
			return base
		}
	}
	if strmPath != "" {
		base := path.Base(pathmap.Normalize(strmPath))
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	return ""
}

func decodeQueryFilename(rawQuery string) (string, error) {
	candidate := rawQuery
	if idx := strings.LastIndex(candidate, "/"); idx >= 0 {
		candidate = candidate[idx+1:]
	}
	unescaped, err := url.PathUnescape(candidate)
	if err != nil {
		return "", err
	}
	unescaped = strings.TrimSpace(unescaped)
	if unescaped == "" || strings.ContainsAny(unescaped, "=&") {
		return "", nil
	}
	if filepath.Ext(unescaped) == "" {
		return "", nil
	}
	return unescaped, nil
}
