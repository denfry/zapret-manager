package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"zapret-manager/internal/batch"
	"zapret-manager/internal/textutil"
	"zapret-manager/internal/zapretfmt"
)

// Меню — то, что видит человек, дважды щёлкнувший по exe. Каждый пункт вызывает
// команду из таблицы commands с теми же аргументами, что и в командной строке:
// меню только спрашивает параметры и показывает состояние, собственной логики
// изменения системы у него нет.

// menuView — снимок состояния для шапки меню и выбора пунктов.
type menuView struct {
	Root          string
	Elevated      bool
	ReleaseDir    string // "" — zapret не установлен
	Version       string
	LatestVersion string
	Strategy      string
	ZapretExists  bool
	ZapretState   svc.State
	ManagerExists bool
	ManagerState  svc.State
	ManagerExe    string // exe службы-сторожа
	SelfExe       string // exe, из которого открыто меню
	PausedUntil   time.Time
	LastCheck     *CheckSummary
	Sites         sitesView // что разблокирует zapret (только если установлен)
	Now           time.Time
}

func (v menuView) installed() bool { return v.ReleaseDir != "" }
func (v menuView) paused() bool    { return !v.PausedUntil.IsZero() && v.Now.Before(v.PausedUntil) }
func (v menuView) zapretRunning() bool {
	return v.ZapretExists && v.ZapretState == svc.Running
}

// foreignExe — меню открыто не из установленного exe (например, скачали новую версию).
func (v menuView) foreignExe() bool {
	return v.ManagerExists && v.ManagerExe != "" && v.SelfExe != "" && !samePath(v.ManagerExe, v.SelfExe)
}

func (a *App) menuSnapshot() menuView {
	relDir, strategy := a.currentInstall()
	st := a.loadState()
	zi, mi := queryService(zapretSvcName), queryService(managerSvcName)
	v := menuView{
		Root: a.Root, Elevated: isElevated(),
		ReleaseDir: relDir, Strategy: strategy, LatestVersion: st.LatestVersion,
		ZapretExists: zi.Exists, ZapretState: zi.State,
		ManagerExists: mi.Exists, ManagerState: mi.State,
		PausedUntil: st.PausedUntil, LastCheck: st.LastCheck, Now: time.Now(),
	}
	if relDir != "" {
		v.Version = releaseVersion(relDir)
		v.Sites = a.readSitesView(relDir)
	}
	if mi.Exists {
		v.ManagerExe = batch.ExeFromCmdline(mi.BinPath)
	}
	if exe, err := os.Executable(); err == nil {
		v.SelfExe = exe
	}
	return v
}

// ---------- шапка ----------

type mood int

const (
	moodNone mood = iota
	moodOK
	moodWarn
	moodBad
)

type statusLine struct {
	label string
	text  string
	mood  mood
}

func statusLines(v menuView) []statusLine {
	var out []statusLine
	switch {
	case !v.installed():
		out = append(out, statusLine{"zapret", "не установлен — выберите «Установить»", moodBad})
	default:
		ver := v.Version
		if v.LatestVersion != "" && v.LatestVersion != v.Version {
			ver += " (вышла " + v.LatestVersion + ")"
		}
		about := "стратегия " + orDash(v.Strategy) + ", версия " + ver
		switch {
		case v.paused() && v.PausedUntil.Year() > 3000:
			out = append(out, statusLine{"zapret", "выключен, пока не включите — " + about, moodWarn})
		case v.paused():
			out = append(out, statusLine{"zapret", "выключен до " + v.PausedUntil.Format("15:04") + " — " + about, moodWarn})
		case v.zapretRunning():
			out = append(out, statusLine{"zapret", "работает — " + about, moodOK})
		case !v.ZapretExists:
			out = append(out, statusLine{"zapret", "служба не создана — " + about, moodBad})
		default:
			out = append(out, statusLine{"zapret", "служба " + stateName(v.ZapretState) + " — " + about, moodBad})
		}
	}
	switch {
	case !v.ManagerExists:
		out = append(out, statusLine{"автопочинка", "не установлена (служба-сторож)", moodWarn})
	case v.ManagerState == svc.Running:
		out = append(out, statusLine{"автопочинка", "включена — сторож сам проверяет связь и чинит", moodOK})
	default:
		out = append(out, statusLine{"автопочинка", "служба-сторож " + stateName(v.ManagerState), moodBad})
	}
	if v.installed() {
		out = append(out, coverageLine(v.Sites))
	}
	if c := v.LastCheck; c != nil {
		text := c.Time.Format("02.01 15:04") + " — Discord " + c.Discord + ", YouTube " + c.YouTube
		if ok, total := otherTotals(c); total > 0 {
			text += fmt.Sprintf(", остальное %d/%d", ok, total)
		}
		m := moodOK
		switch {
		case c.NetworkDown:
			text, m = text+" — не было интернета", moodWarn
		case c.DNSProblem:
			text, m = text+" — похоже на подмену DNS", moodBad
		case !c.Healthy:
			text, m = text+" — есть проблемы", moodBad
		default:
			text += " — всё открывается"
		}
		out = append(out, statusLine{"проверка", text, m})
	} else if v.installed() {
		out = append(out, statusLine{"проверка", "ещё не было", moodNone})
	}
	return out
}

