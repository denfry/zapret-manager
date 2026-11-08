package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"

	"zapret-manager/internal/batch"
	"zapret-manager/internal/version"
	"zapret-manager/internal/zapretfmt"
)

var (
	errNetworkDown  = errors.New("пропал интернет (контрольные ya.ru и vk.com тоже не открываются)")
	errNothingWorks = errors.New("ни одна стратегия не открыла ни одного сайта из проверки")
)

// currentInstall: какая папка релиза и стратегия стоят сейчас. Источник правды —
// служба zapret (её мог поставить и service.bat руками), запасной — state.json.
func (a *App) currentInstall() (relDir, strategy string) {
	info := queryService(zapretSvcName)
	if info.Exists && info.BinPath != "" {
		exe := batch.ExeFromCmdline(info.BinPath)
		if strings.EqualFold(filepath.Base(exe), "winws.exe") {
			relDir = filepath.Dir(filepath.Dir(exe))
			strategy = readRegistryStrategy()
		}
	}
	if relDir == "" || strategy == "" {
		st := a.loadState()
		if relDir == "" {
			relDir = st.ReleaseDir
		}
		if strategy == "" && samePath(relDir, st.ReleaseDir) {
			strategy = st.Strategy
		}
	}
	if relDir != "" && !validRelease(relDir) {
		return "", ""
	}
	return relDir, strategy
}

// applyStrategy ставит стратегию в службу zapret и запускает её.
func (a *App) applyStrategy(relDir, name string) error {
	s, err := a.findStrategy(relDir, name)
	if err != nil {
		return err
	}
	if err := ensureUserLists(relDir); err != nil {
		return err
	}
	cmdline, err := buildCommandLine(relDir, s, readGameFilter(relDir))
	if err != nil {
		return err
	}
	if err := enableTCPTimestamps(); err != nil {
		a.Log.Warnf("TCP timestamps не включить (стратегии с fooling=ts могут не работать): %v", err)
	}
	oldDir, _ := a.currentInstall()
	prev := queryService(zapretSvcName)
	prevStrategy := readRegistryStrategy()
	if err := stopService(zapretSvcName, 15*time.Second); err != nil {
		return err
	}
	killProcesses("winws.exe")
	if oldDir != "" && !samePath(oldDir, relDir) {
		removeWinDivert()
	}
	if err := installZapretService(cmdline, s.Name); err != nil {
		return err
	}
	err = startService(zapretSvcName)
	if err != nil {
		// драйвер WinDivert мог ещё выгружаться — одна повторная попытка
		time.Sleep(3 * time.Second)
		err = startService(zapretSvcName)
	}
	if err != nil {
		// служба не говорит, что не так, — запустим ту же строку руками и покажем вывод winws
		if p, perr := startWinws(cmdline, filepath.Join(relDir, "bin"), 3*time.Second); perr != nil {
			err = errors.Join(err, perr)
		} else {
			p.Stop()
		}
		// не оставляем службу со сломанной строкой — возвращаем прежнюю
		if prev.Exists && prev.BinPath != "" && prev.BinPath != cmdline {
			if rerr := installZapretService(prev.BinPath, prevStrategy); rerr == nil {
				if serr := startService(zapretSvcName); serr == nil {
					a.Log.Warnf("стратегия %s не запустилась — вернул прежнюю (%s)", s.Name, orDash(prevStrategy))
				}
			}
		}
		return err
	}
	ver := releaseVersion(relDir)
	a.updateState(func(st *State) {
		st.ReleaseDir, st.Version, st.Strategy = relDir, ver, s.Name
	})
	a.Log.Eventf("в службе zapret стратегия %s (zapret %s)", s.Name, ver)
	return nil
}

// ---------- подбор ----------

