package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"

	"zapret-manager/internal/sitelist"
	"zapret-manager/internal/zapretfmt"
)

// Что разблокирует zapret. Всё хранится в файлах релиза, которые читает и service.bat,
// поэтому выбор переживает обновление (migrateUserFiles) и виден в его меню:
//
//	lists\list-general-user.txt — свои домены и блок сервисов из каталога (internal/sitelist);
//	lists\list-exclude-user.txt — домены, которые zapret не трогает;
//	lists\ipset-all.txt         — режим охвата: loaded (по спискам), any (все сайты), none;
//	utils\game_filter.enabled   — игры.
//
// zapret помогает только против DPI: подмены SNI, замедления, «заморозки» на 16-20 КБ.
// Он не поможет, если сервис сам не пускает из России (ChatGPT, Spotify…), если адрес
// закрыт по IP целиком или провайдер подменяет DNS. Поэтому всё, что сюда добавлено,
// проверяется настоящими запросами, а то, что не открыла ни одна стратегия, сторож
// не чинит (unfixableGroups).

// siteService — сервис из каталога: Domains попадают в list-general-user.txt,
// Checks — в проверки сторожа (Group = ID).
type siteService struct {
	ID      string
	Name    string
	Domains []string // пусто — сервис покрывает ipset, а не список доменов
	Checks  []Target
	Note    string
}

// siteCatalog — сервисы, которые в России режут DPI. Адреса проверки — с публичными
// сертификатами (chat.signal.org подписан своим CA Signal и всегда выглядел бы как
// подмена) и такие, что открываются без входа в аккаунт.
var siteCatalog = []siteService{
	{
		ID: "hosting", Name: "Cloudflare и зарубежные хостинги",
		Note: "сайты на Cloudflare, Hetzner, DigitalOcean и т.п. режут по подсетям — домены не нужны, работает ipset (режим «по спискам» или «все сайты»)",
		Checks: []Target{
			{"Cloudflare", "https://www.cloudflare.com/", "hosting"},
			{"Hetzner", "https://www.hetzner.com/", "hosting"},
			{"DigitalOcean", "https://www.digitalocean.com/", "hosting"},
		},
	},
	{
		ID: "instagram", Name: "Instagram",
		Domains: []string{"instagram.com", "cdninstagram.com", "ig.me", "instagr.am", "fbcdn.net"},
		Checks: []Target{
			{"Instagram", "https://www.instagram.com/", "instagram"},
			{"Instagram CDN", "https://static.cdninstagram.com/", "instagram"},
		},
	},
	{
		ID: "facebook", Name: "Facebook и Messenger",
		Domains: []string{"facebook.com", "fb.com", "fbcdn.net", "fbsbx.com", "facebook.net", "messenger.com", "m.me"},
		Checks: []Target{
			{"Facebook", "https://www.facebook.com/", "facebook"},
			{"Facebook CDN", "https://static.xx.fbcdn.net/", "facebook"},
		},
	},
	{
		ID: "x", Name: "X (Twitter)",
		Domains: []string{"x.com", "twitter.com", "twimg.com", "t.co"},
		Checks: []Target{
			{"X", "https://x.com/", "x"},
			{"X картинки", "https://abs.twimg.com/", "x"},
		},
	},
	{
		ID: "linkedin", Name: "LinkedIn",
		Domains: []string{"linkedin.com", "licdn.com"},
		Checks: []Target{
			{"LinkedIn", "https://www.linkedin.com/", "linkedin"},
			{"LinkedIn CDN", "https://static.licdn.com/", "linkedin"},
		},
	},
	{
		ID: "rutracker", Name: "RuTracker",
		Domains: []string{"rutracker.org", "rutracker.cc"},
		Checks:  []Target{{"RuTracker", "https://rutracker.org/forum/index.php", "rutracker"}},
	},
	{
		ID: "roblox", Name: "Roblox",
		Domains: []string{"roblox.com", "rbxcdn.com"},
		Note:    "сайт и загрузка игр; для самого игрового трафика включите игры: sites games all",
		Checks: []Target{
			{"Roblox", "https://www.roblox.com/", "roblox"},
			{"Roblox CDN", "https://css.rbxcdn.com/", "roblox"},
		},
	},
	{
		ID: "signal", Name: "Signal",
		Domains: []string{"signal.org", "whispersystems.org", "signal.me", "signal.group", "signal.art"},
		Checks: []Target{
			{"Signal", "https://signal.org/", "signal"},
			{"Signal обновления", "https://updates.signal.org/", "signal"},
		},
	},
}