// otherTotals — сколько адресов сервисов и своих сайтов открылось (без пропущенных
// групп: их сторож не чинит, и в шапке они только пугали бы).
func otherTotals(c *CheckSummary) (ok, total int) {
	for name, g := range c.Groups {
		if slices.Contains(c.Skipped, name) {
			continue
		}
		ok += g.OK
		total += g.Total
	}
	return ok, total
}

// coverageLine — коротко, что сейчас разблокирует zapret.
func coverageLine(s sitesView) statusLine {
	parts := []string{map[string]string{
		zapretfmt.IpsetAny:    "все сайты",
		zapretfmt.IpsetLoaded: "по спискам",
		zapretfmt.IpsetNone:   "только домены из списков",
	}[s.Mode]}
	if parts[0] == "" {
		parts[0] = s.Mode
	}
	if len(s.Services) > 0 {
		parts = append(parts, "сервисы: "+groupTitles(s.Services))
	}
	if len(s.Domains) > 0 {
		parts = append(parts, fmt.Sprintf("своих сайтов: %d", len(s.Domains)))
	}
	if s.Games.Mode != zapretfmt.ModeDisabled {
		parts = append(parts, "игры: "+s.Games.String())
	}
	l := statusLine{"охват", strings.Join(parts, "; "), moodNone}
	if len(s.Unfixable) > 0 {
		l.text += "; не помогает: " + groupTitles(s.Unfixable)
		l.mood = moodWarn
	}
	return l
}

// ---------- пункты ----------

type menuItem struct {
	title string
	// run спрашивает параметры и выполняет действие; false — человек передумал
	// (тогда не нужно ждать Enter, сразу снова меню).
	run func(m *menu) bool
}

func buildMenu(v menuView) []menuItem {
	var items []menuItem
	if !v.ManagerExists {
		title := "Установить — скачать zapret, подобрать стратегию, включить автопочинку"
		if v.installed() {
			title = "Установить zapret-manager — подхватит ваш zapret и включит автопочинку"
		}
		items = append(items, menuItem{title, actInstall})
	} else if v.foreignExe() {
		items = append(items, menuItem{"Заменить установленный zapret-manager этим файлом (версия " + appVersion + ")", actReinstall})
	}
	if v.installed() {
		items = append(items,
			menuItem{"Проверить, что открывается", actCommand("check")},
			menuItem{"Что разблокировать: сервисы, свои сайты, все сайты, игры", actSites},
			menuItem{"Подобрать лучшую стратегию", actSelect},
			menuItem{"Выбрать стратегию вручную", actUse},
		)
		if v.paused() || !v.zapretRunning() {
			items = append(items, menuItem{"Включить zapret", actCommand("on")})
		} else {
			items = append(items, menuItem{"Выключить zapret (на время или до включения)", actOff})
		}
		items = append(items,
			menuItem{"Обновить zapret, списки и hosts", actCommand("update")},
			menuItem{"Подробный статус", actCommand("status")},
		)
	}
	items = append(items,
		menuItem{"Журнал", actCommand("log")},
		menuItem{"Диагностика (конфликты с VPN, антивирусами и т.п.)", actCommand("doctor")},
	)
	if v.installed() || v.ManagerExists {
		items = append(items, menuItem{"Удалить…", actUninstall})
	}
	return items
}

