// Package version parses and orders zapret-discord-youtube release versions
// ("1.10.4", "1.10.5b", "1.10.4-20261108-120000").
package version

import (
	"regexp"
	"strings"
)

var (
	validRe      = regexp.MustCompile(`^[0-9]([0-9A-Za-z.\-]{0,29}[0-9A-Za-z])?$`)
	stampRe      = regexp.MustCompile(`-\d{8}-\d{6}$`)
	tokenRe      = regexp.MustCompile(`\d+|[A-Za-z]+`)
	localVersion = regexp.MustCompile(`(?im)^\s*set\s+"LOCAL_VERSION=([^"]+)"`)
)

// Valid reports whether v looks like a release version. Valid versions are
// also safe to use as a single path component: no separators, no drive
// letters, at most 31 characters, no ".." and no trailing dot or dash (Windows
// drops a trailing dot, so "1." and "1" would be the same folder).
func Valid(v string) bool { return validRe.MatchString(v) && !strings.Contains(v, "..") }

// FromServiceBat extracts LOCAL_VERSION from the content of service.bat.
func FromServiceBat(content []byte) (string, bool) {
	m := localVersion.FindSubmatch(content)
	if m == nil {
		return "", false
	}
	v := strings.TrimSpace(string(m[1]))
	return v, Valid(v)
}

// FromDirName guesses the version from a release folder name such as
// "zapret-discord-youtube-1.10.4". It returns the name itself when there is
// no version suffix. The "-YYYYMMDD-HHMMSS" suffix that a re-download adds
// ("1.10.4-20261109-120000") is not part of the version.
func FromDirName(base string) string {
	base = stampRe.ReplaceAllString(base, "")
	if Valid(base) {
		return base
	}
	if i := strings.LastIndex(base, "-"); i >= 0 && Valid(base[i+1:]) {
		return base[i+1:]
	}
	return base
}

// Compare orders versions: 1.10.4 < 1.10.5 < 1.10.5b < 1.11.0.
// Numeric tokens are compared as arbitrary-precision integers, letter tokens
// case-insensitively, and a number is newer than a letter suffix at the same
// position. It returns -1, 0 or 1 and is antisymmetric.
func Compare(a, b string) int {
	ta := tokenRe.FindAllString(a, -1)
	tb := tokenRe.FindAllString(b, -1)
	for i := 0; i < len(ta) && i < len(tb); i++ {
		na, nb := isDigits(ta[i]), isDigits(tb[i])
		switch {
		case na && nb:
			if c := compareNumeric(ta[i], tb[i]); c != 0 {
				return c
			}
		case na: // number is newer than a suffix
			return 1
		case nb:
			return -1
		default:
			if c := strings.Compare(strings.ToLower(ta[i]), strings.ToLower(tb[i])); c != 0 {
				return c
			}
		}
	}
	// 1.10 и 1.10.0 — одна версия: хвост из одних нулей не делает версию новее
	switch {
	case len(ta) < len(tb) && !allZeros(tb[len(ta):]):
		return -1
	case len(ta) > len(tb) && !allZeros(ta[len(tb):]):
		return 1
	}
	return 0
}

func allZeros(tokens []string) bool {
	for _, t := range tokens {
		if !isDigits(t) || strings.TrimLeft(t, "0") != "" {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// compareNumeric compares decimal digit strings of any length without
// overflow: "007" == "7", "100000000000000000000" > "9".
func compareNumeric(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
