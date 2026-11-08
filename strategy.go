package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zapret-manager/internal/batch"
	"zapret-manager/internal/textutil"
	"zapret-manager/internal/zapretfmt"
)

// Strategy — один general*.bat из релиза (или свой .bat из папки custom).
type Strategy struct {
	Name   string // "general (ALT3)"
	Path   string
	Custom bool
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
	sort.SliceStable(out, func(i, j int) bool { return batch.NaturalKey(out[i].Name) < batch.NaturalKey(out[j].Name) })
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
func buildCommandLine(relDir string, s Strategy, gf zapretfmt.GameFilter) (string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	anyProto, tcp, udp := gf.Vars()
	vars := map[string]string{
		"bin":           filepath.Join(relDir, "bin") + `\`,
		"lists":         filepath.Join(relDir, "lists") + `\`,
		"gamefilter":    anyProto,
		"gamefiltertcp": tcp,
		"gamefilterudp": udp,
	}
	args, err := batch.ExtractWinwsArgs(string(textutil.StripBOM(data)), vars, filepath.Clean(relDir)+`\`)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(s.Path), err)
	}
	exe := filepath.Join(relDir, "bin", "winws.exe")
	return `"` + exe + `" ` + args, nil
}

// ---------- файлы настроек zapret в папке релиза (их же читает service.bat) ----------

func readGameFilter(relDir string) zapretfmt.GameFilter {
	data, err := os.ReadFile(filepath.Join(relDir, "utils", "game_filter.enabled"))
	if err != nil {
		return zapretfmt.DisabledGameFilter()
	}
	return zapretfmt.ParseGameFilter(string(textutil.StripBOM(data)))
}

// ipsetMode как в service.bat: пустой файл — any, заглушка 203.0.113.113/32 — none,
// иначе loaded. Нет файла — none (winws с ним не стартует, ensureUserLists создаст заглушку).
func ipsetMode(relDir string) string {
	data, err := os.ReadFile(filepath.Join(relDir, "lists", "ipset-all.txt"))
	if err != nil {
		return zapretfmt.IpsetNone
	}
	return zapretfmt.IpsetModeOf(data)
}

func countIpsetLines(relDir string) int {
	data, err := os.ReadFile(filepath.Join(relDir, "lists", "ipset-all.txt"))
	if err != nil {
		return 0
	}
	return zapretfmt.CountEntries(data)
}

// ensureUserLists создаёт *-user.txt, как load_user_lists в service.bat. Без них winws
// не стартует, а из службы service.bat никто не вызывает.
func ensureUserLists(relDir string) error {
	lists := filepath.Join(relDir, "lists")
	defaults := map[string]string{
		"ipset-exclude-user.txt": zapretfmt.IpsetNoneMarker + "\r\n",
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
		return os.WriteFile(p, []byte(zapretfmt.IpsetNoneMarker+"\r\n"), 0o644)
	}
	return nil
}
