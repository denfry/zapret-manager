package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"zapret-manager/internal/batch"
	"zapret-manager/internal/textutil"
)

// Config — настройки менеджера (config.json в корневой папке). Настройки самого
// zapret (game filter, режим ipset, *-user.txt) живут в папке релиза в тех же
// файлах, что читает service.bat, чтобы его меню продолжало работать.
type Config struct {
	AutoHeal            bool     `json:"auto_heal"`              // сам перезапускать / подбирать стратегию, когда Discord, YouTube или включённые сайты отвалились
	AutoUpdate          bool     `json:"auto_update"`            // сам ставить новые релизы Flowseal
	UpdateIpset         bool     `json:"update_ipset"`           // обновлять ipset-all.txt из .service/ipset-service.txt
	UpdateListsFromMain bool     `json:"update_lists_from_main"` // подтягивать list-*.txt из ветки main между релизами
	UpdateHosts         bool     `json:"update_hosts"`           // держать в hosts актуальный блок из .service/hosts
	CheckIntervalSec    int      `json:"check_interval_sec"`     // как часто проверять, что всё открывается
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

// repo и branch подставляются в URL GitHub — только безопасные символы.
var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
)

// maxTargets ограничивает число адресов: каждая проверка — отдельное соединение.
const maxTargets = 50

