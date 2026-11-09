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

	"zapret-manager/internal/hostsfile"
)

// appVersion — версия программы; сборка релиза подставляет номер из тега:
// -ldflags "-X main.appVersion=1.2.3".
var appVersion = "1.2.1"

const (
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
		{"menu", false, "menu — меню по пунктам (открывается и двойным щелчком по exe)", cmdMenu},
		{"install", true, "install [--strategy ИМЯ] [--from ПАПКА] [--select] — поставить в C:\\zapret, перенести текущую установку, включить службу-сторож", cmdInstall},
		{"status", false, "status — что стоит и как работает", cmdStatus},
		{"check", false, "check [-v] — проверить прямо сейчас, открываются ли Discord, YouTube и включённые сайты", cmdCheck},
		// admin=false: показать может любой, а add/remove/mode/… сами просят права (cmdSites)
		{"sites", false, "sites [add|remove|mode|games|exclude|include …] — что разблокировать: сервисы, свои сайты, все сайты, игры", cmdSites},
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
		if !launchedFromExplorer() {
			usage()
			fmt.Println()
			cmdStatus(context.Background(), newApp(dir, true), nil)
			return 0
		}
		rest = []string{"menu"} // двойной клик по exe
	}
	name := strings.ToLower(rest[0])
	if name == "help" || name == "-h" || name == "--help" || name == "/?" {
		usage()
		return 0
	}
	cmd := findCommand(name)
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
	// из меню выходят пунктом «0» — второй раз Enter не спрашиваем
	if (pause || launchedFromExplorer()) && cmd.name != "menu" {
		waitEnter()
	}
	return code
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
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
	say, warn := a.Log.Infof, a.Log.Warnf
	say("Ставлю zapret-manager %s в %s", appVersion, a.Root)

	// Папка целиком закрывается на запись для всех, кроме администраторов, —
	// значит, в ней не должно быть чужих файлов (например, это не рабочий стол).
	dedicated, foreign, err := rootIsDedicated(a.Root)
	if err != nil {
		return fail(err)
	}
	if !dedicated {
		return fail(fmt.Errorf("в %s есть посторонние файлы (%s). zapret-manager закрывает свою папку на запись "+
			"для всех, кроме администраторов, поэтому нужна отдельная папка: install --dir ПАПКА",
			a.Root, strings.Join(foreign, ", ")))
	}
	if err := stopManagerIfRunning(); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(a.Root, 0o755); err != nil {
		return fail(err)
	}
	// Права ставим до копирования exe: всё, что появится внутри, их унаследует.
	if err := secureTree(a.Root, true); err != nil {
		return fail(fmt.Errorf("не закрыть папку %s на запись: %w", a.Root, err))
	}
	say("✓ папка %s: менять файлы может только администратор", a.Root)
	for _, d := range []string{a.releasesDir(), a.customDir(), a.path("logs"), a.path("backups")} {
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
		for range 10 { // exe службы освобождается не сразу после её остановки
			if cerr = copyFile(exe, target); cerr == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if cerr != nil {
			return fail(fmt.Errorf("не скопировать себя в %s: %w", target, cerr))
		}
		say("✓ программа скопирована в %s", target)
	}
	if err := a.saveConfigIfMissing(); err != nil {
		return fail(err)
	}
	a.reloadConfig()

	var previousDir string
	err = a.withLockCtx(ctx, cliLockTimeout, func() error {
		oldDir, oldStrategy := a.currentInstall()
		defer func() { previousDir = oldDir }()
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
			say("✓ нашёл текущую установку: %s (zapret %s, стратегия %s)", oldDir, releaseVersion(oldDir), orDash(oldStrategy))
			say("  game filter: %s, ipset: %s", readGameFilter(oldDir), ipsetMode(oldDir))
		}

		var newDir string
		ver, verr := a.fetchLatestVersion(ctx)
		if verr == nil {
			say("✓ последняя версия Flowseal: %s", ver)
			newDir, verr = a.downloadRelease(ctx, ver, false)
		}
		if verr != nil {
			if oldDir == "" {
				return fmt.Errorf("не скачать zapret: %w", verr)
			}
			warn("не скачать релиз (%v) — переношу текущую папку как есть", verr)
			newDir = filepath.Join(a.releasesDir(), releaseVersion(oldDir))
			if !samePath(oldDir, newDir) {
				if err := copyDir(oldDir, newDir); err != nil {
					return err
				}
			}
		}
		say("✓ zapret %s в %s", releaseVersion(newDir), newDir)
		if oldDir != "" && !samePath(oldDir, newDir) {
			if err := migrateUserFiles(oldDir, newDir); err != nil {
				warn("перенос своих списков и настроек: %v", err)
			} else {
				say("✓ перенёс свои списки, game filter и режим ipset")
			}
		}
		if err := ensureUserLists(newDir); err != nil {
			return err
		}

		if a.Cfg.UpdateIpset {
			if changed, err := a.updateIpset(ctx, newDir); err != nil {
				warn("ipset: %v", err)
			} else {
				a.updateState(func(st *State) { st.LastIpsetUpdate = time.Now() })
				say("✓ ipset %s (режим %s)", map[bool]string{true: "обновлён", false: "актуален"}[changed], ipsetMode(newDir))
			}
		}
		if a.Cfg.UpdateListsFromMain {
			changed, err := a.updateMainLists(ctx, newDir)
			if err != nil {
				warn("списки: %v", err)
			}
			a.updateState(func(st *State) { st.LastListsUpdate = time.Now() })
			if len(changed) > 0 {
				say("✓ списки из main: %s", strings.Join(changed, ", "))
			} else if err == nil {
				say("✓ списки доменов актуальны")
			}
		}
		if a.Cfg.UpdateHosts {
			if changed, err := a.updateHosts(ctx); err != nil {
				warn("hosts: %v", err)
			} else {
				a.updateState(func(st *State) { st.LastHostsUpdate = time.Now() })
				say("✓ hosts %s", map[bool]string{true: "обновлён (старая копия в backups\\)", false: "актуален"}[changed])
			}
		}
		a.updateState(func(st *State) { st.LastUpdateCheck = time.Now() })

		name := *strategyFlag
		if name == "" {
			name = oldStrategy
		}
		if name != "" {
			if s, err := a.findStrategy(newDir, name); err != nil {
				warn("%v — подберу заново", err)
				name = ""
			} else {
				name = s.Name
			}
		}
		runSelect := func() error {
			best, ranking, err := a.selectStrategy(ctx, newDir, name, selectOpts{apply: true})
			printRanking(ranking, 5)
			logRanking(a, ranking, 5)
			if err == nil {
				say("✓ стоит %s", best.Strategy)
				return nil
			}
			if !errors.Is(err, errNothingWorks) {
				return err
			}
			// ничего не открылось целиком — оставляем прежнюю, а если её нет, лучшую из плохих
			fallback := name
			if fallback == "" && best != nil && best.Error == "" {
				fallback = best.Strategy
			}
			if fallback == "" {
				return err
			}
			warn("ни одна стратегия не открыла всё — ставлю %s", fallback)
			return a.applyStrategy(newDir, fallback)
		}
		if *selectFlag || name == "" {
			if err := runSelect(); err != nil {
				return err
			}
		} else {
			if err := a.applyStrategy(newDir, name); err != nil {
				return err
			}
			say("✓ служба zapret со стратегией %s", name)
			_ = sleepCtx(ctx, 2*time.Second)
			rep := a.checkNow(ctx)
			printReport(rep)
			say("проверка: %s", rep.shortText())
			if !rep.Healthy && !rep.NetworkDown {
				say("стратегия %s открывает не всё — подбираю лучшую…", name)
				if err := runSelect(); err != nil {
					return err
				}
			}
		}
		a.pruneReleases(newDir)
		return nil
	})
	if err != nil {
		warn("установка прервана: %v", err)
		return 1
	}

	if !*noWatch {
		if err := installManagerService(target); err != nil {
			warn("служба-сторож: %v", err)
			return 1
		}
		if err := startService(managerSvcName); err != nil {
			warn("служба-сторож: %v", err)
			return 1
		}
		say("✓ служба-сторож zapret-manager запущена")
	}
	a.Log.Eventf("установка завершена")
	fmt.Println()
	cmdStatus(ctx, a, nil)
	relDir, _ := a.currentInstall()
	// не советуем удалять папку, внутри которой лежит сама установка (релиз распакован прямо в C:\zapret)
	if previousDir != "" && !samePath(previousDir, relDir) && !isWithin(previousDir, a.Root) && !isWithin(a.Root, previousDir) {
		fmt.Printf("\nСтарая папка %s больше не используется — её можно удалить.\n", previousDir)
	}
	return 0
}