func catalogService(id string) (siteService, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, s := range siteCatalog {
		if s.ID == id {
			return s, true
		}
	}
	return siteService{}, false
}

func catalogIDs() string {
	ids := make([]string, len(siteCatalog))
	for i, s := range siteCatalog {
		ids[i] = s.ID
	}
	return strings.Join(ids, ", ")
}

// groupTitle — как группу проверки показать человеку.
func groupTitle(name string) string {
	switch name {
	case "discord":
		return "Discord"
	case "youtube":
		return "YouTube"
	case "control":
		return "контроль"
	}
	if s, ok := catalogService(name); ok {
		return s.Name
	}
	return name
}

const (
	// maxUserDomainChecks — сколько своих доменов проверять: каждый — запрос в каждой
	// проверке и на каждой стратегии при подборе.
	maxUserDomainChecks = 10
	// maxCheckTargets — всего адресов в одной проверке (по соединению на адрес).
	maxCheckTargets = 64
)

// targetsFor — что проверять: стандартные адреса (или свои из config.json), плюс
// включённые сервисы из каталога и свои сайты из релиза relDir.
func (a *App) targetsFor(relDir string) []Target {
	base := defaultTargets()
	if len(a.Cfg.Targets) > 0 {
		base = a.Cfg.Targets
	}
	out := slices.Clone(base)
	seen := map[string]bool{}
	for _, t := range out {
		seen[t.URL] = true
	}
	for _, t := range siteTargets(relDir) {
		if len(out) >= maxCheckTargets {
			break
		}
		if !seen[t.URL] {
			seen[t.URL] = true
			out = append(out, t)
		}
	}
	return out
}

func siteTargets(relDir string) []Target {
	if relDir == "" {
		return nil
	}
	f, err := readSiteList(userListPath(relDir))
	if err != nil {
		return nil
	}
	var out []Target
	for _, id := range f.Services() {
		if s, ok := catalogService(id); ok {
			out = append(out, s.Checks...)
		}
	}
	n := 0
	for _, line := range f.UserDomains() {
		// в hostlist бывают ^точные и *маски — проверяем только обычные имена
		d, err := sitelist.NormalizeDomain(line)
		if err != nil || !strings.EqualFold(d, line) {
			continue
		}
		if n >= maxUserDomainChecks {
			break
		}
		out = append(out, Target{Name: d, URL: "https://" + d + "/", Group: d})
		n++
	}
	return out
}

// ---------- файлы ----------

func userListPath(relDir string) string {
	return filepath.Join(relDir, "lists", "list-general-user.txt")
}

func excludeListPath(relDir string) string {
	return filepath.Join(relDir, "lists", "list-exclude-user.txt")
}

func readSiteList(p string) (sitelist.File, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return sitelist.Parse(""), nil
	}
	if err != nil {
		return sitelist.File{}, err
	}
	return sitelist.Parse(string(data)), nil
}

// writeSiteList пишет файл, только если содержимое изменилось.
func writeSiteList(p string, f sitelist.File) (bool, error) {
	content := f.Render()
	if old, err := os.ReadFile(p); err == nil && string(old) == content {
		return false, nil
	}
	return true, writeFileAtomic(p, []byte(content))
}

