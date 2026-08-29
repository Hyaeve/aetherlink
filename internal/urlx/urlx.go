// Package urlx normalizes the raw URLs found inside .strm files.
//
// STRM generators write human readable paths straight into the file, so the
// contents are frequently not valid URLs: they contain spaces, CJK characters,
// "&" inside a path segment and sometimes a query string that is really a
// display filename (the 115 pick-code style "?/001.xxx.m4a"). Go refuses to
// build a request from those strings, so every pointer is normalized here
// before it is used or handed to a player through a 302 redirect.
package urlx

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Kind is a coarse classification of a STRM target, used for stats and UI hints.
type Kind string

const (
	KindPickCode115 Kind = "pickcode115"
	KindOpenList    Kind = "openlist"
	KindHTTP        Kind = "http"
	KindLocal       Kind = "local"
	KindUnknown     Kind = "unknown"
)

var errEmpty = errors.New("empty target")

// HasScheme reports whether raw starts with an absolute URL scheme. A single
// leading letter is treated as a Windows drive letter ("D:\Media") rather than
// a scheme, because STRM pointers frequently contain Windows paths.
func HasScheme(raw string) bool {
	i := strings.Index(raw, ":")
	if i < 2 {
		return false
	}
	for pos, r := range raw[:i] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case pos > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// Clean strips BOM markers, wrapping quotes and surrounding whitespace.
func Clean(raw string) string {
	out := strings.TrimPrefix(raw, "\ufeff")
	out = strings.Trim(out, " \t\r\n")
	if len(out) >= 2 {
		if (out[0] == '"' && out[len(out)-1] == '"') || (out[0] == '\'' && out[len(out)-1] == '\'') {
			out = out[1 : len(out)-1]
		}
	}
	return strings.Trim(out, " \t\r\n")
}

// Normalize percent-encodes a raw STRM URL without touching sequences that are
// already encoded. The returned string is safe to use as an HTTP request target
// and as a Location header value.
func Normalize(raw string) (string, error) {
	cleaned := Clean(raw)
	if cleaned == "" {
		return "", errEmpty
	}
	if !utf8.ValidString(cleaned) {
		return "", errors.New("target is not valid UTF-8")
	}
	if !HasScheme(cleaned) {
		return "", fmt.Errorf("target %q has no URL scheme", truncate(cleaned))
	}

	schemeEnd := strings.Index(cleaned, "://")
	if schemeEnd < 0 {
		return "", fmt.Errorf("unsupported URL %q", truncate(cleaned))
	}
	scheme := strings.ToLower(cleaned[:schemeEnd])
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme %q", scheme)
	}
	rest := cleaned[schemeEnd+3:]

	authority := rest
	remainder := ""
	if idx := strings.IndexAny(rest, "/?"); idx >= 0 {
		authority = rest[:idx]
		remainder = rest[idx:]
	}
	if authority == "" {
		return "", errors.New("target has no host")
	}

	rawPath := remainder
	rawQuery := ""
	if idx := strings.Index(remainder, "?"); idx >= 0 {
		rawPath = remainder[:idx]
		rawQuery = remainder[idx+1:]
	}
	if rawPath == "" {
		rawPath = "/"
	}

	var builder strings.Builder
	builder.WriteString(scheme)
	builder.WriteString("://")
	builder.WriteString(authority)
	builder.WriteString(encodePath(rawPath))
	if rawQuery != "" {
		builder.WriteString("?")
		builder.WriteString(encodeQuery(rawQuery))
	}

	normalized := builder.String()
	if _, err := url.Parse(normalized); err != nil {
		return "", fmt.Errorf("normalized URL is still invalid: %w", err)
	}
	return normalized, nil
}

// Parse normalizes raw and returns the parsed URL.
func Parse(raw string) (*url.URL, error) {
	normalized, err := Normalize(raw)
	if err != nil {
		return nil, err
	}
	return url.Parse(normalized)
}

// Classify labels a normalized URL so the UI can show which STRM flavour the
// pointer came from.
func Classify(normalized string) Kind {
	parsed, err := url.Parse(normalized)
	if err != nil {
		return KindUnknown
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(segments) >= 2 && segments[0] == "d" {
		if len(segments) == 2 && isPickCode(segments[1]) {
			return KindPickCode115
		}
		return KindOpenList
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		return KindHTTP
	}
	return KindUnknown
}

// isPickCode matches the flat "/d/<pickcode>.<ext>" form emitted by 115 STRM
// generators, e.g. /d/bi6jeznun2rvu88v6.m4a.
func isPickCode(segment string) bool {
	dot := strings.LastIndex(segment, ".")
	if dot <= 0 || dot == len(segment)-1 {
		return false
	}
	name := segment[:dot]
	if len(name) < 12 || len(name) > 40 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// IsPrivateHost reports whether the URL host is a loopback, link-local or
// RFC1918 address. Those hosts are the common case for self-hosted STRM
// backends (openlist, 115 proxies) and are allowed even when public redirect
// targets are restricted.
func IsPrivateHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if ip == nil {
		return strings.EqualFold(host, "localhost")
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

const pathSafe = "-_.~!$&'()*+,;=:@"

const querySafe = "-_.~!$'()*+,;:@/?&="

func encodePath(path string) string {
	return encodeWith(path, pathSafe, "/")
}

func encodeQuery(query string) string {
	return encodeWith(query, querySafe, "")
}

// encodeWith percent-encodes everything outside the unreserved set plus the
// provided extra characters, while preserving existing %XX escapes so already
// encoded pointers survive a round trip unchanged.
func encodeWith(input, safe, literal string) string {
	var builder strings.Builder
	builder.Grow(len(input))
	for i := 0; i < len(input); i++ {
		c := input[i]
		if c == '%' && i+2 < len(input) && isHex(input[i+1]) && isHex(input[i+2]) {
			builder.WriteString(strings.ToUpper(input[i : i+3]))
			i += 2
			continue
		}
		if strings.IndexByte(literal, c) >= 0 || isUnreserved(c) || strings.IndexByte(safe, c) >= 0 {
			builder.WriteByte(c)
			continue
		}
		fmt.Fprintf(&builder, "%%%02X", c)
	}
	return builder.String()
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~'
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func truncate(value string) string {
	if len(value) <= 120 {
		return value
	}
	return value[:120] + "..."
}