// actCommand — пункт без вопросов: сразу команда без аргументов.
func actCommand(name string) func(*menu) bool {
	return func(m *menu) bool {
		m.exec(name, nil)
		return true
	}
}

func actInstall(m *menu) bool {
	m.say("Установка скачает свежий zapret-discord-youtube в %s, перенесёт настройки", m.root)
	m.say("уже стоящего zapret (если он есть), подберёт стратегию и включит службу-сторож.")
	m.say("Подбор занимает несколько минут: пока он идёт, Discord и YouTube будут то")
	m.say("открываться, то нет.")
	if !m.confirm() {
		return false
	}
	m.install()
	return true
}

func actReinstall(m *menu) bool {
	m.say("Служба-сторож остановится, установленная программа заменится этим файлом")
	m.say("(версия %s). Настройки, стратегия и zapret останутся как есть.", appVersion)
	if !m.confirm() {
		return false
	}
	m.install()
	return true
}

func (m *menu) install() {
	if m.exec("install", nil) != 0 || !m.view.Elevated {
		return // без прав install открылся в отдельном окне — итог там
	}
	target := filepath.Join(m.root, "zapret-manager.exe")
	if m.view.SelfExe != "" && !samePath(m.view.SelfExe, target) {
		m.say("\nПрограмма теперь лежит в %s — дальше открывайте её оттуда.", target)
		m.say("Скачанный файл можно удалить.")
	}
}

func actSelect(m *menu) bool {
	m.say("Программа по очереди попробует все стратегии и поставит ту, при которой")
	m.say("лучше всего открываются Discord, YouTube и включённые сайты. Это займёт")
	m.say("несколько минут; пока идёт подбор, связь с ними будет пропадать. Прервать — Ctrl+C.")
	if !m.confirm() {
		return false
	}
	m.exec("select", nil)
	return true
}

func actUse(m *menu) bool {
	rows, sweep, err := m.strategies()
	if err != nil {
		m.say("Ошибка: %v", err)
		return true
	}
	if len(rows) == 0 {
		m.say("Стратегий не нашлось.")
		return true
	}
	if !sweep.IsZero() {
		m.say("Стратегии и результаты подбора %s (→ — текущая):\n", sweep.Format("02.01 15:04"))
	} else {
		m.say("Стратегии (→ — текущая; результаты появятся после подбора):\n")
	}
	for i, r := range rows {
		mark := "  "
		if r.Current {
			mark = "→ "
		}
		m.say("%s%3d. %s", mark, i+1, r.Line)
	}
	answer, ok := m.ask("\nНомер стратегии (Enter — назад): ")
	if !ok || answer == "" {
		return false
	}
	n, err := strconv.Atoi(answer)
	if err != nil || n < 1 || n > len(rows) {
		m.say("Нет стратегии с номером «%s».", answer)
		return true
	}
	m.exec("use", []string{rows[n-1].Name})
	return true
}

func actOff(m *menu) bool {
	answer, ok := m.ask("На сколько минут выключить? Enter — пока не включите сами, 0 — отмена: ")
	switch {
	case !ok || answer == "0":
		return false
	case answer == "":
		m.exec("off", nil)
	default:
		if _, err := parseOffMinutes(answer); err != nil {
			m.say("%v", err)
			return true
		}
		m.exec("off", []string{answer})
	}
	return true
}

func actUninstall(m *menu) bool {
	m.say("  1. Убрать только службу-сторож (zapret продолжит работать как есть)")
	m.say("  2. Убрать всё: службу-сторож, службу zapret, WinDivert и блок в hosts")
	m.say("  0. Назад")
	answer, ok := m.ask("\nВыберите пункт: ")
	if !ok {
		return false
	}
	var args []string
	switch answer {
	case "1":
	case "2":
		args = []string{"--all"}
	default:
		return false
	}
	if !m.confirm() {
		return false
	}
	m.exec("uninstall", args)
	return true
}

