package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	return &App{Root: root, Cfg: defaultConfig(), Log: &Logger{path: filepath.Join(root, "test.log")}}
}

func TestConfigNormalizeDefaults(t *testing.T) {
	c := Config{CheckIntervalSec: 5, HealthyRatio: 2, KeepVersions: -1}
	if warns := c.normalize(); len(warns) != 0 {
		t.Fatalf("пустые repo/branch — не ошибка: %v", warns)
	}
	d := defaultConfig()
	if c.CheckIntervalSec != d.CheckIntervalSec || c.HealthyRatio != d.HealthyRatio || c.KeepVersions != d.KeepVersions ||
		c.Repo != d.Repo || c.Branch != d.Branch || c.UpdateIntervalHours != d.UpdateIntervalHours {
		t.Fatalf("%+v", c)
	}
}

// repo и branch попадают в URL — подмена на путь или другой хост не проходит.
func TestConfigNormalizeRejectsUnsafeRepo(t *testing.T) {
	bad := []Config{
		{Repo: "evil.com/x/y"},
		{Repo: "../../x"},
		{Repo: "a/b?c=d"},
		{Repo: "a/b", Branch: "../../../other/repo/main"},
		{Repo: "a/b", Branch: "main?x"},
		{Repo: "a/b", Branch: "-main"},
	}
	for _, c := range bad {
		orig := c
		warns := c.normalize()
		if len(warns) == 0 {
			t.Errorf("%+v: нет предупреждения", orig)
		}
		if !repoRe.MatchString(c.Repo) || !branchRe.MatchString(c.Branch) || strings.Contains(c.Branch+c.Repo, "..") {
			t.Errorf("%+v → %+v", orig, c)
		}
	}
	ok := Config{Repo: "my-fork/zapret-discord-youtube", Branch: "feature/x.y"}
	if warns := ok.normalize(); len(warns) != 0 || ok.Repo != "my-fork/zapret-discord-youtube" || ok.Branch != "feature/x.y" {
		t.Fatalf("%v %+v", warns, ok)
	}
}

func TestConfigNormalizeTargets(t *testing.T) {
	c := Config{Targets: []Target{
		{Name: "", URL: " https://discord.com/x ", Group: " Discord "},
		{Name: "ftp", URL: "ftp://x/", Group: "a"},
		{Name: "nohost", URL: "https:///", Group: "a"},
		{Name: "g", URL: "http://example.com/", Group: ""},
	}}
	warns := c.normalize()
	if len(warns) != 2 || len(c.Targets) != 2 {
		t.Fatalf("warns=%v targets=%+v", warns, c.Targets)
	}
	if c.Targets[0].Name != "discord.com" || c.Targets[0].Group != "discord" || c.Targets[1].Group != "other" {
		t.Fatalf("%+v", c.Targets)
	}
	many := Config{}
	for i := range maxTargets + 5 {
		many.Targets = append(many.Targets, Target{URL: fmt.Sprintf("https://h%d.example/", i)})
	}
	if many.normalize(); len(many.Targets) != maxTargets {
		t.Fatalf("адресов %d", len(many.Targets))
	}
}

func TestReloadConfigWarnsOnce(t *testing.T) {
	a := testApp(t)
	if err := os.WriteFile(a.path("config.json"), []byte(`{"repo": "bad repo"`), 0o644); err != nil {
		t.Fatal(err)
	}
	a.reloadConfig()
	a.reloadConfig()
	data, _ := os.ReadFile(a.Log.path)
	if n := strings.Count(string(data), "config.json не читается"); n != 1 {
		t.Fatalf("предупреждение записано %d раз:\n%s", n, data)
	}
	if a.Cfg.Repo != defaultConfig().Repo {
		t.Fatal(a.Cfg.Repo)
	}
}

func TestUpdateStateConcurrent(t *testing.T) {
	a := testApp(t)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { a.updateState(func(s *State) { s.HealStreak++ }) })
	}
	wg.Wait()
	if got := a.loadState().HealStreak; got != 50 {
		t.Fatalf("потеряны обновления: %d из 50", got)
	}
	if left, _ := filepath.Glob(a.path("state.json.*.tmp")); len(left) != 0 {
		t.Fatalf("остались временные файлы: %v", left)
	}
}

// Испорченный state.json не затирается молча — его копия остаётся для разбора.
func TestUpdateStateKeepsCorruptCopy(t *testing.T) {
	a := testApp(t)
	if err := os.WriteFile(a.path("state.json"), []byte(`{"bad_versions": ["1.0"`), 0o644); err != nil {
		t.Fatal(err)
	}
	a.updateState(func(s *State) { s.Strategy = "ALT" })
	if st, err := a.readState(); err != nil || st.Strategy != "ALT" {
		t.Fatalf("%+v %v", st, err)
	}
	corrupt, _ := filepath.Glob(a.path("state.json.corrupt-*"))
	if len(corrupt) != 1 {
		t.Fatalf("нет копии испорченного файла: %v", corrupt)
	}
	if data, _ := os.ReadFile(corrupt[0]); !strings.Contains(string(data), "bad_versions") {
		t.Fatalf("%q", data)
	}
}

