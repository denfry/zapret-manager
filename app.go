package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// Config — настройки менеджера (config.json в корневой папке). Настройки самого
// zapret (game filter, режим ipset, *-user.txt) живут в папке релиза в тех же
// файлах, что читает service.bat, чтобы его меню продолжало работать.
type Config struct {
	AutoHeal            bool     `json:"auto_heal"`              // сам перезапускать / подбирать стратегию, когда Discord или YouTube отвалились
	AutoUpdate          bool     `json:"auto_update"`            // сам ставить новые релизы Flowseal
	UpdateIpset         bool     `json:"update_ipset"`           // обновлять ipset-all.txt из .service/ipset-service.txt
	UpdateListsFromMain bool     `json:"update_lists_from_main"` // подтягивать list-*.txt из ветки main между релизами
	UpdateHosts         bool     `json:"update_hosts"`           // держать в hosts актуальный блок из .service/hosts
	CheckIntervalSec    int      `json:"check_interval_sec"`     // как часто проверять Discord и YouTube
	UpdateIntervalHours int      `json:"update_interval_hours"`  // как часто проверять обновления
	MinSweepIntervalMin int      `json:"min_sweep_interval_min"` // не чаще одного полного подбора за это время
	FailuresBeforeHeal  int      `json:"failures_before_heal"`   // сколько проверок подряд должно провалиться
	HealthyRatio        float64  `json:"healthy_ratio"`          // доля открывшихся адресов в группе, при которой всё в порядке
	KeepVersions        int      `json:"keep_versions"`          // сколько релизов хранить в releases\
	Repo                string   `json:"repo"`
	Branch              string   `json:"branch"`
	ExcludeStrategies   []string `json:"exclude_strategies,omitempty"` // стратегии, которые не пробовать
	Targets             []Target `json:"targets,omitempty"`            // свои адреса для проверки вместо стандартных
}

func defaultConfig() Config {
	return Config{
		AutoHeal:            true,
		AutoUpdate:          true,
		UpdateIpset:         true,
		UpdateListsFromMain: true,
		UpdateHosts:         true,
		CheckIntervalSec:    300,
		UpdateIntervalHours: 6,
		MinSweepIntervalMin: 30,
		FailuresBeforeHeal:  3,
		HealthyRatio:        0.75,
		KeepVersions:        2,
		Repo:                "Flowseal/zapret-discord-youtube",
		Branch:              "main",
	}
}

func (c *Config) normalize() {
	d := defaultConfig()
	if c.CheckIntervalSec < 60 {
		c.CheckIntervalSec = d.CheckIntervalSec
	}
	if c.UpdateIntervalHours < 1 {
		c.UpdateIntervalHours = d.UpdateIntervalHours
	}
	if c.MinSweepIntervalMin < 5 {
		c.MinSweepIntervalMin = d.MinSweepIntervalMin
	}
	if c.FailuresBeforeHeal < 1 {
		c.FailuresBeforeHeal = d.FailuresBeforeHeal
	}
	if c.HealthyRatio <= 0 || c.HealthyRatio > 1 {
		c.HealthyRatio = d.HealthyRatio
	}
	if c.KeepVersions < 1 {
		c.KeepVersions = d.KeepVersions
	}
	if strings.TrimSpace(c.Repo) == "" {
		c.Repo = d.Repo
	}
	if strings.TrimSpace(c.Branch) == "" {
		c.Branch = d.Branch
	}
}

// State — что менеджер помнит между запусками (state.json).
type State struct {
	ReleaseDir      string        `json:"release_dir,omitempty"`
	Version         string        `json:"version,omitempty"`
	Strategy        string        `json:"strategy,omitempty"`
	LatestVersion   string        `json:"latest_version,omitempty"`
	LastUpdateCheck time.Time     `json:"last_update_check"`
	LastIpsetUpdate time.Time     `json:"last_ipset_update"`
	LastListsUpdate time.Time     `json:"last_lists_update"`
	LastHostsUpdate time.Time     `json:"last_hosts_update"`
	LastHeal        time.Time     `json:"last_heal"`
	HealStreak      int           `json:"heal_streak"` // сколько подборов подряд не нашли рабочую стратегию
	LastDNSWarn     time.Time     `json:"last_dns_warn"`
	PausedUntil     time.Time     `json:"paused_until"`
	BadVersions     []string      `json:"bad_versions,omitempty"`
	LastCheck       *CheckSummary `json:"last_check,omitempty"`
	LastSweep       *SweepSummary `json:"last_sweep,omitempty"`
}

type CheckSummary struct {
	Time        time.Time `json:"time"`
	Strategy    string    `json:"strategy"`
	Discord     string    `json:"discord"`
	YouTube     string    `json:"youtube"`
	Other       string    `json:"other,omitempty"`
	Healthy     bool      `json:"healthy"`
	NetworkDown bool      `json:"network_down"`
	DNSProblem  bool      `json:"dns_problem"`
	Failures    []string  `json:"failures,omitempty"`
}

type SweepSummary struct {
	Time    time.Time   `json:"time"`
	Version string      `json:"version"`
	Dir     string      `json:"dir"`
	Ranking []RankEntry `json:"ranking"`
}

type App struct {
	Root    string
	Cfg     Config
	Log     *Logger
	stateMu sync.Mutex

	lastStartFail time.Time // чтобы ensureRunning не долбил SCM каждые 30 с, если служба не стартует
}

