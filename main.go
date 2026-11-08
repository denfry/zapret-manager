package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	appVersion     = "1.0.0"
	managerSvcName = "zapret-manager"
	zapretSvcName  = "zapret"
	defaultRoot    = `C:\zapret`
)

type command struct {
	name  string
	admin bool
	usage string
	run   func(ctx context.Context, a *App, args []string) int
}

var commands []command

func init() {
	commands = []command{
		{"install", true, "install [--strategy ИМЯ] [--from ПАПКА] [--select] — поставить в C:\\zapret, перенести текущую установку, включить службу-сторож", cmdInstall},
		{"status", false, "status — что стоит и как работает", cmdStatus},
		{"check", false, "check — проверить Discord и YouTube прямо сейчас", cmdCheck},
		{"select", true, "select [--quick] [--dry-run] — перебрать стратегии и поставить лучшую", cmdSelect},
		{"use", true, "use ИМЯ — поставить стратегию вручную (например: use ALT11)", cmdUse},
		{"list", false, "list — стратегии и результаты последнего подбора", cmdList},
		{"update", true, "update [--force] — обновить zapret, ipset, списки и hosts", cmdUpdate},
		{"hosts", true, "hosts — обновить только hosts", cmdHosts},
		{"off", true, "off [МИНУТЫ] — выключить zapret (и сторожа) на время или до команды on", cmdOff},
		{"on", true, "on — включить обратно", cmdOn},
		{"doctor", true, "doctor — диагностика конфликтов (как Run Diagnostics в service.bat)", cmdDoctor},
		{"log", false, "log [N] — последние N строк журнала", cmdLog},
		{"uninstall", true, "uninstall [--all] — убрать службу-сторож (--all: ещё zapret, WinDivert и блок в hosts)", cmdUninstall},
		{"version", false, "version", func(context.Context, *App, []string) int { fmt.Println("zapret-manager", appVersion); return 0 }},
	}
}

func main() {
	if inService, _ := svc.IsWindowsService(); inService {
		runAsService()
		return
	}
	setupConsole()
	os.Exit(runCLI(os.Args[1:]))
}

func usage() {
	fmt.Println("zapret-manager " + appVersion + " — автообновление и автопочинка zapret-discord-youtube")
	fmt.Println()
	fmt.Println("Команды:")
	for _, c := range commands {
		fmt.Println("  " + c.usage)
	}
	fmt.Println()
	fmt.Println("Общие флаги: --dir ПАПКА (по умолчанию C:\\zapret)")
}

