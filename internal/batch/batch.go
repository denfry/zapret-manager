// Package batch turns a zapret strategy file (general*.bat) into the winws.exe
// argument string exactly as cmd.exe would execute it: %VAR% expansion,
// ^-continuations joined, ^x unescaped outside quotes.
//
// The output goes into the binPath of the zapret service (LocalSystem) and
// into CreateProcess during strategy tests, so the parser never guesses:
// unknown variables are an error, not an empty string.
package batch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	varNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	digitRunRe = regexp.MustCompile(`\d+`)
)

// Errors returned by ExtractWinwsArgs.
var (
	ErrNoWinws = errors.New("нет строки запуска winws.exe")
	ErrNoArgs  = errors.New("у winws.exe нет аргументов")
)

// ExtractWinwsArgs finds the first non-comment logical line that starts
// winws.exe and returns its expanded arguments. vars are matched
// case-insensitively and must be given with lower-case keys; dp0 is the value
// of %~dp0 (release folder with a trailing backslash).
//
// The launch line is the first one that mentions "winws.exe" and has arguments:
// lines like `taskkill /IM "winws.exe"` are skipped. Redirections and command
// separators after the arguments (>nul 2>&1, & …, | …) are not winws arguments
// and are cut off, as cmd.exe would.
func ExtractWinwsArgs(content string, vars map[string]string, dp0 string) (string, error) {
	sawWinws := false
	for _, line := range logicalLines(content) {
		low := asciiLower(line)
		if isComment(low) {
			continue
		}
		i := strings.Index(low, `winws.exe"`)
		if i < 0 {
			continue
		}
		sawWinws = true
		rest := line[i+len(`winws.exe"`):]
		expanded, err := ExpandVars(rest, vars, dp0)
		if err != nil {
			return "", err
		}
		args := strings.TrimSpace(UnescapeCarets(cutOperators(expanded)))
		if !strings.Contains(args, "--") {
			continue
		}
		return args, nil
	}
	if sawWinws {
		return "", ErrNoArgs
	}
	return "", ErrNoWinws
}

// isComment: строка — комментарий cmd.exe (::, rem, @rem; после rem — пробел,
// табуляция, конец строки или разделитель вроде «rem.»). low — в нижнем регистре.
func isComment(low string) bool {
	t := strings.TrimLeft(low, " \t@")
	if strings.HasPrefix(t, "::") {
		return true
	}
	rest, ok := strings.CutPrefix(t, "rem")
	return ok && (rest == "" || strings.ContainsRune(" \t.:/\\(,;=", rune(rest[0])))
}

// cutOperators обрезает строку на первом операторе cmd.exe вне кавычек и не
// экранированном ^: &, |, <, >. Номер потока перед перенаправлением (2>) уходит тоже.
func cutOperators(s string) string {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '^':
			i++ // следующий символ экранирован
		case c == '&' || c == '|':
			return s[:i]
		case c == '<' || c == '>':
			if i > 0 && s[i-1] >= '0' && s[i-1] <= '9' && (i == 1 || s[i-2] == ' ' || s[i-2] == '\t') {
				return s[:i-1]
			}
			return s[:i]
		}
	}
	return s
}

// logicalLines joins physical lines ending with an odd number of carets
// (a trailing ^ is a continuation, ^^ is a literal caret).
func logicalLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	var logical []string
	var cur strings.Builder
	for ln := range strings.SplitSeq(content, "\n") {
		ln = strings.TrimRight(ln, "\r")
		carets := len(ln) - len(strings.TrimRight(ln, "^"))
		if carets%2 == 1 {
			cur.WriteString(ln[:len(ln)-1])
			continue
		}
		cur.WriteString(ln)
		logical = append(logical, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		logical = append(logical, cur.String())
	}
	return logical
}

// ExpandVars is the first cmd.exe parsing phase: %VAR%, %% and %~dp0.
// A % that does not start a valid variable name is kept literally.
func ExpandVars(s string, vars map[string]string, dp0 string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '%' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '%' {
			b.WriteByte('%')
			i += 2
			continue
		}
		if len(s)-i >= 5 && strings.EqualFold(s[i:i+5], "%~dp0") {
			b.WriteString(dp0)
			i += 5
			continue
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			b.WriteByte('%')
			i++
			continue
		}
		name := s[i+1 : i+1+j]
		if !varNameRe.MatchString(name) {
			b.WriteByte('%')
			i++
			continue
		}
		v, ok := vars[strings.ToLower(name)]
		if !ok {
			return "", fmt.Errorf("неизвестная переменная %%%s%%", name)
		}
		b.WriteString(v)
		i += j + 2
	}
	return b.String(), nil
}

// UnescapeCarets is the second cmd.exe phase: ^x → x outside double quotes
// (so --dpi-desync-fake-tls=^! becomes =!). A lone trailing ^ is dropped.
func UnescapeCarets(s string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '^' && !inQuote:
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ExeFromCmdline returns the executable path of a service command line:
// the quoted first token, or everything up to ".exe", or the first field.
func ExeFromCmdline(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if rest, ok := strings.CutPrefix(cmd, `"`); ok {
		if exe, _, closed := strings.Cut(rest, `"`); closed {
			return exe
		}
		return strings.Trim(cmd, `"`)
	}
	// «.exe» — конец пути, только если за ним пробел или конец строки
	// (C:\a.exe-tools\winws.exe — один путь)
	low := asciiLower(cmd)
	for off := 0; ; {
		i := strings.Index(low[off:], ".exe")
		if i < 0 {
			break
		}
		end := off + i + 4
		if end == len(cmd) || cmd[end] == ' ' || cmd[end] == '\t' {
			return cmd[:end]
		}
		off = end
	}
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return ""
}

// asciiLower lower-cases only ASCII letters. Unlike strings.ToLower it keeps
// the byte length of any input (strings.ToLower replaces invalid UTF-8 with
// the 3-byte U+FFFD), so an index found in the result is valid in s. .bat
// files are often saved in CP1251/CP866, which is invalid UTF-8.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// NaturalKey reproduces the strategy order of service.bat: digit runs are
// left-padded with zeros to 8 characters, comparison is case-insensitive.
func NaturalKey(s string) string {
	return strings.ToLower(digitRunRe.ReplaceAllStringFunc(s, func(d string) string {
		if len(d) >= 8 {
			return d
		}
		return strings.Repeat("0", 8-len(d)) + d
	}))
}