func logRanking(a *App, r []RankEntry, n int) {
	for i, e := range r {
		if i >= n {
			break
		}
		a.Log.Debugf("  %d. %s", i+1, e.Line())
	}
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
	fmt.Println("\nчто разблокирует (подробно — zapret-manager sites):")
	for _, l := range a.readSitesView(relDir).lines() {
		fmt.Println("  " + l)
	}

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
	for l := range strings.SplitSeq(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		if ts, msg, ok := parseEventLine(l); ok {
			out = append(out, ts.Format("02.01 15:04")+"  "+msg)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// ---------- check / select / use / list ----------

func cmdCheck(ctx context.Context, a *App, args []string) int {
	fs := newFlags("check")
	verbose := fs.Bool("v", false, "показать текст ошибок")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, strategy := a.currentInstall()
	fmt.Printf("Проверяю (стратегия %s)…\n\n", orDash(strategy))
	rep := a.checkNow(ctx)
	printReportV(rep, *verbose)
	if rep.Healthy {
		return 0
	}
	return 1
}

func printReport(rep *HealthReport) { printReportV(rep, false) }

func printReportV(rep *HealthReport, verbose bool) {
	for _, r := range rep.Results {
		mark := "✓"
		info := fmt.Sprintf("HTTP %d, %d КБ, %d мс", r.Status, r.Bytes/1024, r.Elapsed.Milliseconds())
		if !r.OK {
			mark = "✗"
			info = kindText(r)
			if verbose && r.Detail != "" {
				info += " — " + r.Detail
			}
			if rep.Skipped[r.Group] {
				info += " (ни одна стратегия не открыла — сторож это не чинит)"
			}
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
	err := a.withLockCtx(ctx, cliLockTimeout, func() error {
		relDir, current := a.currentInstall()
		if relDir == "" {
			return errors.New("zapret не установлен — сначала zapret-manager install")
		}
		best, ranking, err := a.selectStrategy(ctx, relDir, current, selectOpts{quick: *quick, apply: !*dry})
		printRanking(ranking, 8)
		logRanking(a, ranking, 8)
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
	err := a.withLockCtx(ctx, cliLockTimeout, func() error {
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
	rows, sweep, err := a.strategyRows()
	if err != nil {
		return fail(err)
	}
	if !sweep.IsZero() {
		fmt.Printf("Результаты подбора %s:\n\n", sweep.Format("02.01 15:04"))
	}
	for _, r := range rows {
		mark := "  "
		if r.Current {
			mark = "→ "
		}
		fmt.Println(mark + r.Line)
	}
	return 0
}

// strategyRow — стратегия для списка: имя и результат последнего подбора, если был.
type strategyRow struct {
	Name    string
	Line    string // имя или строка рейтинга; у своих стратегий — пометка (custom)
	Current bool
}

// strategyRows — стратегии установленного релиза; sweep — время подбора, чьи
// результаты подставлены (нулевое, если подбора по этому релизу не было).
func (a *App) strategyRows() (rows []strategyRow, sweep time.Time, err error) {
	relDir, current := a.currentInstall()
	if relDir == "" {
		return nil, time.Time{}, errors.New("zapret не установлен")
	}
	all, err := a.listAllStrategies(relDir)
	if err != nil {
		return nil, time.Time{}, err
	}
	st := a.loadState()
	results := map[string]RankEntry{}
	if st.LastSweep != nil && samePath(st.LastSweep.Dir, relDir) {
		for _, e := range st.LastSweep.Ranking {
			results[strings.ToLower(e.Strategy)] = e
		}
		sweep = st.LastSweep.Time
	}
	for _, s := range all {
		line := s.Name
		if e, ok := results[strings.ToLower(s.Name)]; ok {
			line = e.Line()
		}
		if s.Custom {
			line += "  (custom)"
		}
		rows = append(rows, strategyRow{Name: s.Name, Line: line, Current: strings.EqualFold(s.Name, current)})
	}
	return rows, sweep, nil
}

// ---------- update / hosts ----------

func cmdUpdate(ctx context.Context, a *App, args []string) int {
	fs := newFlags("update")
	force := fs.Bool("force", false, "переставить релиз, даже если версия та же")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	err := a.withLockCtx(ctx, cliLockTimeout, func() error {
		a.updateAll(ctx, true, *force)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

func cmdHosts(ctx context.Context, a *App, _ []string) int {
	var changed bool
	err := a.withLockCtx(ctx, cliLockTimeout, func() error {
		var err error
		changed, err = a.updateHosts(ctx)
		return err
	})
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

// maxOffMinutes — неделя; дольше — это уже «off» без срока.
const maxOffMinutes = 7 * 24 * 60

// parseOffMinutes разбирает срок паузы для off. Верхняя граница заодно не даёт
// time.Duration переполниться (огромное число дало бы срок в прошлом).
func parseOffMinutes(s string) (time.Duration, error) {
	m, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || m <= 0 || m > maxOffMinutes {
		return 0, fmt.Errorf("нужно число минут от 1 до %d, например 30 (без числа — до включения вручную)", maxOffMinutes)
	}
	return time.Duration(m) * time.Minute, nil
}

func cmdOff(ctx context.Context, a *App, args []string) int {
	until := time.Date(9999, 1, 1, 0, 0, 0, 0, time.Local)
	if len(args) > 0 {
		d, err := parseOffMinutes(args[0])
		if err != nil {
			fmt.Println("off [МИНУТЫ]:", err)
			return 2
		}
		until = time.Now().Add(d)
	}
	err := a.withLockCtx(ctx, cliLockTimeout, func() error {
		// паузу ставим до остановки: иначе сторож может успеть поднять службу обратно
		a.updateState(func(st *State) { st.PausedUntil = until })
		if err := stopService(zapretSvcName, 15*time.Second); err != nil {
			a.updateState(func(st *State) { st.PausedUntil = time.Time{} })
			return err
		}
		killProcesses("winws.exe")
		// ручной запуск — чтобы после перезагрузки посреди паузы zapret сам не поднялся
		return setServiceStartType(zapretSvcName, mgr.StartManual)
	})
	if err != nil {
		return fail(err)
	}
	if until.Year() > 3000 {
		fmt.Println("zapret выключен до команды: zapret-manager on")
		a.Log.Eventf("zapret выключен вручную")
	} else {
		fmt.Printf("zapret выключен до %s, потом сторож включит его сам.\n", until.Format("15:04"))
		a.Log.Eventf("zapret выключен вручную до %s", until.Format("02.01 15:04"))
	}
	return 0
}

func cmdOn(ctx context.Context, a *App, _ []string) int {
	err := a.withLockCtx(ctx, cliLockTimeout, func() error {
		a.updateState(func(st *State) { st.PausedUntil = time.Time{} })
		_ = setServiceStartType(zapretSvcName, mgr.StartAutomatic)
		return startService(zapretSvcName)
	})
	if err != nil {
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
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("Журнал пока пуст:", a.path("logs", "manager.log"))
		return 0
	}
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
			if out, changed := hostsfile.RemoveBlock(string(data)); changed {
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