func runCLI(args []string) int {
	pause := false
	dir := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--pause":
			pause = true
		case a == "--dir" && i+1 < len(args):
			dir = args[i+1]
			i++
		case strings.HasPrefix(a, "--dir="):
			dir = strings.TrimPrefix(a, "--dir=")
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		usage()
		fmt.Println()
		a := newApp(dir, true)
		cmdStatus(context.Background(), a, nil)
		if launchedFromExplorer() {
			waitEnter()
		}
		return 0
	}
	name := strings.ToLower(rest[0])
	if name == "help" || name == "-h" || name == "--help" || name == "/?" {
		usage()
		return 0
	}
	var cmd *command
	for i := range commands {
		if commands[i].name == name {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		fmt.Printf("Нет такой команды: %s\n\n", rest[0])
		usage()
		return 2
	}
	if cmd.admin && !isElevated() {
		return relaunchElevated(args)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := newApp(dir, true)
	code := cmd.run(ctx, a, rest[1:])
	if pause || launchedFromExplorer() {
		waitEnter()
	}
	return code
}

func waitEnter() {
	fmt.Println()
	fmt.Print("Нажмите Enter, чтобы закрыть окно…")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	return fs
}

func fail(err error) int {
	fmt.Println("Ошибка:", err)
	return 1
}

// ---------- install ----------

func cmdInstall(ctx context.Context, a *App, args []string) int {
	fs := newFlags("install")
	strategyFlag := fs.String("strategy", "", "стратегия (по умолчанию — та, что уже стоит, иначе подбор)")
	fromFlag := fs.String("from", "", "папка старой установки zapret для переноса настроек")
	selectFlag := fs.Bool("select", false, "подобрать стратегию заново, даже если она уже выбрана")
	noWatch := fs.Bool("no-watchdog", false, "не ставить службу-сторож")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Printf("Ставлю zapret-manager %s в %s\n\n", appVersion, a.Root)

	if err := stopManagerIfRunning(); err != nil {
		return fail(err)
	}
	for _, d := range []string{a.Root, a.releasesDir(), a.customDir(), a.path("logs"), a.path("backups")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fail(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	target := a.path("zapret-manager.exe")
	if !samePath(exe, target) {
		var cerr error
		for i := 0; i < 10; i++ {
			if cerr = copyFile(exe, target); cerr == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if cerr != nil {
			return fail(fmt.Errorf("не скопировать себя в %s: %w", target, cerr))
		}
		fmt.Println("✓ программа скопирована в", target)
	}
	if err := a.saveConfigIfMissing(); err != nil {
		return fail(err)
	}
	a.reloadConfig()

	err = a.withLock(3*time.Minute, func() error {
		oldDir, oldStrategy := a.currentInstall()
		if *fromFlag != "" {
			if !validRelease(*fromFlag) {
				return fmt.Errorf("%s не похожа на папку zapret-discord-youtube", *fromFlag)
			}
			oldDir = *fromFlag
			if oldStrategy == "" {
				oldStrategy = readRegistryStrategy()
			}
		}
		if oldDir != "" {
			fmt.Printf("✓ нашёл текущую установку: %s (zapret %s, стратегия %s)\n", oldDir, releaseVersion(oldDir), orDash(oldStrategy))
			fmt.Printf("  game filter: %s, ipset: %s\n", readGameFilter(oldDir), ipsetMode(oldDir))
		}

		var newDir string
		ver, verr := a.fetchLatestVersion(ctx)
		if verr == nil {
			fmt.Printf("✓ последняя версия Flowseal: %s\n", ver)
			newDir, verr = a.downloadRelease(ctx, ver, false)
		}
		if verr != nil {
			if oldDir == "" {
				return fmt.Errorf("не скачать zapret: %w", verr)
			}
			fmt.Printf("! не скачать релиз (%v) — переношу текущую папку как есть\n", verr)
			newDir = filepath.Join(a.releasesDir(), releaseVersion(oldDir))
			if !samePath(oldDir, newDir) {
				if err := copyDir(oldDir, newDir); err != nil {
					return err
				}
			}
		}
		fmt.Printf("✓ zapret %s в %s\n", releaseVersion(newDir), newDir)
		if oldDir != "" && !samePath(oldDir, newDir) {
			migrateUserFiles(oldDir, newDir)
			fmt.Println("✓ перенёс свои списки, game filter и режим ipset")
		}
		if err := ensureUserLists(newDir); err != nil {
			return err
		}

		if a.Cfg.UpdateIpset {
			if changed, err := a.updateIpset(ctx, newDir); err != nil {
				fmt.Println("! ipset:", err)
			} else {
				a.updateState(func(st *State) { st.LastIpsetUpdate = time.Now() })
				fmt.Printf("✓ ipset %s (режим %s)\n", map[bool]string{true: "обновлён", false: "актуален"}[changed], ipsetMode(newDir))
			}
		}
		if a.Cfg.UpdateListsFromMain {
			changed, err := a.updateMainLists(ctx, newDir)
			if err != nil {
				fmt.Println("! списки:", err)
			}
			a.updateState(func(st *State) { st.LastListsUpdate = time.Now() })
			if len(changed) > 0 {
				fmt.Println("✓ списки из main:", strings.Join(changed, ", "))
			} else if err == nil {
				fmt.Println("✓ списки доменов актуальны")
			}
		}
		if a.Cfg.UpdateHosts {
			if changed, err := a.updateHosts(ctx); err != nil {
				fmt.Println("! hosts:", err)
			} else {
				a.updateState(func(st *State) { st.LastHostsUpdate = time.Now() })
				fmt.Printf("✓ hosts %s\n", map[bool]string{true: "обновлён (старая копия в backups\\)", false: "актуален"}[changed])
			}
		}
		a.updateState(func(st *State) { st.LastUpdateCheck = time.Now() })

		name := *strategyFlag
		if name == "" {
			name = oldStrategy
		}
		if name != "" {
			if s, err := a.findStrategy(newDir, name); err != nil {
				fmt.Printf("! %v — подберу заново\n", err)
				name = ""
			} else {
				name = s.Name
			}
		}
		if *selectFlag || name == "" {
			fmt.Println()
			best, ranking, err := a.selectStrategy(ctx, newDir, name, selectOpts{apply: true, progress: func(s string) { fmt.Println(s) }})
			printRanking(ranking, 5)
			if err != nil {
				if errors.Is(err, errNothingWorks) && best != nil {
					fmt.Println("! ни одна стратегия не открыла всё — ставлю лучшую из имеющихся")
					if aerr := a.applyStrategy(newDir, best.Strategy); aerr != nil {
						return aerr
					}
				} else {
					return err
				}
			}
		} else {
			if err := a.applyStrategy(newDir, name); err != nil {
				return err
			}
			fmt.Println("✓ служба zapret со стратегией", name)
			_ = sleepCtx(ctx, 2*time.Second)
			rep := a.checkNow(ctx)
			printReport(rep)
			if !rep.Healthy && !rep.NetworkDown {
				fmt.Println("\nСтратегия не открывает всё — подбираю лучшую…")
				best, ranking, err := a.selectStrategy(ctx, newDir, name, selectOpts{apply: true, progress: func(s string) { fmt.Println(s) }})
				printRanking(ranking, 5)
				if err != nil && !errors.Is(err, errNothingWorks) {
					return err
				}
				if best != nil {
					fmt.Println("✓ стоит", best.Strategy)
				}
			}
		}
		a.pruneReleases(newDir)
		return nil
	})
	if err != nil {
		return fail(err)
	}

	if !*noWatch {
		if err := installManagerService(target); err != nil {
			return fail(err)
		}
		if err := startService(managerSvcName, 15*time.Second); err != nil {
			return fail(err)
		}
		fmt.Println("✓ служба-сторож zapret-manager запущена")
	}
	a.Log.Eventf("установка завершена")
	fmt.Println()
	cmdStatus(ctx, a, nil)
	relDir, _ := a.currentInstall()
	if old := strings.TrimSpace(*fromFlag); old != "" && !samePath(old, relDir) {
		fmt.Printf("\nСтарую папку %s можно удалить.\n", old)
	}
	return 0
}

// ---------- status ----------

func cmdStatus(_ context.Context, a *App, _ []string) int {
	st := a.loadState()
	relDir, strategy := a.currentInstall()
	fmt.Printf("zapret-manager %s, папка %s\n\n", appVersion, a.Root)
	if relDir == "" {
		fmt.Println("zapret не установлен. Запустите: zapret-manager install")
		return 0
	}
	ver := releaseVersion(relDir)
	latest := ""
	if st.LatestVersion != "" && st.LatestVersion != ver {
		latest = " (последняя " + st.LatestVersion + ")"
	}
	fmt.Printf("zapret:      %s%s — %s\n", ver, latest, relDir)
	fmt.Printf("стратегия:   %s\n", orDash(strategy))
	zi := queryService(zapretSvcName)
	switch {
	case !zi.Exists:
		fmt.Println("служба zapret:   нет")
	case zi.State == svc.Running:
		fmt.Printf("служба zapret:   работает (winws.exe pid %d)\n", zi.PID)
	default:
		fmt.Printf("служба zapret:   %s\n", stateName(zi.State))
	}
	mi := queryService(managerSvcName)
	if mi.Exists {
		fmt.Printf("сторож:          %s\n", stateName(mi.State))
	} else {
		fmt.Println("сторож:          не установлен")
	}
	if !st.PausedUntil.IsZero() && time.Now().Before(st.PausedUntil) {
		if st.PausedUntil.Year() > 3000 {
			fmt.Println("пауза:           до команды on")
		} else {
			fmt.Printf("пауза:           до %s\n", st.PausedUntil.Format("02.01 15:04"))
		}
	}
	mode := ipsetMode(relDir)
	ipsetInfo := mode
	if mode == "loaded" {
		ipsetInfo += fmt.Sprintf(" (%d подсетей)", countIpsetLines(relDir))
	}
	fmt.Printf("game filter:     %s\nipset:           %s\n", readGameFilter(relDir), ipsetInfo)

	if c := st.LastCheck; c != nil {
		verdict := "всё открывается"
		switch {
		case c.NetworkDown:
			verdict = "не было интернета"
		case c.DNSProblem:
			verdict = "проблема DNS"
		case !c.Healthy:
			verdict = "есть проблемы: " + strings.Join(c.Failures, "; ")
		}
		other := ""
		if c.Other != "" {
			other = ", " + c.Other
		}
		fmt.Printf("\nпоследняя проверка: %s — Discord %s, YouTube %s%s — %s\n", c.Time.Format("02.01 15:04"), c.Discord, c.YouTube, other, verdict)
	}
	if sw := st.LastSweep; sw != nil && len(sw.Ranking) > 0 {
		var top []string
		for i, e := range sw.Ranking {
			if i >= 3 || e.Score == 0 {
				break
			}
			top = append(top, fmt.Sprintf("%s (%d/%d)", e.Strategy, e.Score, e.Max))
		}
		fmt.Printf("последний подбор:   %s — %s\n", sw.Time.Format("02.01 15:04"), strings.Join(top, ", "))
	}
	if !st.LastUpdateCheck.IsZero() {
		fmt.Printf("обновления:         проверены %s; ipset %s; hosts %s\n",
			st.LastUpdateCheck.Format("02.01 15:04"), fmtDate(st.LastIpsetUpdate), fmtDate(st.LastHostsUpdate))
	}
	if events := a.lastEvents(8); len(events) > 0 {
		fmt.Println("\nсобытия:")
		for _, e := range events {
			fmt.Println("  " + e)
		}
	}
	return 0
}

func (a *App) lastEvents(n int) []string {
	data, err := os.ReadFile(a.path("logs", "manager.log"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		if i := strings.Index(l, " "+lvlEvent+" "); i > 0 {
			ts, err := time.ParseInLocation("2006-01-02 15:04:05", l[:i], time.Local)
			msg := l[i+len(lvlEvent)+2:]
			if err == nil {
				out = append(out, ts.Format("02.01 15:04")+"  "+msg)
			} else {
				out = append(out, msg)
			}
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// ---------- check / select / use / list ----------

func cmdCheck(ctx context.Context, a *App, _ []string) int {
	_, strategy := a.currentInstall()
	fmt.Printf("Проверяю (стратегия %s)…\n\n", orDash(strategy))
	rep := a.checkNow(ctx)
	printReport(rep)
	if rep.Healthy {
		return 0
	}
	return 1
}

func printReport(rep *HealthReport) {
	for _, r := range rep.Results {
		mark := "✓"
		info := fmt.Sprintf("HTTP %d, %d КБ, %d мс", r.Status, r.Bytes/1024, r.Elapsed.Milliseconds())
		if !r.OK {
			mark = "✗"
			info = kindText(r)
		}
		fmt.Printf("  %s %-20s %s\n", mark, r.Name, info)
	}
	fmt.Println()
	switch {
	case rep.NetworkDown:
		fmt.Println("Итог: интернета нет (контрольные сайты тоже не открываются).")
	case rep.Healthy:
		fmt.Printf("Итог: %s — работает.\n", rep.shortText())
	case rep.DNSProblem:
		fmt.Printf("Итог: %s — похоже на подмену DNS, включите DNS-over-HTTPS.\n", rep.shortText())
	default:
		fmt.Printf("Итог: %s — есть проблемы.\n", rep.shortText())
	}
}

func printRanking(r []RankEntry, n int) {
	if len(r) == 0 {
		return
	}
	fmt.Println("\nЛучшие:")
	for i, e := range r {
		if i >= n {
			break
		}
		fmt.Printf("  %d. %s\n", i+1, e.Line())
	}
}

func cmdSelect(ctx context.Context, a *App, args []string) int {
	fs := newFlags("select")
	quick := fs.Bool("quick", false, "сперва проверить лучших из прошлого подбора")
	dry := fs.Bool("dry-run", false, "только проверить, ничего не менять")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := a.withLock(3*time.Minute, func() error {
		relDir, current := a.currentInstall()
		if relDir == "" {
			return errors.New("zapret не установлен — сначала zapret-manager install")
		}
		best, ranking, err := a.selectStrategy(ctx, relDir, current, selectOpts{quick: *quick, apply: !*dry, progress: func(s string) { fmt.Println(s) }})
		printRanking(ranking, 8)
		if err != nil {
			return err
		}
		fmt.Println()
		switch {
		case *dry:
			fmt.Printf("Лучшая: %s (ничего не менял, --dry-run)\n", best.Strategy)
		case strings.EqualFold(best.Strategy, current):
			fmt.Printf("Текущая стратегия %s и так лучшая.\n", current)
			a.Log.Eventf("подбор вручную: %s осталась лучшей", current)
		default:
			fmt.Printf("Поставил %s (была %s).\n", best.Strategy, orDash(current))
			a.Log.Eventf("подбор вручную: %s → %s", orDash(current), best.Strategy)
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

func cmdUse(ctx context.Context, a *App, args []string) int {
	if len(args) == 0 {
		fmt.Println("Укажите стратегию: zapret-manager use ALT11")
		return 2
	}
	name := strings.Join(args, " ")
	err := a.withLock(3*time.Minute, func() error {
		relDir, _ := a.currentInstall()
		if relDir == "" {
			return errors.New("zapret не установлен — сначала zapret-manager install")
		}
		if err := a.applyStrategy(relDir, name); err != nil {
			return err
		}
		_ = sleepCtx(ctx, 2*time.Second)
		printReport(a.checkNow(ctx))
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

func cmdList(_ context.Context, a *App, _ []string) int {
	relDir, current := a.currentInstall()
	if relDir == "" {
		fmt.Println("zapret не установлен")
		return 1
	}
	all, err := a.listAllStrategies(relDir)
	if err != nil {
		return fail(err)
	}
	st := a.loadState()
	results := map[string]RankEntry{}
	if st.LastSweep != nil && samePath(st.LastSweep.Dir, relDir) {
		for _, e := range st.LastSweep.Ranking {
			results[strings.ToLower(e.Strategy)] = e
		}
		fmt.Printf("Результаты подбора %s:\n\n", st.LastSweep.Time.Format("02.01 15:04"))
	}
	for _, s := range all {
		mark := "  "
		if strings.EqualFold(s.Name, current) {
			mark = "→ "
		}
		line := s.Name
		if e, ok := results[strings.ToLower(s.Name)]; ok {
			line = e.Line()
		}
		if s.Custom {
			line += "  (custom)"
		}
		fmt.Println(mark + line)
	}
	return 0
}

// ---------- update / hosts ----------

func cmdUpdate(ctx context.Context, a *App, args []string) int {
	fs := newFlags("update")
	force := fs.Bool("force", false, "переставить релиз, даже если версия та же")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := a.withLock(3*time.Minute, func() error {
		a.updateAll(ctx, true, *force)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

func cmdHosts(ctx context.Context, a *App, _ []string) int {
	changed, err := a.updateHosts(ctx)
	if err != nil {
		return fail(err)
	}
	a.updateState(func(st *State) { st.LastHostsUpdate = time.Now() })
	if changed {
		fmt.Println("hosts обновлён, кэш DNS сброшен. Старая копия в", a.path("backups"))
		a.Log.Eventf("hosts обновлён вручную")
	} else {
		fmt.Println("hosts уже актуален.")
	}
	return 0
}

// ---------- off / on ----------

func cmdOff(_ context.Context, a *App, args []string) int {
	until := time.Date(9999, 1, 1, 0, 0, 0, 0, time.Local)
	if len(args) > 0 {
		m, err := strconv.Atoi(args[0])
		if err != nil || m <= 0 {
			fmt.Println("off [МИНУТЫ] — например: off 30")
			return 2
		}
		until = time.Now().Add(time.Duration(m) * time.Minute)
	}
	a.updateState(func(st *State) { st.PausedUntil = until })
	if err := stopService(zapretSvcName, 15*time.Second); err != nil {
		return fail(err)
	}
	killProcesses("winws.exe")
	if until.Year() > 3000 {
		_ = setServiceStartType(zapretSvcName, mgr.StartManual)
		fmt.Println("zapret выключен до команды: zapret-manager on")
		a.Log.Eventf("zapret выключен вручную")
	} else {
		fmt.Printf("zapret выключен до %s, потом сторож включит его сам.\n", until.Format("15:04"))
		a.Log.Eventf("zapret выключен вручную до %s", until.Format("02.01 15:04"))
	}
	return 0
}

func cmdOn(ctx context.Context, a *App, _ []string) int {
	a.updateState(func(st *State) { st.PausedUntil = time.Time{} })
	_ = setServiceStartType(zapretSvcName, mgr.StartAutomatic)
	if err := startService(zapretSvcName, 15*time.Second); err != nil {
		return fail(err)
	}
	a.Log.Eventf("zapret включён вручную")
	fmt.Println("zapret включён.")
	_ = sleepCtx(ctx, 2*time.Second)
	printReport(a.checkNow(ctx))
	return 0
}

// ---------- doctor / log / uninstall ----------

func cmdDoctor(_ context.Context, a *App, _ []string) int {
	fmt.Println("Диагностика:")
	return a.runDoctor()
}

func cmdLog(_ context.Context, a *App, args []string) int {
	n := 40
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			n = v
		}
	}
	f, err := os.Open(a.path("logs", "manager.log"))
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(data), "\r", ""), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return 0
}

func cmdUninstall(_ context.Context, a *App, args []string) int {
	fs := newFlags("uninstall")
	all := fs.Bool("all", false, "убрать и сам zapret (службы zapret и WinDivert) и блок в hosts")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := stopManagerIfRunning(); err != nil {
		fmt.Println("!", err)
	}
	if err := deleteService(managerSvcName); err != nil {
		fmt.Println("! служба-сторож:", err)
	} else {
		fmt.Println("✓ служба-сторож удалена")
	}
	if *all {
		_ = stopService(zapretSvcName, 15*time.Second)
		killProcesses("winws.exe")
		if err := deleteService(zapretSvcName); err != nil {
			fmt.Println("! служба zapret:", err)
		} else {
			fmt.Println("✓ служба zapret удалена")
		}
		removeWinDivert()
		fmt.Println("✓ WinDivert остановлен")
		if data, err := os.ReadFile(hostsPath()); err == nil {
			if out, changed := removeHostsBlock(string(data)); changed {
				if err := a.writeHosts(data, out); err != nil {
					fmt.Println("! hosts:", err)
				} else {
					fmt.Println("✓ блок zapret-manager убран из hosts")
				}
			}
		}
	} else {
		fmt.Println("Служба zapret оставлена как есть — работает без сторожа.")
	}
	fmt.Printf("Файлы в %s не удалял.\n", a.Root)
	return 0
}

// ---------- мелочи ----------

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fmtDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("02.01 15:04")
}