func TestStateRoundTrip(t *testing.T) {
	a := testApp(t)
	now := time.Now().Truncate(time.Second)
	a.updateState(func(s *State) {
		s.PausedUntil = time.Date(9999, 1, 1, 0, 0, 0, 0, time.Local)
		s.LastSweep = &SweepSummary{Time: now, Ranking: []RankEntry{{Strategy: "A", Score: 3}}}
		s.BadVersions = []string{"1.0"}
	})
	st := a.loadState()
	if st.PausedUntil.Year() != 9999 || st.LastSweep == nil || st.LastSweep.Ranking[0].Strategy != "A" || st.BadVersions[0] != "1.0" {
		t.Fatalf("%+v", st)
	}
	var raw map[string]any
	data, _ := os.ReadFile(a.path("state.json"))
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
}

func TestFileLockExcludesOtherHolders(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".lock")
	release, err := acquireFileLock(p, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireFileLock(p, 0, nil); !errors.Is(err, errBusy) {
		t.Fatalf("вторая блокировка должна быть занята: %v", err)
	}
	waits := 0
	time.AfterFunc(300*time.Millisecond, release)
	r2, err := acquireFileLock(p, 5*time.Second, func() { waits++ })
	if err != nil {
		t.Fatal(err)
	}
	r2()
	if waits != 1 {
		t.Fatalf("onWait вызван %d раз", waits)
	}
}

func TestWithLockBusy(t *testing.T) {
	a := testApp(t)
	err := a.withLock(time.Second, func() error {
		return a.withLock(0, func() error { return nil })
	})
	if !errors.Is(err, errBusy) {
		t.Fatalf("вложенная операция должна получить errBusy: %v", err)
	}
}

func TestParseEventLine(t *testing.T) {
	ts, msg, ok := parseEventLine("2026-11-08 12:30:00 [*] стратегия сменена")
	if !ok || msg != "стратегия сменена" || ts.Hour() != 12 {
		t.Fatalf("%v %q %v", ts, msg, ok)
	}
	for _, l := range []string{
		"2026-11-08 12:30:00 [.] ответ сервера: x [*] y", // [*] внутри текста — не событие
		"2026-11-08 12:30:00 [ ] инфо",
		"garbage [*] x",
		"",
		"2026-13-45 99:99:99 [*] битое время",
	} {
		if _, _, ok := parseEventLine(l); ok {
			t.Errorf("%q не событие", l)
		}
	}
}

func TestLoggerRotates(t *testing.T) {
	dir := t.TempDir()
	l := &Logger{path: filepath.Join(dir, "m.log")}
	if err := os.WriteFile(l.path, make([]byte, maxLogBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	l.Eventf("строка 1\nстрока 2")
	data, _ := os.ReadFile(l.path)
	if !strings.Contains(string(data), "[*] строка 1\r\n") || !strings.Contains(string(data), "[*] строка 2\r\n") {
		t.Fatalf("%q", data)
	}
	if fi, err := os.Stat(l.path + ".1"); err != nil || fi.Size() <= maxLogBytes {
		t.Fatal("старый лог не перенесён в .1")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "f.txt")
	for _, s := range []string{"one", "two"} {
		if err := writeFileAtomic(p, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(p); string(data) != "two" {
		t.Fatalf("%q", data)
	}
	if left, _ := filepath.Glob(p + ".*.tmp"); len(left) != 0 {
		t.Fatalf("остались временные файлы: %v", left)
	}
}

func TestPathHelpers(t *testing.T) {
	if !samePath(`C:\Zapret\`, `c:\zapret`) || samePath(`C:\zapret`, `C:\zapret2`) {
		t.Fatal("samePath")
	}
	cases := []struct {
		p, dir string
		want   bool
	}{
		{`C:\zapret\releases\1.0`, `C:\zapret`, true},
		{`C:\zapret`, `C:\ZAPRET`, true},
		{`C:\zapret2`, `C:\zapret`, false},
		{`C:\`, `C:\zapret`, false},
	}
	for _, c := range cases {
		if got := isWithin(c.p, c.dir); got != c.want {
			t.Errorf("isWithin(%s, %s) = %v", c.p, c.dir, got)
		}
	}
}

// Временная ошибка чтения (здесь — state.json оказался папкой) не порча: файл не
// откладывается в .corrupt и не затирается пустым состоянием.
func TestUpdateStateKeepsUnreadableState(t *testing.T) {
	a := testApp(t)
	p := a.path("state.json")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	a.updateState(func(st *State) { st.Strategy = "x" })
	if !dirExists(p) {
		t.Fatal("state.json переименован или заменён, хотя он не испорчен")
	}
	if m, _ := filepath.Glob(a.path("state.json.corrupt-*")); len(m) != 0 {
		t.Fatalf("лишняя копия: %v", m)
	}
}

func TestReadStateCorruptIsTyped(t *testing.T) {
	a := testApp(t)
	if err := os.WriteFile(a.path("state.json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readState(); !errors.Is(err, errStateCorrupt) {
		t.Fatalf("ждал errStateCorrupt, получил %v", err)
	}
}

func TestWriteFileReplaceNeverOverwritesInPlace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// держим файл открытым без FILE_SHARE_DELETE — замена невозможна
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	err = writeFileReplace(p, []byte("new"))
	_ = f.Close()
	if err == nil {
		t.Skip("ОС позволила заменить открытый файл — проверять нечего")
	}
	if data, _ := os.ReadFile(p); string(data) != "old" {
		t.Fatalf("файл переписан на месте: %q", data)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Fatalf("остался временный файл: %v", m)
	}
}

func TestWithLockCtxCancelStopsWaiting(t *testing.T) {
	a := testApp(t)
	release, err := acquireFileLock(a.path(".lock"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = a.withLockCtx(ctx, time.Minute, func() error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ждал отмену, получил %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("ожидание не прервалось отменой")
	}
}
