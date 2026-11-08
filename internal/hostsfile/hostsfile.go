// Package hostsfile merges the Flowseal hosts block (.service/hosts) into the
// Windows hosts file between two marker lines and removes it again.
//
// Invariants (covered by tests and fuzzing):
//   - Merge is idempotent: merging the result again reports no change;
//   - lines that do not mention upstream names are preserved verbatim;
//   - an unterminated begin marker never causes lines to be dropped.
package hostsfile

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"zapret-manager/internal/textutil"
)

// Block markers. They are matched after trimming spaces, so editors that add
// indentation do not break detection.
const (
	Begin = "# >>> zapret-manager: hosts from Flowseal/zapret-discord-youtube (auto-updated) >>>"
	End   = "# <<< zapret-manager <<<"
)

// MinEntries is the minimum number of host entries an upstream file must
// contain; anything less is treated as an error page or a truncated download.
const MinEntries = 3

// ParseUpstream validates .service/hosts and returns the normalized lines of
// the block: comments kept, runs of blank lines collapsed, fields separated by
// a single space. Every entry must start with a valid IP address.
func ParseUpstream(data []byte) ([]string, error) {
	lines := textutil.CleanLines(data)
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	entries := 0
	var out []string
	prevBlank := false
	for _, l := range lines {
		if l == "" {
			if !prevBlank {
				out = append(out, "")
			}
			prevBlank = true
			continue
		}
		prevBlank = false
		if strings.HasPrefix(l, "#") {
			if isMarker(l) {
				return nil, fmt.Errorf("строка %q совпадает с маркером блока", l)
			}
			out = append(out, l)
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 || net.ParseIP(f[0]) == nil {
			return nil, fmt.Errorf("строка %q не похожа на запись hosts", l)
		}
		entries++
		out = append(out, strings.Join(f, " "))
	}
	if entries < MinEntries {
		return nil, errors.New("в hosts из репозитория почти нет записей")
	}
	return out, nil
}

func isMarker(line string) bool {
	t := strings.TrimSpace(line)
	return t == Begin || t == End
}

// names returns the lower-cased host names mentioned in block lines.
func names(lines []string) map[string]bool {
	set := map[string]bool{}
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		body, _, _ := strings.Cut(l, "#")
		f := strings.Fields(body)
		if len(f) < 2 {
			continue
		}
		for _, h := range f[1:] {
			set[strings.ToLower(h)] = true
		}
	}
	return set
}

// Merge removes the previous block and foreign entries for the same names
// (otherwise one name would resolve to two IPs), keeps everything else and
// appends the fresh block at the end. It returns the new content (CRLF) and
// whether it differs from local after normalization.
func Merge(local string, upstream []string) (string, bool) {
	upNames := names(upstream)
	text := strings.TrimPrefix(strings.ReplaceAll(local, "\r\n", "\n"), textutil.BOM)
	lines := strings.Split(text, "\n")
	begin, end := blockRange(lines)

	var kept []string
	for i, l := range lines {
		if begin >= 0 && end >= 0 && i >= begin && i <= end {
			continue
		}
		if isMarker(l) {
			continue // orphaned marker
		}
		kept = append(kept, filterLine(l, upNames)...)
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	var b strings.Builder
	for _, k := range kept {
		b.WriteString(k + "\r\n")
	}
	if len(kept) > 0 {
		b.WriteString("\r\n")
	}
	b.WriteString(Begin + "\r\n")
	for _, u := range upstream {
		b.WriteString(u + "\r\n")
	}
	b.WriteString(End + "\r\n")
	out := b.String()
	return out, textutil.NormalizeText(out) != textutil.NormalizeText(local)
}

// filterLine returns the line unchanged, without the names owned by the
// block, or nothing when every name on it is owned by the block.
func filterLine(l string, upNames map[string]bool) []string {
	l = strings.TrimRight(l, "\r")
	t := strings.TrimSpace(l)
	if t == "" || strings.HasPrefix(t, "#") {
		return []string{l}
	}
	body, comment, hasComment := strings.Cut(t, "#")
	f := strings.Fields(body)
	if len(f) < 2 {
		return []string{l}
	}
	var remain []string
	for _, h := range f[1:] {
		if !upNames[strings.ToLower(h)] {
			remain = append(remain, h)
		}
	}
	switch {
	case len(remain) == len(f)-1:
		return []string{l}
	case len(remain) == 0:
		return nil // the entry is entirely about names owned by the block
	default:
		nl := f[0] + " " + strings.Join(remain, " ")
		if hasComment {
			nl += " #" + comment
		}
		return []string{nl}
	}
}

// RemoveBlock removes only the managed block (for uninstall --all). Like
// Merge, it removes block contents only when both markers are present; an
// orphaned marker line is removed alone and the lines after it are kept.
func RemoveBlock(local string) (string, bool) {
	text := strings.TrimPrefix(strings.ReplaceAll(local, "\r\n", "\n"), textutil.BOM)
	lines := strings.Split(text, "\n")
	begin, end := blockRange(lines)
	var kept []string
	changed := false
	for i, l := range lines {
		if begin >= 0 && end >= 0 && i >= begin && i <= end {
			changed = true
			continue
		}
		if isMarker(l) {
			changed = true
			continue
		}
		kept = append(kept, strings.TrimRight(l, "\r"))
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	return strings.Join(kept, "\r\n") + "\r\n", changed
}

// blockRange returns the indexes of the first Begin marker and the first End
// marker after it, or -1 when absent.
func blockRange(lines []string) (begin, end int) {
	begin = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == Begin && begin < 0 {
			begin = i
		} else if t == End && begin >= 0 {
			return begin, i
		}
	}
	return begin, -1
}
