package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Strategy — один general*.bat из релиза (или свой .bat из папки custom).
type Strategy struct {
	Name   string // "general (ALT3)"
	Path   string
	Custom bool
}

var digitRunRe = regexp.MustCompile(`\d+`)

// naturalKey повторяет сортировку service.bat: числа дополняются нулями до 8 знаков.
func naturalKey(s string) string {
	return strings.ToLower(digitRunRe.ReplaceAllStringFunc(s, func(d string) string {
		if len(d) >= 8 {
			return d
		}
		return strings.Repeat("0", 8-len(d)) + d
	}))
}

func listStrategies(dir string, custom bool) ([]Strategy, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Strategy
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".bat") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "service") {
			continue
		}
		out = append(out, Strategy{Name: strings.TrimSuffix(name, filepath.Ext(name)), Path: filepath.Join(dir, name), Custom: custom})
	}
	sort.SliceStable(out, func(i, j int) bool { return naturalKey(out[i].Name) < naturalKey(out[j].Name) })
	return out, nil
}

// listAllStrategies — стратегии релиза плюс свои из custom\, без исключённых в конфиге.
func (a *App) listAllStrategies(relDir string) ([]Strategy, error) {
	rel, err := listStrategies(relDir, false)
	if err != nil {
		return nil, fmt.Errorf("не читается папка релиза %s: %w", relDir, err)
	}
	seen := map[string]bool{}
	for _, s := range rel {
		seen[strings.ToLower(s.Name)] = true
	}
	if custom, err := listStrategies(a.customDir(), true); err == nil {
		for _, s := range custom {
			if !seen[strings.ToLower(s.Name)] {
				rel = append(rel, s)
			}
		}
	}
	excluded := map[string]bool{}
	for _, x := range a.Cfg.ExcludeStrategies {
		excluded[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(x), ".bat"))] = true
	}
	out := rel[:0]
	for _, s := range rel {
		if !excluded[strings.ToLower(s.Name)] {
			out = append(out, s)
		}
	}
	return out, nil
}

// findStrategy понимает "general (ALT3)", "general (ALT3).bat", "ALT3" и однозначную подстроку.
func (a *App) findStrategy(relDir, name string) (Strategy, error) {
	all, err := a.listAllStrategies(relDir)
	if err != nil {
		return Strategy{}, err
	}
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".bat"))
	if n == "" {
		return Strategy{}, errors.New("не указано имя стратегии")
	}
	for _, s := range all {
		if strings.ToLower(s.Name) == n {
			return s, nil
		}
	}
	for _, s := range all {
		if strings.ToLower(s.Name) == "general ("+n+")" {
			return s, nil
		}
	}
	var hits []string
	var hit Strategy
	for _, s := range all {
		if strings.Contains(strings.ToLower(s.Name), n) {
			hits = append(hits, s.Name)
			hit = s
		}
	}
	switch len(hits) {
	case 1:
		return hit, nil
	case 0:
		return Strategy{}, fmt.Errorf("стратегия %q не найдена в %s", name, relDir)
	default:
		return Strategy{}, fmt.Errorf("под %q подходит несколько стратегий: %s", name, strings.Join(hits, ", "))
	}
}

// buildCommandLine превращает general*.bat в командную строку winws.exe — ровно ту,
// что запустил бы сам .bat (переменные подставлены, ^ обработаны, кавычки как есть).
// Эта же строка идёт в binPath службы и в CreateProcess при тестах.
func buildCommandLine(relDir string, s Strategy, gf GameFilter) (string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	any, tcp, udp := gf.Vars()
	vars := map[string]string{
		"bin":           filepath.Join(relDir, "bin") + `\`,
		"lists":         filepath.Join(relDir, "lists") + `\`,
		"gamefilter":    any,
		"gamefiltertcp": tcp,
		"gamefilterudp": udp,
	}
	args, err := extractWinwsArgs(string(stripBOM(data)), vars, filepath.Clean(relDir)+`\`)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(s.Path), err)
	}
	exe := filepath.Join(relDir, "bin", "winws.exe")
	return `"` + exe + `" ` + args, nil
}

func extractWinwsArgs(content string, vars map[string]string, dp0 string) (string, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	var logical []string
	var cur strings.Builder
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimRight(ln, "\r")
		// нечётное число ^ в конце — перенос строки (^^ — это буквальный ^)
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
	for _, line := range logical {
		low := strings.ToLower(line)
		trim := strings.TrimSpace(low)
		if strings.HasPrefix(trim, "::") || strings.HasPrefix(trim, "rem ") || strings.HasPrefix(trim, "@rem ") {
			continue
		}
		i := strings.Index(low, `winws.exe"`)
		if i < 0 {
			continue
		}
		rest := line[i+len(`winws.exe"`):]
		expanded, err := expandBatchVars(rest, vars, dp0)
		if err != nil {
			return "", err
		}
		args := strings.TrimSpace(unescapeCarets(expanded))
		if !strings.Contains(args, "--") {
			return "", errors.New("у winws.exe нет аргументов")
		}
		return args, nil
	}
	return "", errors.New("нет строки запуска winws.exe")
}

var batchVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// expandBatchVars — первая фаза разбора cmd: %VAR%, %%, %~dp0.
func expandBatchVars(s string, vars map[string]string, dp0 string) (string, error) {
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
		if strings.HasPrefix(strings.ToLower(s[i:]), "%~dp0") {
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
		if !batchVarNameRe.MatchString(name) {
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

// unescapeCarets — вторая фаза cmd: ^x → x вне кавычек (так --dpi-desync-fake-tls=^! становится =!).
func unescapeCarets(s string) string {
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

// exeFromCmdline достаёт путь к exe из командной строки службы.
func exeFromCmdline(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if j := strings.Index(cmd[1:], `"`); j >= 0 {
			return cmd[1 : 1+j]
		}
		return strings.Trim(cmd, `"`)
	}
	low := strings.ToLower(cmd)
	if i := strings.Index(low, ".exe"); i >= 0 {
		return cmd[:i+4]
	}
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return ""
}

// ---------- Game filter (utils\game_filter.enabled, формат service.bat) ----------

type GameFilter struct {
	Mode     string // disabled | all | tcp | udp
	TCPRange string
	UDPRange string
}

func readGameFilter(relDir string) GameFilter {
	gf := GameFilter{Mode: "disabled", TCPRange: "1024-65535", UDPRange: "1024-65535"}
	data, err := os.ReadFile(filepath.Join(relDir, "utils", "game_filter.enabled"))
	if err != nil {
		return gf
	}
	return parseGameFilter(string(stripBOM(data)))
}

func parseGameFilter(content string) GameFilter {
	gf := GameFilter{Mode: "disabled", TCPRange: "1024-65535", UDPRange: "1024-65535"}
	var tcpCand, udpCand string
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "mode":
			gf.Mode = strings.ToLower(v)
		case "all":
			gf.Mode = "all"
		case "udp":
			if v == "" {
				gf.Mode = "udp"
			} else {
				udpCand = v
			}
		case "tcp":
			if v == "" {
				gf.Mode = "tcp"
			} else {
				tcpCand = v
			}
		}
	}
	if r, ok := validatePortRanges(tcpCand); ok {
		gf.TCPRange = r
	}
	if r, ok := validatePortRanges(udpCand); ok {
		gf.UDPRange = r
	}
	switch gf.Mode {
	case "all", "tcp", "udp":
	default:
		gf.Mode = "disabled"
	}
	return gf
}

// Vars — значения %GameFilter%, %GameFilterTCP%, %GameFilterUDP% (12 — «пустой» порт, как в service.bat).
func (g GameFilter) Vars() (any, tcp, udp string) {
	switch g.Mode {
	case "all":
		return g.TCPRange, g.TCPRange, g.UDPRange
	case "tcp":
		return g.TCPRange, g.TCPRange, "12"
	case "udp":
		return g.UDPRange, "12", g.UDPRange
	}
	return "12", "12", "12"
}

func (g GameFilter) String() string {
	switch g.Mode {
	case "all":
		return fmt.Sprintf("TCP %s и UDP %s", g.TCPRange, g.UDPRange)
	case "tcp":
		return "TCP " + g.TCPRange
	case "udp":
		return "UDP " + g.UDPRange
	}
	return "выключен"
}

var portItemRe = regexp.MustCompile(`^[1-9][0-9]*(-[1-9][0-9]*)?$`)

func validatePortRanges(s string) (string, bool) {
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return "", false
	}
	for _, item := range strings.Split(s, ",") {
		if !portItemRe.MatchString(item) {
			return "", false
		}
		lo, hi, found := strings.Cut(item, "-")
		if !found {
			hi = lo
		}
		if len(lo) > 5 || len(hi) > 5 {
			return "", false
		}
		a, _ := strconv.Atoi(lo)
		b, _ := strconv.Atoi(hi)
		if a > 65535 || b > 65535 || a > b {
			return "", false
		}
	}
	return s, true
}

// ---------- ipset и пользовательские списки ----------

const ipsetNoneMarker = "203.0.113.113/32"

// ipsetMode как в service.bat: пустой файл — any, заглушка 203.0.113.113/32 — none, иначе loaded.
func ipsetMode(relDir string) string {
	data, err := os.ReadFile(filepath.Join(relDir, "lists", "ipset-all.txt"))
	if err != nil {
		return "none"
	}
	text := strings.TrimSpace(string(stripBOM(data)))
	if text == "" {
		return "any"
	}
	if strings.Contains(text, ipsetNoneMarker) {
		return "none"
	}
	return "loaded"
}

func countIpsetLines(relDir string) int {
	data, err := os.ReadFile(filepath.Join(relDir, "lists", "ipset-all.txt"))
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			n++
		}
	}
	return n
}

// ensureUserLists создаёт *-user.txt, как load_user_lists в service.bat. Без них winws
// не стартует, а из службы service.bat никто не вызывает.
func ensureUserLists(relDir string) error {
	lists := filepath.Join(relDir, "lists")
	defaults := map[string]string{
		"ipset-exclude-user.txt": ipsetNoneMarker + "\r\n",
		"list-general-user.txt":  "# Never leave this file empty\r\ndomain.example.abc\r\n",
		"list-exclude-user.txt":  "domain.example.abc\r\n",
	}
	for name, content := range defaults {
		p := filepath.Join(lists, name)
		if !fileExists(p) {
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	p := filepath.Join(lists, "ipset-all.txt")
	if !fileExists(p) {
		return os.WriteFile(p, []byte(ipsetNoneMarker+"\r\n"), 0o644)
	}
	return nil
}