type RankEntry struct {
	Strategy string `json:"strategy"`
	Score    int    `json:"score"`
	Max      int    `json:"max"`
	Discord  string `json:"discord"`
	YouTube  string `json:"youtube"`
	Other    string `json:"other,omitempty"`
	// Groups — по группам (без control): из них пересчитывается Healthy, когда
	// подбор решает, какие группы zapret не открывает (unfixableGroups).
	Groups    map[string]GroupStat `json:"groups,omitempty"`
	LatencyMs int64                `json:"latency_ms"`
	Healthy   bool                 `json:"healthy"`
	Fails     []string             `json:"fails,omitempty"`
	Error     string               `json:"error,omitempty"`
}

// groupsText — «Discord 6/6, YouTube 4/4, Instagram 2/2» для журнала.
func (e RankEntry) groupsText() string {
	s := "Discord " + e.Discord + ", YouTube " + e.YouTube
	if e.Other != "" {
		s += ", " + e.Other
	}
	return s
}

func (e RankEntry) Line() string {
	if e.Error != "" {
		return fmt.Sprintf("%-32s ошибка: %s", e.Strategy, e.Error)
	}
	mark := "  "
	if e.Healthy {
		mark = "✓ "
	}
	s := fmt.Sprintf("%s%-32s Discord %-5s YouTube %-5s", mark, e.Strategy, e.Discord, e.YouTube)
	if e.Other != "" {
		s += " " + e.Other
	}
	if e.Score > 0 {
		s += fmt.Sprintf(" %4d мс", e.LatencyMs/int64(max(e.Score, 1)))
	}
	return s
}

func (e *RankEntry) fill(rep *HealthReport) {
	e.Score, e.Max = rep.Score, rep.Max
	e.Discord = rep.group("discord").String()
	e.YouTube = rep.group("youtube").String()
	e.Other = rep.otherGroups()
	e.Groups = map[string]GroupStat{}
	for name, g := range rep.Groups {
		if name != "control" {
			e.Groups[name] = *g
		}
	}
	e.LatencyMs = rep.LatencyMs
	e.Healthy = rep.Healthy
	e.Fails = rep.failures()
}

// rehealth пересчитывает Healthy с другим набором пропускаемых групп.
func (e *RankEntry) rehealth(ratio float64, skip map[string]bool) {
	if e.Error != "" || e.Groups == nil {
		return
	}
	e.Healthy = true
	for name, g := range e.Groups {
		if !groupHealthy(name, g, ratio, skip) {
			e.Healthy = false
		}
	}
	e.Other = otherGroupsText(e.Groups, skip)
}

// unfixableGroups — группы (кроме Discord, YouTube и control), которые на полном
// подборе ни одна стратегия не открыла до порога ratio. Значит, их режут не DPI
// (блокировка по IP, подмена DNS) или лежит сам сервис — смена стратегии не поможет,
// и подбирать её снова и снова ради них нельзя: Discord и YouTube будут мигать.
func unfixableGroups(entries []RankEntry, ratio float64) map[string]bool {
	best := map[string]float64{}
	for _, e := range entries {
		if e.Error != "" {
			continue
		}
		for name, g := range e.Groups {
			if coreGroup(name) || g.Total == 0 {
				continue
			}
			best[name] = max(best[name], float64(g.OK)/float64(g.Total))
		}
	}
	out := map[string]bool{}
	for name, r := range best {
		if r < ratio-1e-9 {
			out[name] = true
		}
	}
	return out
}

// unfixableTTL — сколько не чинить группу, которую не открыла ни одна стратегия.
// Потом подбор повторится: блокировки меняются.
const unfixableTTL = 24 * time.Hour

// skippedGroups — группы из последнего полного подбора, моложе unfixableTTL.
// Discord и YouTube сюда не попадают никогда.
func (a *App) skippedGroups() map[string]bool {
	sw := a.loadState().LastSweep
	if sw == nil || time.Since(sw.Time) > unfixableTTL {
		return nil
	}
	skip := map[string]bool{}
	for _, name := range sw.Unfixable {
		if !coreGroup(name) {
			skip[name] = true
		}
	}
	return skip
}