// syncCatalog приводит домены включённых сервисов к каталогу этой версии программы
// (каталог мог пополниться). Неизвестные разделы — например, от более новой версии —
// не трогает. winws перечитывает списки сам, перезапуск не нужен.
func syncCatalog(relDir string) ([]string, error) {
	p := userListPath(relDir)
	if !fileExists(p) {
		return nil, nil
	}
	f, err := readSiteList(p)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, id := range f.Services() {
		if s, ok := catalogService(id); ok && f.SetService(id, s.Domains) {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	_, err = writeSiteList(p, f)
	return names, err
}

// setIpsetMode переключает ipset-all.txt так же, как пункт IPSet Filter в service.bat:
// список подсетей уходит в ipset-all.txt.backup и возвращается оттуда (а если копии
// нет — скачивается заново).
func (a *App) setIpsetMode(ctx context.Context, relDir, mode string) (bool, error) {
	cur := ipsetMode(relDir)
	if cur == mode {
		return false, nil
	}
	p := filepath.Join(relDir, "lists", "ipset-all.txt")
	backup := p + ".backup"
	if mode == zapretfmt.IpsetLoaded {
		if !fileExists(backup) {
			// при режиме не loaded updateIpset кладёт свежий список как раз в .backup
			if _, err := a.updateIpset(ctx, relDir); err != nil {
				return false, fmt.Errorf("сохранённого списка подсетей нет, а скачать не вышло: %w", err)
			}
		}
		return true, os.Rename(backup, p)
	}
	content := []byte(nil) // any — пустой файл
	if mode == zapretfmt.IpsetNone {
		content = []byte(zapretfmt.IpsetNoneMarker + "\r\n")
	}
	if cur == zapretfmt.IpsetLoaded {
		if err := os.Rename(p, backup); err != nil {
			return false, err
		}
	}
	if err := writeFileAtomic(p, content); err != nil {
		if cur == zapretfmt.IpsetLoaded {
			_ = os.Rename(backup, p) // best effort: вернуть список подсетей на место
		}
		return false, err
	}
	return true, nil
}

// restartZapretIfRunning перезапускает winws, чтобы он перечитал списки. Остановленную
// службу (пауза, выключена) не трогаем: изменения подхватятся при запуске.
func restartZapretIfRunning() error {
	if queryService(zapretSvcName).State != svc.Running {
		return nil
	}
	if err := stopService(zapretSvcName, 15*time.Second); err != nil {
		return err
	}
	killProcesses("winws.exe")
	return startService(zapretSvcName)
}

// forgetUnfixable убирает группы из «zapret не помогает»: после смены настроек их надо
// проверить заново. Без аргументов — все.
func (a *App) forgetUnfixable(groups ...string) {
	a.updateState(func(st *State) {
		if st.LastSweep == nil {
			return
		}
		if len(groups) == 0 {
			st.LastSweep.Unfixable = nil
			return
		}
		st.LastSweep.Unfixable = slices.DeleteFunc(st.LastSweep.Unfixable, func(g string) bool { return slices.Contains(groups, g) })
	})
}

// ---------- что сейчас включено ----------

type sitesView struct {
	Mode      string // режим ipset: loaded | any | none
	Subnets   int
	Builtin   int      // доменов в списках Flowseal (list-general, list-google)
	Services  []string // включённые сервисы каталога
	Domains   []string // свои домены
	Excluded  []string
	Games     zapretfmt.GameFilter
	Unfixable []string // по последнему подбору zapret их не открывает
}

func (a *App) readSitesView(relDir string) sitesView {
	v := sitesView{Mode: ipsetMode(relDir), Games: readGameFilter(relDir)}
	if v.Mode == zapretfmt.IpsetLoaded {
		v.Subnets = countIpsetLines(relDir)
	}
	for _, name := range []string{"list-general.txt", "list-google.txt"} {
		if data, err := os.ReadFile(filepath.Join(relDir, "lists", name)); err == nil {
			v.Builtin += zapretfmt.CountEntries(data)
		}
	}
	if f, err := readSiteList(userListPath(relDir)); err == nil {
		v.Services, v.Domains = f.Services(), f.UserDomains()
	}
	if f, err := readSiteList(excludeListPath(relDir)); err == nil {
		v.Excluded = f.UserDomains()
	}
	for g := range a.skippedGroups() {
		v.Unfixable = append(v.Unfixable, g)
	}
	slices.Sort(v.Unfixable)
	return v
}

func modeText(mode string) string {
	switch mode {
	case zapretfmt.IpsetAny:
		return "все сайты — zapret обрабатывает соединения со всеми сайтами, кроме исключений"
	case zapretfmt.IpsetLoaded:
		return "по спискам — домены из списков и подсети зарубежных хостингов"
	case zapretfmt.IpsetNone:
		return "только домены из списков (подсети хостингов выключены)"
	}
	return mode
}

func (v sitesView) serviceEnabled(id string) bool { return slices.Contains(v.Services, id) }

func groupTitles(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = groupTitle(id)
	}
	return strings.Join(out, ", ")
}

// lines — сводка для команды sites и меню.
func (v sitesView) lines() []string {
	mode := modeText(v.Mode)
	if v.Mode == zapretfmt.IpsetLoaded {
		mode += fmt.Sprintf(" (%d подсетей)", v.Subnets)
	}
	out := []string{
		"Режим:        " + mode,
		fmt.Sprintf("Всегда:       Discord, YouTube и остальное из списков Flowseal (доменов: %d)", v.Builtin),
	}
	svcs := "нет"
	if len(v.Services) > 0 {
		svcs = groupTitles(v.Services)
	}
	out = append(out, "Сервисы:      "+svcs)
	own := "нет"
	if len(v.Domains) > 0 {
		own = strings.Join(v.Domains, ", ")
	}
	out = append(out, "Свои сайты:   "+own)
	if len(v.Excluded) > 0 {
		out = append(out, "Исключения:   "+strings.Join(v.Excluded, ", "))
	}
	out = append(out, "Игры:         "+v.Games.String())
	if len(v.Unfixable) > 0 {
		out = append(out, "Не помогает:  "+groupTitles(v.Unfixable)+" — ни одна стратегия не открыла (блокировка по IP, подмена DNS или сервис недоступен сам)")
	}
	return out
}

// ---------- команда sites ----------

const sitesUsage = `Что разблокировать:
  sites                          — что сейчас обрабатывает zapret и каталог сервисов
  sites add СЕРВИС|ДОМЕН …       — включить сервис из каталога (all — все) или добавить свой сайт
  sites remove СЕРВИС|ДОМЕН …    — выключить сервис или убрать свой сайт
  sites mode all|lists|domains   — все сайты / по спискам (как в Flowseal) / только домены
  sites games off|all|tcp|udp    — игры (game filter): порты 1024-65535 для подсетей из ipset
  sites exclude ДОМЕН …          — zapret не трогает этот сайт (если с zapret он сломался)
  sites include ДОМЕН …          — убрать из исключений`

const notHelpedText = `zapret не поможет, если сервис сам не пускает из России (ChatGPT, Gemini, Claude,
Spotify, Netflix…), если адрес закрыт по IP целиком или провайдер подменяет DNS
(тогда — DNS-over-HTTPS). Для этого нужен VPN.`

var ipsetModeWords = map[string]string{
	"all": zapretfmt.IpsetAny, "any": zapretfmt.IpsetAny,
	"lists": zapretfmt.IpsetLoaded, "loaded": zapretfmt.IpsetLoaded,
	"domains": zapretfmt.IpsetNone, "none": zapretfmt.IpsetNone,
}

var gameModeWords = map[string]string{
	"off": zapretfmt.ModeDisabled, "disabled": zapretfmt.ModeDisabled,
	"all": zapretfmt.ModeAll, "on": zapretfmt.ModeAll,
	"tcp": zapretfmt.ModeTCP, "udp": zapretfmt.ModeUDP,
}

// errSitesUsage — аргументов не хватает или они не те: показать справку sitesUsage.
var errSitesUsage = errors.New("неверные аргументы sites")

// sitesPlan — разобранная и проверенная команда; файлы ещё не тронуты.
type sitesPlan struct {
	sub      string // add | remove | exclude | include | mode | games
	services []siteService
	domains  []string
	mode     string // режим ipset для mode, режим game filter для games
}

func planSites(args []string) (sitesPlan, error) {
	p := sitesPlan{sub: strings.ToLower(args[0])}
	rest := args[1:]
	if len(rest) == 0 {
		return p, errSitesUsage
	}
	switch p.sub {
	case "add", "remove":
		for _, arg := range rest {
			if w := strings.ToLower(arg); w == "all" || w == "все" {
				p.services = append(p.services, siteCatalog...)
				continue
			}
			if s, ok := catalogService(arg); ok {
				p.services = append(p.services, s)
				continue
			}
			d, err := sitelist.NormalizeDomain(arg)
			if err != nil {
				return p, fmt.Errorf("%w\nСервисы каталога: %s", err, catalogIDs())
			}
			p.domains = append(p.domains, d)
		}
	case "exclude", "include":
		for _, arg := range rest {
			d, err := sitelist.NormalizeDomain(arg)
			if err != nil {
				return p, err
			}
			p.domains = append(p.domains, d)
		}
	case "mode", "games":
		words := ipsetModeWords
		if p.sub == "games" {
			words = gameModeWords
		}
		m, ok := words[strings.ToLower(rest[0])]
		if len(rest) != 1 || !ok {
			return p, errSitesUsage
		}
		p.mode = m
	default:
		return p, fmt.Errorf("нет подкоманды %q\n\n%s", args[0], sitesUsage)
	}
	return p, nil
}

func cmdSites(ctx context.Context, a *App, args []string) int {
	if len(args) == 0 {
		return a.sitesShow()
	}
	p, err := planSites(args)
	if errors.Is(err, errSitesUsage) {
		fmt.Println(sitesUsage)
		return 2
	}
	if err != nil {
		fmt.Println(err)
		return 2
	}
	// показать может любой, менять файлы в папке релиза — только администратор
	if !isElevated() {
		return relaunchElevated(append(append([]string{"sites"}, args...), "--dir", a.Root))
	}
	var msgs []string
	changed := false
	err = a.withLockCtx(ctx, cliLockTimeout, func() error {
		var err error
		msgs, changed, err = a.applySites(ctx, p)
		return err
	})
	for _, m := range msgs {
		fmt.Println("  " + m)
	}
	if err != nil {
		return fail(err)
	}
	if !changed {
		return 0
	}
	if st := a.loadState(); !st.PausedUntil.IsZero() && time.Now().Before(st.PausedUntil) {
		fmt.Println("\nzapret сейчас выключен — изменения заработают, когда он включится.")
		return 0
	}
	fmt.Println("\nПроверяю…")
	_ = sleepCtx(ctx, 2*time.Second)
	rep := a.checkNow(ctx)
	printReport(rep)
	if !rep.Healthy && !rep.NetworkDown && !rep.DNSProblem {
		fmt.Println("\nНе всё открывается с текущей стратегией. Подобрать другую: zapret-manager select —")
		fmt.Println("или подождите: сторож подберёт сам. Что не откроет ни одна стратегия, он чинить не будет.")
	}
	return 0
}

// applySites — под withLock: находит установку, меняет файлы, перезапускает zapret.
func (a *App) applySites(ctx context.Context, p sitesPlan) ([]string, bool, error) {
	relDir, strategy := a.currentInstall()
	if relDir == "" {
		return nil, false, errors.New("zapret не установлен — сначала zapret-manager install")
	}
	if p.sub == "games" {
		msgs, changed, err := a.setGameFilter(relDir, strategy, p.mode)
		if err != nil || !changed {
			return msgs, false, err
		}
		a.forgetUnfixable()
		a.Log.Eventf("что разблокировать: %s", strings.Join(msgs, "; "))
		return msgs, true, nil // applyStrategy уже перезапустил zapret
	}
	e, err := a.editSiteFiles(ctx, relDir, p)
	if err != nil || !e.changed {
		return e.msgs, false, err
	}
	if e.recheckAll {
		a.forgetUnfixable()
	} else if len(e.recheck) > 0 {
		a.forgetUnfixable(e.recheck...)
	}
	a.Log.Eventf("что разблокировать: %s", strings.Join(e.msgs, "; "))
	if err := restartZapretIfRunning(); err != nil {
		return e.msgs, true, fmt.Errorf("перезапуск zapret: %w", err)
	}
	return e.msgs, true, nil
}

// siteEdit — итог правки файлов релиза командой sites.
type siteEdit struct {
	msgs       []string
	changed    bool
	recheck    []string // группы, которые снова надо чинить, если не откроются
	recheckAll bool     // сменился режим — заново проверить всё
}

// editSiteFiles меняет только файлы в папке релиза relDir (служб не касается).
func (a *App) editSiteFiles(ctx context.Context, relDir string, p sitesPlan) (siteEdit, error) {
	var e siteEdit
	if err := ensureUserLists(relDir); err != nil {
		return e, err
	}
	var err error
	switch p.sub {
	case "add", "remove":
		f, rerr := readSiteList(userListPath(relDir))
		if rerr != nil {
			return e, rerr
		}
		for _, s := range p.services {
			switch {
			case p.sub == "add" && f.SetService(s.ID, s.Domains):
				e.msgs = append(e.msgs, "✓ включён "+s.Name)
				e.recheck = append(e.recheck, s.ID)
			case p.sub == "add":
				e.msgs = append(e.msgs, s.Name+" уже включён")
			case f.RemoveService(s.ID):
				e.msgs = append(e.msgs, "✓ выключен "+s.Name)
			case len(p.services) == 1:
				e.msgs = append(e.msgs, s.Name+" и не был включён")
			}
		}
		for _, d := range p.domains {
			switch {
			case p.sub == "add" && f.AddDomain(d):
				e.msgs = append(e.msgs, "✓ добавлен "+d)
				e.recheck = append(e.recheck, d)
			case p.sub == "add":
				e.msgs = append(e.msgs, d+" уже в списке")
			case f.RemoveDomain(d):
				e.msgs = append(e.msgs, "✓ убран "+d)
			default:
				e.msgs = append(e.msgs, d+" нет среди своих сайтов (списки Flowseal так не меняются)")
			}
		}
		e.changed, err = writeSiteList(userListPath(relDir), f)
	case "exclude", "include":
		f, rerr := readSiteList(excludeListPath(relDir))
		if rerr != nil {
			return e, rerr
		}
		for _, d := range p.domains {
			switch {
			case p.sub == "exclude" && f.AddDomain(d):
				e.msgs = append(e.msgs, "✓ zapret не будет трогать "+d)
			case p.sub == "exclude":
				e.msgs = append(e.msgs, d+" уже в исключениях")
			case f.RemoveDomain(d):
				e.msgs = append(e.msgs, "✓ "+d+" больше не в исключениях")
				e.recheck = append(e.recheck, d)
			default:
				e.msgs = append(e.msgs, d+" нет в исключениях")
			}
		}
		e.changed, err = writeSiteList(excludeListPath(relDir), f)
	case "mode":
		if e.changed, err = a.setIpsetMode(ctx, relDir, p.mode); err != nil {
			return e, err
		}
		e.recheckAll = true
		if e.changed {
			e.msgs = append(e.msgs, "✓ режим: "+modeText(p.mode))
		} else {
			e.msgs = append(e.msgs, "режим и так: "+modeText(p.mode))
		}
	default:
		return e, fmt.Errorf("подкоманда %q не меняет списки", p.sub)
	}
	if err != nil {
		e.changed = false
	}
	return e, err
}

// setGameFilter пишет utils\game_filter.enabled (формат service.bat) и переставляет
// стратегию: порты игр — часть командной строки winws. Не встало — файл возвращается.
func (a *App) setGameFilter(relDir, strategy, mode string) ([]string, bool, error) {
	cur := readGameFilter(relDir)
	if cur.Mode == mode {
		return []string{"игры и так: " + cur.String()}, false, nil
	}
	if st := a.loadState(); !st.PausedUntil.IsZero() && time.Now().Before(st.PausedUntil) {
		return nil, false, errors.New("zapret выключен — включите его (zapret-manager on) и повторите: порты игр меняют командную строку службы")
	}
	next := cur
	next.Mode = mode
	p := filepath.Join(relDir, "utils", "game_filter.enabled")
	old, readErr := os.ReadFile(p)
	if err := writeFileAtomic(p, []byte(next.FileContent())); err != nil {
		return nil, false, err
	}
	if strategy != "" {
		if err := a.applyStrategy(relDir, strategy); err != nil {
			if readErr == nil {
				_ = writeFileAtomic(p, old) // best effort: файл как был; служба уже откатилась сама
			} else {
				_ = os.Remove(p)
			}
			return nil, false, err
		}
	}
	return []string{"✓ игры: " + next.String()}, true, nil
}

func (a *App) sitesShow() int {
	relDir, _ := a.currentInstall()
	if relDir == "" {
		fmt.Println("zapret не установлен. Запустите: zapret-manager install")
		return 0
	}
	v := a.readSitesView(relDir)
	fmt.Printf("Что разблокирует zapret (%s):\n\n", relDir)
	for _, l := range v.lines() {
		fmt.Println("  " + l)
	}
	fmt.Println("\nКаталог (sites add ИМЯ / sites remove ИМЯ, sites add all — все):")
	for _, s := range siteCatalog {
		mark := "[ ]"
		if v.serviceEnabled(s.ID) {
			mark = "[✓]"
		}
		line := fmt.Sprintf("  %s %-10s %s", mark, s.ID, s.Name)
		if s.Note != "" {
			line += " — " + s.Note
		}
		fmt.Println(line)
	}
	fmt.Println()
	fmt.Println(sitesUsage)
	fmt.Println()
	fmt.Println(notHelpedText)
	return 0
}