func newApp(dirFlag string, console bool) *App {
	root := resolveRoot(dirFlag)
	a := &App{Root: root, Cfg: defaultConfig()}
	logPath := filepath.Join(root, "logs", "manager.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		logPath = ""
	}
	a.Log = &Logger{path: logPath, console: console}
	a.reloadConfig()
	return a
}

// resolveRoot: --dir, затем папка exe (если там уже установлен менеджер),
// затем папка exe службы zapret-manager, иначе C:\zapret.
func resolveRoot(dirFlag string) string {
	if dirFlag != "" {
		if abs, err := filepath.Abs(dirFlag); err == nil {
			return abs
		}
		return dirFlag
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if fileExists(filepath.Join(dir, "config.json")) || dirExists(filepath.Join(dir, "releases")) {
			return dir
		}
	}
	if info := queryService(managerSvcName); info.Exists && info.BinPath != "" {
		if exe := exeFromCmdline(info.BinPath); exe != "" {
			return filepath.Dir(exe)
		}
	}
	return defaultRoot
}

func (a *App) path(parts ...string) string {
	return filepath.Join(append([]string{a.Root}, parts...)...)
}

func (a *App) releasesDir() string { return a.path("releases") }
func (a *App) customDir() string   { return a.path("custom") }

func (a *App) reloadConfig() {
	cfg := defaultConfig()
	if data, err := os.ReadFile(a.path("config.json")); err == nil {
		if err := json.Unmarshal(stripBOM(data), &cfg); err != nil {
			a.Log.Warnf("config.json не читается (%v) — беру настройки по умолчанию", err)
			cfg = defaultConfig()
		}
	}
	cfg.normalize()
	a.Cfg = cfg
}

func (a *App) saveConfigIfMissing() error {
	p := a.path("config.json")
	if fileExists(p) {
		return nil
	}
	data, _ := json.MarshalIndent(defaultConfig(), "", "  ")
	return writeFileAtomic(p, data)
}

func (a *App) loadState() State {
	var st State
	if data, err := os.ReadFile(a.path("state.json")); err == nil {
		_ = json.Unmarshal(stripBOM(data), &st)
	}
	return st
}

// updateState перечитывает state.json, применяет fn и сохраняет. Между процессами
// (служба и консольная команда) возможна гонка, но обе пишут под общей блокировкой.
func (a *App) updateState(fn func(*State)) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	st := a.loadState()
	fn(&st)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(a.path("state.json"), data); err != nil {
		a.Log.Debugf("state.json не сохранён: %v", err)
	}
}

var errBusy = errors.New("сейчас идёт другая операция (подбор или обновление) — попробуйте через пару минут")

// withLock — общая блокировка между службой и консольными командами: файл .lock,
// открытый без общего доступа. Держит процесс; при падении процесса снимается сам.
func (a *App) withLock(timeout time.Duration, fn func() error) error {
	if err := os.MkdirAll(a.Root, 0o755); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(a.path(".lock"))
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	announced := false
	for {
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
			windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			defer windows.CloseHandle(h)
			return fn()
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return fmt.Errorf("блокировка %s: %w", a.path(".lock"), err)
		}
		if time.Now().After(deadline) {
			return errBusy
		}
		if !announced && timeout > 5*time.Second {
			a.Log.Infof("жду, пока служба закончит текущую операцию…")
			announced = true
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------- лог ----------

type Logger struct {
	mu      sync.Mutex
	path    string
	console bool
}

const (
	lvlInfo  = "[ ]"
	lvlEvent = "[*]"
	lvlWarn  = "[!]"
	lvlDebug = "[.]"
)

func (l *Logger) write(level, msg string, toConsole bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.console && toConsole {
		switch level {
		case lvlWarn:
			fmt.Println("! " + msg)
		default:
			fmt.Println(msg)
		}
	}
	if l.path == "" {
		return
	}
	if fi, err := os.Stat(l.path); err == nil && fi.Size() > 2<<20 {
		_ = os.Remove(l.path + ".1")
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, line := range strings.Split(strings.TrimRight(msg, "\r\n"), "\n") {
		fmt.Fprintf(f, "%s %s %s\r\n", time.Now().Format("2006-01-02 15:04:05"), level, strings.TrimRight(line, "\r"))
	}
}

func (l *Logger) Infof(format string, a ...any)  { l.write(lvlInfo, fmt.Sprintf(format, a...), true) }
func (l *Logger) Eventf(format string, a ...any) { l.write(lvlEvent, fmt.Sprintf(format, a...), true) }
func (l *Logger) Warnf(format string, a ...any)  { l.write(lvlWarn, fmt.Sprintf(format, a...), true) }
func (l *Logger) Debugf(format string, a ...any) { l.write(lvlDebug, fmt.Sprintf(format, a...), false) }

// ---------- файлы ----------

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		// файл может быть занят (winws держит открытым) — пишем поверх
		return os.WriteFile(p, data, 0o644)
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(p, target)
	})
}

func samePath(a, b string) bool {
	ca, _ := filepath.Abs(a)
	cb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

// normalizeText приводит текст к LF без хвостовых пробелов строк — для сравнения списков.
func normalizeText(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	return strings.ReplaceAll(s, "\n", "\r\n") + "\r\n"
}