// ---------- что разблокировать ----------

func actSites(m *menu) bool {
	s := m.view.Sites
	m.say("Что разблокирует zapret сейчас:\n")
	for _, l := range s.lines() {
		m.say("  " + l)
	}
	m.say("")
	m.say("  1. Сервисы: включить или выключить (Instagram, X, Roblox…)")
	m.say("  2. Добавить свой сайт")
	m.say("  3. Убрать свой сайт")
	m.say("  4. Режим: все сайты / по спискам / только домены")
	m.say("  5. Игры (game filter)")
	m.say("  6. Исключения: сайт сломался из-за zapret")
	m.say("  7. Включить всё: все сервисы каталога и режим «все сайты»")
	m.say("  0. Назад")
	answer, ok := m.ask("\nВыберите пункт: ")
	if !ok {
		return false
	}
	switch answer {
	case "1":
		return actSitesServices(m)
	case "2":
		d, ok := m.ask("Адрес сайта, например example.com (Enter — назад): ")
		if !ok || d == "" {
			return false
		}
		m.exec("sites", []string{"add", d})
	case "3":
		d, ok := m.pick("Свои сайты", s.Domains)
		if !ok {
			return false
		}
		m.exec("sites", []string{"remove", d})
	case "4":
		return actSitesMode(m)
	case "5":
		return actSitesGames(m)
	case "6":
		return actSitesExclude(m)
	case "7":
		m.say("Включатся все сервисы каталога, и zapret будет обрабатывать соединения со")
		m.say("всеми сайтами, кроме исключений (ya.ru, vk.com, банки и т.п. из списков Flowseal).")
		m.say("Если какой-то сайт перестанет открываться — добавьте его в исключения (пункт 6).")
		m.say("Сторож проверит всё и подберёт стратегию; что zapret открыть не может, он")
		m.say("покажет как «не помогает» и чинить не будет.")
		if !m.confirm() {
			return false
		}
		if m.exec("sites", []string{"add", "all"}) == 0 {
			m.exec("sites", []string{"mode", "all"})
		}
	default:
		return false
	}
	return true
}

func actSitesServices(m *menu) bool {
	s := m.view.Sites
	m.say("Сервисы ([✓] — включён):\n")
	for i, svc := range siteCatalog {
		mark := "[ ]"
		if s.serviceEnabled(svc.ID) {
			mark = "[✓]"
		}
		line := fmt.Sprintf("  %d. %s %s", i+1, mark, svc.Name)
		if svc.Note != "" {
			line += " — " + svc.Note
		}
		m.say(line)
	}
	answer, ok := m.ask("\nНомера сервисов через пробел — включить выключенные и выключить включённые (Enter — назад): ")
	if !ok || answer == "" {
		return false
	}
	var add, remove []string
	for f := range strings.FieldsFuncSeq(answer, func(r rune) bool { return r == ' ' || r == ',' }) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > len(siteCatalog) {
			m.say("Нет сервиса с номером «%s».", f)
			return true
		}
		id := siteCatalog[n-1].ID
		if s.serviceEnabled(id) {
			remove = appendUnique(remove, id)
		} else {
			add = appendUnique(add, id)
		}
	}
	if len(add) > 0 {
		m.exec("sites", append([]string{"add"}, add...))
	}
	if len(remove) > 0 {
		m.exec("sites", append([]string{"remove"}, remove...))
	}
	return true
}

func actSitesMode(m *menu) bool {
	options := []struct{ word, mode, title string }{
		{"all", zapretfmt.IpsetAny, "Все сайты — zapret обрабатывает всё, кроме исключений. Больше всего открывается, но изредка ломает отдельные сайты"},
		{"lists", zapretfmt.IpsetLoaded, "По спискам — домены из списков и подсети зарубежных хостингов (как в Flowseal по умолчанию)"},
		{"domains", zapretfmt.IpsetNone, "Только домены из списков — если подсети хостингов что-то ломают"},
	}
	for i, o := range options {
		mark := "  "
		if o.mode == m.view.Sites.Mode {
			mark = "→ "
		}
		m.say("%s%d. %s", mark, i+1, o.title)
	}
	answer, ok := m.ask("\nВыберите режим (Enter — назад): ")
	n, err := strconv.Atoi(answer)
	if !ok || err != nil || n < 1 || n > len(options) {
		return false
	}
	m.exec("sites", []string{"mode", options[n-1].word})
	return true
}

