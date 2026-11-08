package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"zapret-manager/internal/batch"
	"zapret-manager/internal/zapretfmt"
)

// realReleaseDir — распакованный релиз Flowseal для интеграционных тестов:
// ZAPRET_TEST_DIR или ~/Desktop/zapret-discord-youtube-1.10.4.
func realReleaseDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("ZAPRET_TEST_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Desktop", "zapret-discord-youtube-1.10.4")
	}
	if !validRelease(dir) {
		t.Skip("нет распакованного релиза для проверки (ZAPRET_TEST_DIR):", dir)
	}
	return dir
}

// Все настоящие стратегии из релиза должны разбираться без ошибок и без остатков %VAR%.
func TestRealStrategies(t *testing.T) {
	dir := realReleaseDir(t)
	list, err := listStrategies(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 10 {
		t.Fatalf("стратегий мало: %d", len(list))
	}
	gfs := []zapretfmt.GameFilter{
		zapretfmt.DisabledGameFilter(),
		{Mode: zapretfmt.ModeAll, TCPRange: "1024-65535", UDPRange: "1024-65535"},
	}
	for _, gf := range gfs {
		for _, s := range list {
			cmd, err := buildCommandLine(dir, s, gf)
			if err != nil {
				t.Errorf("%s: %v", s.Name, err)
				continue
			}
			if strings.Contains(cmd, "%") || strings.Contains(cmd, "^") {
				t.Errorf("%s: остались %% или ^: %s", s.Name, cmd)
			}
			if !strings.HasPrefix(cmd, `"`+filepath.Join(dir, "bin", "winws.exe")+`" --wf-`) {
				t.Errorf("%s: странное начало: %.120s", s.Name, cmd)
			}
			if strings.Count(cmd, `"`)%2 != 0 {
				t.Errorf("%s: непарные кавычки", s.Name)
			}
		}
	}
}

func TestIpsetMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lists"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ipsetMode(dir); got != zapretfmt.IpsetNone {
		t.Errorf("без файла: %s, want none", got)
	}
	p := filepath.Join(dir, "lists", "ipset-all.txt")
	check := func(content, want string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ipsetMode(dir); got != want {
			t.Errorf("%q: %s, want %s", content, got, want)
		}
	}
	check("", zapretfmt.IpsetAny)
	check("203.0.113.113/32\r\n", zapretfmt.IpsetNone)
	check("1.0.0.0/24\r\n1.1.1.0/24\r\n", zapretfmt.IpsetLoaded)
	if n := countIpsetLines(dir); n != 2 {
		t.Errorf("countIpsetLines = %d", n)
	}
}

func TestEnsureUserListsKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	lists := filepath.Join(dir, "lists")
	if err := os.MkdirAll(lists, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(lists, "list-general-user.txt")
	if err := os.WriteFile(mine, []byte("my.domain\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureUserLists(dir); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(mine); string(data) != "my.domain\r\n" {
		t.Fatalf("свой список перезаписан: %q", data)
	}
	for _, f := range []string{"list-exclude-user.txt", "ipset-exclude-user.txt", "ipset-all.txt"} {
		if !fileExists(filepath.Join(lists, f)) {
			t.Errorf("не создан %s", f)
		}
	}
}

func writeStrategy(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindStrategyAndCustom(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join(root, "releases", "1.0")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &App{Root: root, Cfg: defaultConfig(), Log: &Logger{}}
	if err := os.MkdirAll(a.customDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"general.bat", "general (ALT).bat", "general (ALT2).bat", "general (ALT10).bat", "service.bat"} {
		writeStrategy(t, rel, n, "x")
	}
	writeStrategy(t, a.customDir(), "my strategy.bat", "x")
	writeStrategy(t, a.customDir(), "general (ALT2).bat", "dup") // дубль релиза не добавляется

	all, err := a.listAllStrategies(rel)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range all {
		names = append(names, s.Name)
	}
	want := "general|general (ALT)|general (ALT2)|general (ALT10)|my strategy"
	if strings.Join(names, "|") != want {
		t.Fatalf("порядок/состав: %v", names)
	}

	cases := map[string]string{
		"general (ALT2).bat": "general (ALT2)",
		"ALT10":              "general (ALT10)",
		"alt":                "general (ALT)", // точное «general (alt)» важнее подстроки
		"general (alt":       "",              // подстрока подходит к трём — неоднозначно
		"my":                 "my strategy",
		"nope":               "",
	}
	for in, want := range cases {
		s, err := a.findStrategy(rel, in)
		if (err != nil) != (want == "") || (err == nil && s.Name != want) {
			t.Errorf("findStrategy(%q) = %q, %v; want %q", in, s.Name, err, want)
		}
	}

	a.Cfg.ExcludeStrategies = []string{"general (ALT).bat", " MY STRATEGY "}
	all, _ = a.listAllStrategies(rel)
	if len(all) != 3 {
		t.Fatalf("exclude_strategies не сработал: %v", all)
	}
}

// Сверка с тем, что прописал в службу service.bat: аргументы должны совпасть один в один
// (service.bat пишет «--opt значение», мы — «--opt=значение», как в самом .bat).
func TestMatchesServiceBatInstall(t *testing.T) {
	info := queryService(zapretSvcName)
	strategy := readRegistryStrategy()
	if !info.Exists || strategy == "" {
		t.Skip("служба zapret не установлена")
	}
	exe := batch.ExeFromCmdline(info.BinPath)
	relDir := filepath.Dir(filepath.Dir(exe))
	if !validRelease(relDir) {
		t.Skip("папка службы не похожа на релиз")
	}
	s, err := (&App{Cfg: defaultConfig(), Root: t.TempDir()}).findStrategy(relDir, strategy)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := buildCommandLine(relDir, s, readGameFilter(relDir))
	if err != nil {
		t.Fatal(err)
	}
	norm := func(cmd string) []string {
		parts, err := windows.DecomposeCommandLine(cmd)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range parts[1:] {
			if strings.HasPrefix(p, "--") {
				if k, v, ok := strings.Cut(p, "="); ok {
					out = append(out, k, v)
					continue
				}
			}
			out = append(out, p)
		}
		return out
	}
	a, b := norm(ours), norm(info.BinPath)
	if strings.Join(a, "\x00") != strings.Join(b, "\x00") {
		for i := 0; i < len(a) && i < len(b); i++ {
			if a[i] != b[i] {
				t.Fatalf("расхождение на аргументе %d: у нас %q, в службе %q", i, a[i], b[i])
			}
		}
		t.Fatalf("разное число аргументов: %d против %d", len(a), len(b))
	}
	t.Logf("%s: %d аргументов совпали", strategy, len(a))
}