// normalize подставляет значения по умолчанию вместо недопустимых и возвращает
// предупреждения о том, что пришлось исправить.
func (c *Config) normalize() []string {
	d := defaultConfig()
	var warns []string
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
	c.Repo, c.Branch = strings.TrimSpace(c.Repo), strings.TrimSpace(c.Branch)
	if c.Repo == "" {
		c.Repo = d.Repo
	} else if !repoRe.MatchString(c.Repo) || strings.Contains(c.Repo, "..") {
		warns = append(warns, fmt.Sprintf("repo %q не похож на «владелец/репозиторий» — беру %s", c.Repo, d.Repo))
		c.Repo = d.Repo
	}
	if c.Branch == "" {
		c.Branch = d.Branch
	} else if !branchRe.MatchString(c.Branch) || strings.Contains(c.Branch, "..") {
		warns = append(warns, fmt.Sprintf("branch %q недопустима — беру %s", c.Branch, d.Branch))
		c.Branch = d.Branch
	}
	var targets []Target
	for _, t := range c.Targets {
		u, err := url.Parse(strings.TrimSpace(t.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
			warns = append(warns, fmt.Sprintf("адрес проверки %q пропущен: нужен http(s)://хост/…", t.URL))
			continue
		}
		t.URL = u.String()
		t.Group = strings.ToLower(strings.TrimSpace(t.Group))
		if t.Group == "" {
			t.Group = "other"
		}
		if strings.TrimSpace(t.Name) == "" {
			t.Name = u.Host
		}
		targets = append(targets, t)
	}
	if len(targets) > maxTargets {
		warns = append(warns, fmt.Sprintf("адресов проверки %d, беру первые %d", len(targets), maxTargets))
		targets = targets[:maxTargets]
	}
	c.Targets = targets
	return warns
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
	Time        time.Time            `json:"time"`
	Strategy    string               `json:"strategy"`
	Discord     string               `json:"discord"`
	YouTube     string               `json:"youtube"`
	Other       string               `json:"other,omitempty"`
	Groups      map[string]GroupStat `json:"groups,omitempty"`  // всё, кроме Discord/YouTube/control: сервисы, свои сайты
	Skipped     []string             `json:"skipped,omitempty"` // группы, которые zapret не открывает
	Healthy     bool                 `json:"healthy"`
	NetworkDown bool                 `json:"network_down"`
	DNSProblem  bool                 `json:"dns_problem"`
	Failures    []string             `json:"failures,omitempty"`
}

type SweepSummary struct {
	Time    time.Time   `json:"time"`
	Version string      `json:"version"`
	Dir     string      `json:"dir"`
	Ranking []RankEntry `json:"ranking"`
	// Unfixable — группы, которые на этом подборе не открыла ни одна стратегия.
	// Сутки после подбора сторож их не чинит (App.skippedGroups).
	Unfixable []string `json:"unfixable,omitempty"`
}

// App — корневая папка, конфиг и лог одного процесса (консольной команды или службы).
// Методы App вызываются из одной горутины, кроме Log и updateState (они под мьютексами).
type App struct {
	Root       string
	Cfg        Config
	Log        *Logger
	downloader *downloader // nil — общий defaultDownloader; тесты подставляют свой
	rawBase    string      // "" — https://raw.githubusercontent.com (тесты подставляют свой сервер)
	githubBase string      // "" — https://github.com
	stateMu    sync.Mutex

	cfgWarns      string    // последние предупреждения конфига — чтобы не повторять их каждые 5 минут
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
		if exe := batch.ExeFromCmdline(info.BinPath); exe != "" {
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
	var warns []string
	if data, err := os.ReadFile(a.path("config.json")); err == nil {
		if err := json.Unmarshal(textutil.StripBOM(data), &cfg); err != nil {
			warns = append(warns, fmt.Sprintf("config.json не читается (%v) — беру настройки по умолчанию", err))
			cfg = defaultConfig()
		}
	}
	warns = append(warns, cfg.normalize()...)
	if joined := strings.Join(warns, "\n"); joined != a.cfgWarns {
		a.cfgWarns = joined
		for _, w := range warns {
			a.Log.Warnf("config.json: %s", w)
		}
	}
	a.Cfg = cfg
}

func (a *App) saveConfigIfMissing() error {
	p := a.path("config.json")
	if fileExists(p) {
		return nil
	}
	data, err := json.MarshalIndent(defaultConfig(), "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, data)
}

// errStateCorrupt — state.json прочитан, но это не JSON состояния. Только такую ошибку
// можно считать порчей: прочие (файл занят антивирусом, нет прав) — временные.
var errStateCorrupt = errors.New("state.json испорчен")

// readState читает state.json. Отсутствующий файл — пустое состояние без ошибки.
func (a *App) readState() (State, error) {
	var st State
	data, err := os.ReadFile(a.path("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(textutil.StripBOM(data), &st); err != nil {
		return State{}, fmt.Errorf("%w: %w", errStateCorrupt, err)
	}
	return st, nil
}

// loadState — состояние для чтения; при ошибке пустое (status и проверки не должны падать).
// Читает без блокировки, поэтому временную ошибку (файл как раз заменяют) повторяет:
// пустое состояние вместо настоящего — это, например, «паузы нет», и сторож включил бы
// zapret, выключенный командой off.
func (a *App) loadState() State {
	var st State
	var err error
	for range 5 {
		if st, err = a.readState(); err == nil {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	return State{}
}

// stateLockTimeout — сколько ждать, пока другой процесс допишет state.json.
const stateLockTimeout = 10 * time.Second

// updateState перечитывает state.json, применяет fn и сохраняет. Служба и консольные
// команды — разные процессы, поэтому чтение-изменение-запись идёт под отдельной
// файловой блокировкой .state.lock (общую .lock надолго занимает подбор).
// fn не должна вызывать updateState.
func (a *App) updateState(fn func(*State)) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	release, err := acquireFileLock(a.path(".state.lock"), stateLockTimeout, nil)
	if err != nil {
		// без прав администратора папка только для чтения — это нормально для status/check
		a.Log.Debugf("state.json не сохранён: %v", err)
		return
	}
	defer release()
	st, err := a.readState()
	if err != nil && !errors.Is(err, errStateCorrupt) {
		// файл не прочитать прямо сейчас — не сохраняем, чтобы не затереть его пустым
		a.Log.Warnf("state.json не прочитать (%v) — состояние не сохранено", err)
		return
	}
	if err != nil {
		// испорченный файл не затираем молча: откладываем копию для разбора
		bad := a.path(fmt.Sprintf("state.json.corrupt-%s", time.Now().Format("20060102-150405")))
		if rerr := os.Rename(a.path("state.json"), bad); rerr != nil {
			a.Log.Warnf("state.json не читается (%v) и не переименовать (%v) — состояние не сохранено", err, rerr)
			return
		}
		a.Log.Warnf("state.json не читается (%v) — отложен в %s, начинаю с чистого состояния", err, filepath.Base(bad))
		st = State{}
	}
	fn(&st)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		a.Log.Warnf("state.json не сохранён: %v", err)
		return
	}
	if err := writeFileReplace(a.path("state.json"), data); err != nil {
		a.Log.Warnf("state.json не сохранён: %v", err)
	}
}

var errBusy = errors.New("сейчас идёт другая операция (подбор или обновление) — попробуйте через пару минут")

const lockPollInterval = 500 * time.Millisecond

// acquireFileLock — межпроцессная блокировка: файл, открытый без общего доступа.
// Держит процесс; при его падении Windows закрывает дескриптор и блокировка снимается.
// timeout 0 — одна попытка. onWait вызывается один раз, если пришлось ждать.
func acquireFileLock(p string, timeout time.Duration, onWait func()) (release func(), err error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	waited := false
	for {
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
			windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			return func() { _ = windows.CloseHandle(h) }, nil
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, fmt.Errorf("блокировка %s: %w", p, err)
		}
		if !time.Now().Before(deadline) {
			return nil, errBusy
		}
		if !waited && onWait != nil {
			onWait()
			waited = true
		}
		time.Sleep(lockPollInterval)
	}
}

// withLock — общая блокировка долгих операций (подбор, обновление, смена стратегии)
// между службой и консольными командами.
func (a *App) withLock(timeout time.Duration, fn func() error) error {
	if err := os.MkdirAll(a.Root, 0o755); err != nil {
		return err
	}
	release, err := acquireFileLock(a.path(".lock"), timeout, func() {
		if timeout > 5*time.Second {
			a.Log.Infof("жду, пока служба закончит текущую операцию…")
		}
	})
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// ---------- лог ----------

// Logger пишет в logs\manager.log (общий для службы и консоли) и, если console,
// дублирует Info/Event/Warn на экран. Файл открывается на каждую запись: так
// ротация и запись из двух процессов не держат дескриптор.
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

	logTimeLayout = "2006-01-02 15:04:05"
	maxLogBytes   = 2 << 20
)

func (l *Logger) write(level, msg string, toConsole bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.console && toConsole {
		if level == lvlWarn {
			fmt.Println("! " + msg)
		} else {
			fmt.Println(msg)
		}
	}
	if l.path == "" {
		return
	}
	if fi, err := os.Stat(l.path); err == nil && fi.Size() > maxLogBytes {
		_ = os.Remove(l.path + ".1")
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // best-effort log
	ts := time.Now().Format(logTimeLayout)
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(msg, "\r\n"), "\n") {
		b.WriteString(ts + " " + level + " " + strings.TrimRight(line, "\r") + "\r\n")
	}
	_, _ = f.WriteString(b.String())
}

func (l *Logger) Infof(format string, a ...any)  { l.write(lvlInfo, fmt.Sprintf(format, a...), true) }
func (l *Logger) Eventf(format string, a ...any) { l.write(lvlEvent, fmt.Sprintf(format, a...), true) }
func (l *Logger) Warnf(format string, a ...any)  { l.write(lvlWarn, fmt.Sprintf(format, a...), true) }
func (l *Logger) Debugf(format string, a ...any) { l.write(lvlDebug, fmt.Sprintf(format, a...), false) }

// parseEventLine разбирает строку лога уровня Event: «2006-01-02 15:04:05 [*] текст».
// Уровень ищется строго после метки времени, чтобы «[*]» внутри текста другой
// строки (например, из ответа сервера) не выдавал её за событие.
func parseEventLine(l string) (time.Time, string, bool) {
	prefix := len(logTimeLayout)
	marker := " " + lvlEvent + " "
	if len(l) < prefix+len(marker) || l[prefix:prefix+len(marker)] != marker {
		return time.Time{}, "", false
	}
	ts, err := time.ParseInLocation(logTimeLayout, l[:prefix], time.Local)
	if err != nil {
		return time.Time{}, "", false
	}
	return ts, l[prefix+len(marker):], true
}

// ---------- файлы ----------

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// writeFileAtomic пишет во временный файл рядом (уникальное имя — два процесса не
// мешают друг другу), сбрасывает на диск и переименовывает поверх.
// Если заменить не удалось (файл держит открытым winws), пишет поверх — так списки
// обновляются и при работающем zapret.
func writeFileAtomic(p string, data []byte) error {
	return writeFileTemp(p, data, true)
}

// writeFileReplace — как writeFileAtomic, но никогда не пишет поверх: файл либо
// заменяется целиком, либо остаётся прежним. Для state.json, который другой процесс
// читает без блокировки, — иначе он прочитал бы половину файла.
func writeFileReplace(p string, data []byte) error {
	return writeFileTemp(p, data, false)
}

// renameRetries — замену ненадолго блокирует читатель (Go открывает файлы без
// FILE_SHARE_DELETE): несколько попыток с паузой.
const renameRetries = 20

func writeFileTemp(p string, data []byte, overwriteFallback bool) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	var rerr error
	for i := range renameRetries {
		if rerr = os.Rename(tmp, p); rerr == nil {
			return nil
		}
		if i < renameRetries-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	_ = os.Remove(tmp) // best effort: временный файл больше не нужен
	if !overwriteFallback {
		return rerr
	}
	// файл может быть занят (winws держит списки открытыми) — пишем поверх
	return os.WriteFile(p, data, 0o644)
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
		if !fi.Mode().IsRegular() {
			return nil // ссылки и устройства не копируем
		}
		return copyFile(p, target)
	})
}

func samePath(a, b string) bool {
	ca, _ := filepath.Abs(a)
	cb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

// isWithin: p лежит внутри dir или совпадает с ней (без учёта регистра).
func isWithin(p, dir string) bool {
	cp, _ := filepath.Abs(p)
	cd, _ := filepath.Abs(dir)
	cp, cd = strings.ToLower(filepath.Clean(cp)), strings.ToLower(filepath.Clean(cd))
	return cp == cd || strings.HasPrefix(cp, strings.TrimRight(cd, `\`)+`\`)
}