func actSitesGames(m *menu) bool {
	m.say("Игры: zapret обрабатывает порты 1024-65535 для подсетей из ipset (в режиме «все сайты» —")
	m.say("для всех адресов). Включайте, если игра не подключается; если что-то стало хуже — выключите.\n")
	options := []struct{ word, mode, title string }{
		{"off", zapretfmt.ModeDisabled, "Выключить"},
		{"all", zapretfmt.ModeAll, "TCP и UDP"},
		{"tcp", zapretfmt.ModeTCP, "Только TCP"},
		{"udp", zapretfmt.ModeUDP, "Только UDP"},
	}
	for i, o := range options {
		mark := "  "
		if o.mode == m.view.Sites.Games.Mode {
			mark = "→ "
		}
		m.say("%s%d. %s", mark, i+1, o.title)
	}
	answer, ok := m.ask("\nВыберите (Enter — назад): ")
	n, err := strconv.Atoi(answer)
	if !ok || err != nil || n < 1 || n > len(options) {
		return false
	}
	m.exec("sites", []string{"games", options[n-1].word})
	return true
}

func actSitesExclude(m *menu) bool {
	ex := m.view.Sites.Excluded
	if len(ex) > 0 {
		m.say("Сейчас в исключениях: %s\n", strings.Join(ex, ", "))
	}
	m.say("  1. Добавить сайт в исключения (zapret его не трогает)")
	m.say("  2. Убрать сайт из исключений")
	m.say("  0. Назад")
	answer, ok := m.ask("\nВыберите пункт: ")
	if !ok {
		return false
	}
	switch answer {
	case "1":
		d, ok := m.ask("Адрес сайта, например bank.example (Enter — назад): ")
		if !ok || d == "" {
			return false
		}
		m.exec("sites", []string{"exclude", d})
	case "2":
		d, ok := m.pick("Исключения", ex)
		if !ok {
			return false
		}
		m.exec("sites", []string{"include", d})
	default:
		return false
	}
	return true
}

// pick показывает нумерованный список и возвращает выбранный элемент.
func (m *menu) pick(title string, items []string) (string, bool) {
	if len(items) == 0 {
		m.say("%s: пусто.", title)
		return "", false
	}
	m.say("%s:\n", title)
	for i, it := range items {
		m.say("  %d. %s", i+1, it)
	}
	answer, ok := m.ask("\nНомер (Enter — назад): ")
	n, err := strconv.Atoi(answer)
	if !ok || err != nil || n < 1 || n > len(items) {
		return "", false
	}
	return items[n-1], true
}

// ---------- цикл ----------

type menu struct {
	in    *bufio.Reader
	out   io.Writer
	root  string // папка установки — уходит в команды как --dir
	color bool   // консоль понимает ANSI-последовательности
	note  string // сообщение над меню на одну перерисовку
	view  menuView

	snapshot   func() menuView
	exec       func(name string, args []string) int
	strategies func() ([]strategyRow, time.Time, error)
}

func newConsoleMenu(a *App) *menu {
	m := &menu{in: bufio.NewReader(os.Stdin), out: os.Stdout, root: a.Root, color: enableVT()}
	m.snapshot = a.menuSnapshot
	m.exec = m.execCommand
	m.strategies = func() ([]strategyRow, time.Time, error) {
		a.reloadConfig() // exclude_strategies могли поменять, пока меню открыто
		return a.strategyRows()
	}
	return m
}

func cmdMenu(_ context.Context, a *App, _ []string) int {
	m := newConsoleMenu(a)
	// Почти все действия меняют систему: при двойном клике просим права один раз на всё
	// меню, а не отдельным окном на каждый пункт.
	if launchedFromExplorer() && !isElevated() {
		err := runElevated([]string{"menu", "--dir", a.Root})
		if err == nil {
			return 0 // меню открылось в окне с правами администратора, это окно закрывается
		}
		if errors.Is(err, windows.ERROR_CANCELLED) {
			m.note = "Права администратора не выданы: пункты, меняющие систему, запросят их отдельно."
		} else {
			m.note = "Не удалось запросить права администратора: " + err.Error()
		}
	}
	return m.loop()
}