func sortRanking(r []RankEntry, current string) {
	sort.SliceStable(r, func(i, j int) bool {
		a, b := r[i], r[j]
		if (a.Error == "") != (b.Error == "") {
			return a.Error == ""
		}
		if a.Healthy != b.Healthy {
			return a.Healthy
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		ac, bc := strings.EqualFold(a.Strategy, current), strings.EqualFold(b.Strategy, current)
		if ac != bc {
			return ac // при равенстве не дёргаем текущую
		}
		return a.LatencyMs < b.LatencyMs
	})
}

// testStrategy запускает winws со стратегией и проверяет адреса. Ошибка самой
// стратегии (не разбирается, winws не стартует) — это результат, а не сбой подбора:
// она пишется в RankEntry.Error, а error возвращается только при отмене или без сети.
func (a *App) testStrategy(ctx context.Context, relDir string, s Strategy, gf zapretfmt.GameFilter) (RankEntry, error) {
	e := RankEntry{Strategy: s.Name}
	cmdline, err := buildCommandLine(relDir, s, gf)
	if err != nil {
		e.Error = err.Error()
		return e, nil //nolint:nilerr // strategy error is recorded in the ranking
	}
	killProcesses("winws.exe")
	p, err := startWinws(cmdline, filepath.Join(relDir, "bin"), 8*time.Second)
	if err != nil {
		e.Error = err.Error()
		return e, nil //nolint:nilerr // strategy error is recorded in the ranking
	}
	rep := runHealth(ctx, a.targetsFor(relDir), a.Cfg.HealthyRatio, sweepTimeout, nil)
	p.Stop()
	if err := ctx.Err(); err != nil {
		return e, err
	}
	if rep.NetworkDown {
		return e, errNetworkDown
	}
	e.fill(rep)
	return e, nil
}

func (a *App) testStrategyRetry(ctx context.Context, relDir string, s Strategy, gf zapretfmt.GameFilter) (RankEntry, error) {
	e, err := a.testStrategy(ctx, relDir, s, gf)
	if errors.Is(err, errNetworkDown) {
		if serr := sleepCtx(ctx, 5*time.Second); serr != nil {
			return e, serr
		}
		e, err = a.testStrategy(ctx, relDir, s, gf)
	}
	return e, err
}

// sweep по очереди запускает winws с каждой стратегией и проверяет адреса — как
// «Run Tests → Standard» в service.bat. Тройку лучших перепроверяет и берёт худший
// из двух замеров, чтобы случайная удача не решала.
//
// skip — группы, которые не влияют на Healthy. full — полный перебор: тогда skip
// пересчитывается по его итогам (unfixableGroups) и возвращается вторым значением.
func (a *App) sweep(ctx context.Context, relDir string, list []Strategy, current string, skip map[string]bool, full bool, progress func(string)) ([]RankEntry, map[string]bool, error) {
	gf := readGameFilter(relDir)
	if err := ensureUserLists(relDir); err != nil {
		return nil, nil, err
	}
	ratio := a.Cfg.HealthyRatio
	byName := map[string]Strategy{}
	entries := make([]RankEntry, 0, len(list))
	for i, s := range list {
		byName[s.Name] = s
		e, err := a.testStrategyRetry(ctx, relDir, s, gf)
		if err != nil {
			return nil, nil, err
		}
		e.rehealth(ratio, skip)
		entries = append(entries, e)
		progress(fmt.Sprintf("  [%2d/%d] %s", i+1, len(list), e.Line()))
	}
	if full {
		skip = unfixableGroups(entries, ratio)
		for i := range entries {
			entries[i].rehealth(ratio, skip)
		}
	}
	sortRanking(entries, current)
	if len(entries) > 1 {
		for i := 0; i < len(entries) && i < 3; i++ {
			if entries[i].Score == 0 || entries[i].Error != "" {
				break
			}
			e2, err := a.testStrategyRetry(ctx, relDir, byName[entries[i].Strategy], gf)
			if err != nil {
				return nil, nil, err
			}
			e2.rehealth(ratio, skip)
			if e2.Error == "" && (e2.Score < entries[i].Score || (e2.Score == entries[i].Score && !e2.Healthy)) {
				entries[i] = e2
			} else if e2.Error == "" {
				entries[i].LatencyMs = (entries[i].LatencyMs + e2.LatencyMs) / 2
			}
			progress("  перепроверка: " + entries[i].Line())
		}
		sortRanking(entries, current)
	}
	return entries, skip, nil
}

type selectOpts struct {
	quick    bool // сперва проверить лучших из прошлого подбора
	apply    bool // поставить лучшую в службу
	progress func(string)
}

// quickCandidates — текущая стратегия плюс до 4 лучших из прошлого подбора этого релиза.
func (a *App) quickCandidates(relDir, current string, all []Strategy) []Strategy {
	st := a.loadState()
	if st.LastSweep == nil || !samePath(st.LastSweep.Dir, relDir) {
		return nil
	}
	byName := map[string]Strategy{}
	for _, s := range all {
		byName[strings.ToLower(s.Name)] = s
	}
	var out []Strategy
	if s, ok := byName[strings.ToLower(current)]; ok {
		out = append(out, s)
	}
	for _, e := range st.LastSweep.Ranking {
		if len(out) >= 5 {
			break
		}
		if strings.EqualFold(e.Strategy, current) || e.Score == 0 || e.Error != "" {
			continue
		}
		if s, ok := byName[strings.ToLower(e.Strategy)]; ok {
			out = append(out, s)
		}
	}
	return out
}

// selectStrategy останавливает службу, подбирает стратегию и (если apply) ставит лучшую.
// При любой ошибке служба zapret возвращается в прежнем виде.
func (a *App) selectStrategy(ctx context.Context, relDir, current string, o selectOpts) (*RankEntry, []RankEntry, error) {
	if o.progress == nil {
		o.progress = func(s string) { a.Log.Infof("%s", s) }
	}
	all, err := a.listAllStrategies(relDir)
	if err != nil {
		return nil, nil, err
	}
	if len(all) == 0 {
		return nil, nil, fmt.Errorf("в %s нет стратегий", relDir)
	}
	prev := queryService(zapretSvcName)
	if err := stopService(zapretSvcName, 15*time.Second); err != nil {
		return nil, nil, err
	}
	killProcesses("winws.exe")
	applied := false
	defer func() {
		killProcesses("winws.exe")
		if !applied && prev.Exists && prev.StartType != 4 /* disabled */ {
			if err := startService(zapretSvcName); err != nil {
				a.Log.Warnf("не удалось вернуть службу zapret: %v", err)
			}
		}
	}()

	skip := a.skippedGroups()
	var ranking []RankEntry
	if o.quick {
		if cands := a.quickCandidates(relDir, current, all); len(cands) > 1 {
			o.progress(fmt.Sprintf("быстрая проверка %d лучших стратегий из прошлого подбора…", len(cands)))
			r, _, err := a.sweep(ctx, relDir, cands, current, skip, false, o.progress)
			if err != nil {
				return nil, nil, err
			}
			if len(r) > 0 && r[0].Healthy {
				ranking = r
			} else {
				o.progress("лучшие из прошлого подбора не помогли — перебираю все")
			}
		}
	}
	if ranking == nil {
		o.progress(fmt.Sprintf("перебираю %d стратегий (пока идёт подбор, сайты будут то открываться, то нет)…", len(all)))
		r, unfixable, err := a.sweep(ctx, relDir, all, current, skip, true, o.progress)
		if err != nil {
			return nil, nil, err
		}
		ranking = r
		names := slices.Sorted(maps.Keys(unfixable))
		for _, name := range names {
			a.Log.Eventf("%s не открывается ни с одной стратегией: его режут не DPI (по IP, подменой DNS) или он недоступен сам — zapret тут не поможет. Сторож перепроверит через сутки",
				groupTitle(name))
		}
		ver := releaseVersion(relDir)
		a.updateState(func(st *State) {
			st.LastSweep = &SweepSummary{Time: time.Now(), Version: ver, Dir: relDir, Ranking: r, Unfixable: names}
		})
	}
	best := ranking[0]
	if best.Score == 0 || best.Error != "" {
		return &best, ranking, errNothingWorks
	}
	if !o.apply {
		return &best, ranking, nil
	}
	if err := a.applyStrategy(relDir, best.Strategy); err != nil {
		return &best, ranking, err
	}
	applied = true
	return &best, ranking, nil
}

// ---------- проверка и починка ----------

func (a *App) checkNow(ctx context.Context) *HealthReport {
	relDir, strategy := a.currentInstall()
	rep := runHealth(ctx, a.targetsFor(relDir), a.Cfg.HealthyRatio, checkTimeout, a.skippedGroups())
	a.updateState(func(st *State) { st.LastCheck = rep.summary(strategy) })
	return rep
}

// heal: перезапуск → быстрый подбор из прошлых лучших → полный подбор → проверка обновлений.
func (a *App) heal(ctx context.Context, rep *HealthReport) {
	relDir, strategy := a.currentInstall()
	if relDir == "" {
		return
	}
	st := a.loadState()
	if rep.DNSProblem {
		if time.Since(st.LastDNSWarn) > time.Hour {
			a.Log.Eventf("сайты не открываются из-за DNS: %s. Смена стратегии не поможет — включите DNS-over-HTTPS (Windows: Параметры → Сеть → DNS)",
				strings.Join(rep.failures(), "; "))
			a.updateState(func(s *State) { s.LastDNSWarn = time.Now() })
		}
		return
	}
	a.Log.Eventf("не работает (%s): %s — перезапускаю zapret", rep.shortText(), strings.Join(rep.failures(), "; "))
	_ = stopService(zapretSvcName, 15*time.Second)
	killProcesses("winws.exe")
	if err := startService(zapretSvcName); err != nil {
		a.Log.Warnf("перезапуск zapret: %v", err)
	}
	if sleepCtx(ctx, 3*time.Second) != nil {
		return
	}
	if r2 := a.checkNow(ctx); r2.Healthy {
		a.Log.Eventf("после перезапуска заработало (%s)", r2.shortText())
		return
	}
	// если подборы подряд ничего не находят (например, лёг сам Discord), реже дёргаем сеть
	// (сервисы, которые zapret не открывает вовсе, сюда не доходят — см. skippedGroups):
	// 30 мин → 1 ч → 2 ч → 4 ч
	minGap := time.Duration(a.Cfg.MinSweepIntervalMin) * time.Minute << min(st.HealStreak, 3)
	if since := time.Since(st.LastHeal); since < minGap {
		a.Log.Infof("подбор был %s назад, следующий не раньше чем через %s", since.Round(time.Minute), (minGap - since).Round(time.Minute))
		return
	}
	a.updateState(func(s *State) { s.LastHeal = time.Now() })
	a.Log.Eventf("перезапуск не помог — подбираю стратегию")
	best, _, err := a.selectStrategy(ctx, relDir, strategy, selectOpts{quick: true, apply: true})
	healed := false
	switch {
	case errors.Is(err, errNothingWorks):
		a.Log.Eventf("ни одна стратегия zapret %s сейчас не работает", releaseVersion(relDir))
	case err != nil:
		a.Log.Warnf("подбор не удался: %v", err)
		return
	default:
		if !strings.EqualFold(best.Strategy, strategy) {
			a.Log.Eventf("стратегия сменена: %s → %s (%s)", strategy, best.Strategy, best.groupsText())
		} else {
			a.Log.Eventf("текущая стратегия %s по-прежнему лучшая (%s)", best.Strategy, best.groupsText())
		}
		healed = best.Healthy
	}
	a.updateState(func(s *State) {
		if healed {
			s.HealStreak = 0
		} else {
			s.HealStreak++
		}
	})
	if healed {
		return
	}
	if a.Cfg.AutoUpdate {
		a.Log.Infof("проверяю, не вышла ли новая версия zapret…")
		if err := a.updateRelease(ctx, false); err != nil {
			a.Log.Warnf("обновление: %v", err)
		}
	}
}

// ---------- обновления ----------

// updateRelease ставит новый релиз Flowseal: качает, переносит настройки, ставит ту же
// стратегию (или подбирает), сравнивает с тем, что было, и откатывается, если стало хуже.
func (a *App) updateRelease(ctx context.Context, force bool) error {
	latest, err := a.fetchLatestVersion(ctx)
	if err != nil {
		return fmt.Errorf("не узнать последнюю версию: %w", err)
	}
	a.updateState(func(st *State) { st.LatestVersion = latest })
	relDir, strategy := a.currentInstall()
	cur := ""
	if relDir != "" {
		cur = releaseVersion(relDir)
	}
	st := a.loadState()
	if !force && cur != "" && version.Compare(latest, cur) <= 0 {
		a.Log.Infof("zapret %s — последняя версия", cur)
		return nil
	}
	if !force && slices.Contains(st.BadVersions, latest) {
		a.Log.Infof("версия %s уже откатывалась как нерабочая — пропускаю (update --force поставит)", latest)
		return nil
	}
	if cur != "" && cur != latest {
		a.Log.Eventf("вышел zapret %s (стоит %s) — обновляю", latest, cur)
	}
	newDir, err := a.downloadRelease(ctx, latest, force)
	if err != nil {
		return err
	}
	if relDir != "" && !samePath(relDir, newDir) {
		if err := migrateUserFiles(relDir, newDir); err != nil {
			a.Log.Warnf("перенос своих списков и настроек: %v", err)
		}
	}
	if err := ensureUserLists(newDir); err != nil {
		return err
	}
	if a.Cfg.UpdateIpset {
		if _, err := a.updateIpset(ctx, newDir); err != nil {
			a.Log.Warnf("ipset: %v", err)
		}
	}
	if a.Cfg.UpdateListsFromMain {
		if _, err := a.updateMainLists(ctx, newDir); err != nil {
			a.Log.Warnf("списки из main: %v", err)
		}
	}
	if relDir == "" || strategy == "" {
		_, _, err := a.selectStrategy(ctx, newDir, "", selectOpts{apply: true})
		return err
	}
	if samePath(relDir, newDir) {
		// та же папка (force): просто переставим текущую стратегию
		return a.applyStrategy(newDir, strategy)
	}

	var base *HealthReport
	if queryService(zapretSvcName).State == svc.Running {
		base = runHealth(ctx, a.targetsFor(relDir), a.Cfg.HealthyRatio, checkTimeout, a.skippedGroups())
	}
	var rep *HealthReport
	if _, ferr := a.findStrategy(newDir, strategy); ferr == nil {
		if err := a.applyStrategy(newDir, strategy); err != nil {
			a.Log.Warnf("стратегия %s в новой версии не запустилась: %v", strategy, err)
		} else {
			_ = sleepCtx(ctx, 2*time.Second)
			rep = runHealth(ctx, a.targetsFor(newDir), a.Cfg.HealthyRatio, checkTimeout, a.skippedGroups())
		}
	} else {
		a.Log.Infof("в новой версии нет стратегии %s", strategy)
	}
	if rep == nil || !rep.Healthy {
		a.Log.Infof("подбираю стратегию в новой версии…")
		if _, _, err := a.selectStrategy(ctx, newDir, strategy, selectOpts{apply: true}); err == nil {
			rep = runHealth(ctx, a.targetsFor(newDir), a.Cfg.HealthyRatio, checkTimeout, a.skippedGroups())
		} else if !errors.Is(err, errNothingWorks) {
			a.Log.Warnf("подбор: %v", err)
		}
	}
	if releaseIsWorse(base, rep) {
		a.Log.Eventf("zapret %s работает хуже, чем %s — откатываюсь", latest, cur)
		if err := a.applyStrategy(relDir, strategy); err != nil {
			return fmt.Errorf("откат не удался: %w", err)
		}
		a.updateState(func(st *State) { st.BadVersions = appendUnique(st.BadVersions, latest) })
		return errors.New("новая версия хуже — оставил прежнюю")
	}
	_, nowStrategy := a.currentInstall()
	a.Log.Eventf("zapret обновлён: %s → %s, стратегия %s", cur, latest, nowStrategy)
	a.pruneReleases(newDir)
	return nil
}

// releaseIsWorse решает, откатывать ли новый релиз. base — замер на старом релизе
// (nil, если zapret не работал), rep — на новом (nil, если ни одна стратегия не встала).
// Откатываемся, только если старый релиз был здоров, а новый — нет или открывает меньше.
// Без интернета во время замера (NetworkDown) не судим: это не вина релиза.
func releaseIsWorse(base, rep *HealthReport) bool {
	if base == nil || base.NetworkDown || !base.Healthy {
		return false
	}
	if rep == nil {
		return true
	}
	return !rep.NetworkDown && (!rep.Healthy || rep.Score < base.Score)
}

// updateAll — плановое обновление: релиз, ipset, списки, hosts. manual=true — по команде
// пользователя (тогда выключенные в конфиге пункты тоже выполняются, кроме hosts).
func (a *App) updateAll(ctx context.Context, manual, force bool) {
	if a.Cfg.AutoUpdate || manual {
		if err := a.updateRelease(ctx, force); err != nil {
			a.Log.Warnf("релиз: %v", err)
		}
	}
	relDir, _ := a.currentInstall()
	if relDir != "" && (a.Cfg.UpdateIpset || manual) {
		if changed, err := a.updateIpset(ctx, relDir); err != nil {
			a.Log.Warnf("ipset: %v", err)
		} else {
			a.updateState(func(st *State) { st.LastIpsetUpdate = time.Now() })
			if changed {
				a.Log.Eventf("ipset обновлён (%d подсетей)", countIpsetLines(relDir))
			} else {
				a.Log.Infof("ipset актуален")
			}
		}
	}
	if relDir != "" && (a.Cfg.UpdateListsFromMain || manual) {
		changed, err := a.updateMainLists(ctx, relDir)
		if err != nil {
			a.Log.Warnf("списки: %v", err)
		}
		a.updateState(func(st *State) { st.LastListsUpdate = time.Now() })
		if len(changed) > 0 {
			a.Log.Eventf("обновлены списки из main: %s", strings.Join(changed, ", "))
		} else if err == nil {
			a.Log.Infof("списки доменов актуальны")
		}
	}
	if relDir != "" {
		if names, err := syncCatalog(relDir); err != nil {
			a.Log.Warnf("сервисы из каталога: %v", err)
		} else if len(names) > 0 {
			a.Log.Eventf("обновлены домены сервисов: %s", strings.Join(names, ", "))
		}
	}
	if a.Cfg.UpdateHosts {
		if changed, err := a.updateHosts(ctx); err != nil {
			a.Log.Warnf("hosts: %v", err)
		} else {
			a.updateState(func(st *State) { st.LastHostsUpdate = time.Now() })
			if changed {
				a.Log.Eventf("hosts обновлён (блок Flowseal: github, telegram, discord)")
			} else {
				a.Log.Infof("hosts актуален")
			}
		}
	}
	a.updateState(func(st *State) { st.LastUpdateCheck = time.Now() })
}

// ---------- мелочи ----------

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func appendUnique(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}
