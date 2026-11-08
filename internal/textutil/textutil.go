// Package textutil holds small text helpers shared by the parsers of files
// that come from the zapret release, the upstream repository and the user.
// All functions are pure and safe for arbitrary (untrusted) input.
package textutil

import "strings"

// BOM is the UTF-8 byte order mark as a string (written with byte escapes so
// the source file itself never contains the invisible character).
const BOM = "\xef\xbb\xbf"

// StripBOM removes a leading UTF-8 byte order mark.
func StripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// CleanLines splits text into trimmed lines (BOM and CR removed) and drops
// trailing empty lines. Empty lines in the middle are kept.
func CleanLines(data []byte) []string {
	text := strings.ReplaceAll(string(StripBOM(data)), "\r", "")
	var out []string
	for l := range strings.SplitSeq(text, "\n") {
		out = append(out, strings.TrimSpace(l))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// NormalizeText converts text to LF line endings without trailing spaces on
// lines and without trailing newlines. It is used to compare list files
// regardless of CRLF/LF and editor whitespace.
func NormalizeText(s string) string {
	s = strings.TrimPrefix(s, BOM)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}