func (m *menu) loop() int {
	for {
		m.view = m.snapshot()
		items := buildMenu(m.view)
		m.clearScreen()
		m.render(items)
		answer, ok := m.ask("\nВыберите пункт и нажмите Enter: ")
		if !ok || answer == "0" {
			return 0
		}
		if answer == "" {
			continue
		}
		n, err := strconv.Atoi(answer)
		if err != nil || n < 1 || n > len(items) {
			m.note = fmt.Sprintf("Нет пункта «%s» — введите число от 0 до %d.", answer, len(items))
			continue
		}
		m.say("")
		if items[n-1].run(m) {
			if _, ok := m.ask("\nНажмите Enter, чтобы вернуться в меню…"); !ok {
				return 0
			}
		}
	}
}

func (m *menu) render(items []menuItem) {
	v := m.view
	m.say("%s · папка %s", m.paint("1", "zapret-manager "+appVersion), v.Root)
	m.say("%s", strings.Repeat("─", 64))
	for _, l := range statusLines(v) {
		m.say(" %s %-12s %s", m.dot(l.mood), l.label, l.text)
	}
	if !v.Elevated && m.note == "" {
		m.say("\n %s", m.paint("33", "Без прав администратора: пункты, меняющие систему, откроются в отдельном окне."))
	}
	if m.note != "" {
		m.say("\n %s", m.paint("33", m.note))
		m.note = ""
	}
	m.say("")
	for i, it := range items {
		m.say("  %2d  %s", i+1, it.title)
	}
	m.say("   0  Выход")
}

// execCommand выполняет команду так же, как её выполнила бы командная строка.
func (m *menu) execCommand(name string, args []string) int {
	cmd := findCommand(name)
	if cmd == nil {
		m.say("Внутренняя ошибка: нет команды %s", name)
		return 2
	}
	if cmd.admin && !isElevated() {
		full := append([]string{name}, args...)
		return relaunchElevated(append(full, "--dir", m.root))
	}
	// Ctrl+C прерывает только текущее действие (команды откатываются по ctx), а не меню.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cmd.run(ctx, newApp(m.root, true), args)
}

func (m *menu) say(format string, args ...any) {
	_, _ = fmt.Fprintf(m.out, format+"\n", args...) // вывод в консоль: ошибку записи некуда сообщить
}

// ask показывает вопрос и читает ответ; false — ввод закончился (окно закрывают, EOF).
func (m *menu) ask(prompt string) (string, bool) {
	_, _ = fmt.Fprint(m.out, prompt)
	line, err := m.in.ReadString('\n')
	if err != nil && line == "" {
		m.say("")
		return "", false
	}
	// BOM приходит, если ввод подан через конвейер PowerShell в UTF-8
	return strings.TrimSpace(string(textutil.StripBOM([]byte(line)))), true
}

// confirm: Enter или «д»/«да» — да. Латинское y не принимаем: на русской раскладке
// это клавиша «н», то есть «нет».
func (m *menu) confirm() bool {
	answer, ok := m.ask("\nEnter — продолжить, 0 — отмена: ")
	if !ok {
		return false
	}
	switch strings.ToLower(answer) {
	case "", "д", "да", "1":
		return true
	}
	m.say("Отменено.")
	return false
}

func (m *menu) paint(code, s string) string {
	if !m.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (m *menu) dot(md mood) string {
	switch md {
	case moodOK:
		return m.paint("32", "●")
	case moodWarn:
		return m.paint("33", "●")
	case moodBad:
		return m.paint("31", "●")
	}
	return "·"
}

func (m *menu) clearScreen() {
	if m.color {
		_, _ = fmt.Fprint(m.out, "\x1b]0;zapret-manager\x07\x1b[H\x1b[2J\x1b[3J")
		return
	}
	m.say("")
}

// enableVT включает разбор ANSI-последовательностей в консоли (Windows 10+);
// false — вывод не в консоль или консоль их не понимает.
func enableVT() bool {
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
