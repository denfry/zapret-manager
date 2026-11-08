package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
)

var (
	errNetworkDown  = errors.New("пропал интернет (контрольные ya.ru и vk.com тоже не открываются)")
	errNothingWorks = errors.New("ни одна стратегия не открыла Discord и YouTube")
)

// currentInstall: какая папка релиза и стратегия стоят сейчас. Источник правды —
// служба zapret (её мог поставить и service.bat руками), запасной — state.json.
func (a *App) currentInstall() (relDir, strategy string) {
	info := queryService(zapretSvcName)
	if info.Exists && info.BinPath != "" {
		exe := exeFromCmdline(info.BinPath)
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
	enableTCPTimestamps()
	oldDir, _ := a.currentInstall()
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
	if err := startService(zapretSvcName, 15*time.Second); err != nil {
		// служба не говорит, что не так, — запустим ту же строку руками и покажем вывод winws
		if p, perr := startWinws(cmdline, filepath.Join(relDir, "bin"), 3*time.Second); perr != nil {
			err = fmt.Errorf("%v; %v", err, perr)
		} else {
			p.Stop()
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
	Strategy  string   `json:"strategy"`
	Score     int      `json:"score"`
	Max       int      `json:"max"`
	Discord   string   `json:"discord"`
	YouTube   string   `json:"youtube"`
	Other     string   `json:"other,omitempty"`
	LatencyMs int64    `json:"latency_ms"`
	Healthy   bool     `json:"healthy"`
	Fails     []string `json:"fails,omitempty"`
	Error     string   `json:"error,omitempty"`
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
	e.LatencyMs = rep.LatencyMs
	e.Healthy = rep.Healthy
	e.Fails = rep.failures()
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

func (a *App) testStrategy(ctx context.Context, relDir string, s Strategy, gf GameFilter) (RankEntry, error) {
	e := RankEntry{Strategy: s.Name}
	cmdline, err := buildCommandLine(relDir, s, gf)
	if err != nil {
		e.Error = err.Error()
		return e, nil
	}
	killProcesses("winws.exe")
	p, err := startWinws(cmdline, filepath.Join(relDir, "bin"), 8*time.Second)
	if err != nil {
		e.Error = err.Error()
		return e, nil
	}
	rep := runHealth(ctx, a.targets(), a.Cfg.HealthyRatio, sweepTimeout)
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

func (a *App) testStrategyRetry(ctx context.Context, relDir string, s Strategy, gf GameFilter) (RankEntry, error) {
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
func (a *App) sweep(ctx context.Context, relDir string, list []Strategy, current string, progress func(string)) ([]RankEntry, error) {
	gf := readGameFilter(relDir)
	if err := ensureUserLists(relDir); err != nil {
		return nil, err
	}
	byName := map[string]Strategy{}
	entries := make([]RankEntry, 0, len(list))
	for i, s := range list {
		byName[s.Name] = s
		e, err := a.testStrategyRetry(ctx, relDir, s, gf)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
		progress(fmt.Sprintf("  [%2d/%d] %s", i+1, len(list), e.Line()))
	}
	sortRanking(entries, current)
	if len(entries) > 1 {
		for i := 0; i < len(entries) && i < 3; i++ {
			if entries[i].Score == 0 || entries[i].Error != "" {
				break
			}
			e2, err := a.testStrategyRetry(ctx, relDir, byName[entries[i].Strategy], gf)
			if err != nil {
				return nil, err
			}
			if e2.Error == "" && (e2.Score < entries[i].Score || (e2.Score == entries[i].Score && !e2.Healthy)) {
				entries[i] = e2
			} else if e2.Error == "" {
				entries[i].LatencyMs = (entries[i].LatencyMs + e2.LatencyMs) / 2
			}
			progress("  перепроверка: " + entries[i].Line())
		}
		sortRanking(entries, current)
	}
	return entries, nil
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
			if err := startService(zapretSvcName, 15*time.Second); err != nil {
				a.Log.Warnf("не удалось вернуть службу zapret: %v", err)
			}
		}
	}()

	var ranking []RankEntry
	if o.quick {
		if cands := a.quickCandidates(relDir, current, all); len(cands) > 1 {
			o.progress(fmt.Sprintf("быстрая проверка %d лучших стратегий из прошлого подбора…", len(cands)))
			r, err := a.sweep(ctx, relDir, cands, current, o.progress)
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
		o.progress(fmt.Sprintf("перебираю %d стратегий (Discord и YouTube на пару минут будут мигать)…", len(all)))
		r, err := a.sweep(ctx, relDir, all, current, o.progress)
		if err != nil {
			return nil, nil, err
		}
		ranking = r
		ver := releaseVersion(relDir)
		a.updateState(func(st *State) {
			st.LastSweep = &SweepSummary{Time: time.Now(), Version: ver, Dir: relDir, Ranking: r}
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
	rep := runHealth(ctx, a.targets(), a.Cfg.HealthyRatio, checkTimeout)
	_, strategy := a.currentInstall()
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
			a.Log.Eventf("Discord/YouTube не открываются из-за DNS: %s. Смена стратегии не поможет — включите DNS-over-HTTPS (Windows: Параметры → Сеть → DNS)",
				strings.Join(rep.failures(), "; "))
			a.updateState(func(s *State) { s.LastDNSWarn = time.Now() })
		}
		return
	}
	a.Log.Eventf("не работает (%s): %s — перезапускаю zapret", rep.shortText(), strings.Join(rep.failures(), "; "))
	_ = stopService(zapretSvcName, 15*time.Second)
	killProcesses("winws.exe")
	if err := startService(zapretSvcName, 15*time.Second); err != nil {
		a.Log.Warnf("перезапуск zapret: %v", err)
	}
	if sleepCtx(ctx, 3*time.Second) != nil {
		return
	}
	if r2 := a.checkNow(ctx); r2.Healthy {
		a.Log.Eventf("после перезапуска заработало (%s)", r2.shortText())
		return
	}
	minGap := time.Duration(a.Cfg.MinSweepIntervalMin) * time.Minute
	if since := time.Since(st.LastHeal); since < minGap {
		a.Log.Infof("подбор был %s назад, следующий не раньше чем через %s", since.Round(time.Minute), (minGap - since).Round(time.Minute))
		return
	}
	a.updateState(func(s *State) { s.LastHeal = time.Now() })
	a.Log.Eventf("перезапуск не помог — подбираю стратегию")
	best, _, err := a.selectStrategy(ctx, relDir, strategy, selectOpts{quick: true, apply: true})
	switch {
	case errors.Is(err, errNothingWorks):
		a.Log.Eventf("ни одна стратегия zapret %s сейчас не работает", releaseVersion(relDir))
	case err != nil:
		a.Log.Warnf("подбор не удался: %v", err)
		return
	default:
		if !strings.EqualFold(best.Strategy, strategy) {
			a.Log.Eventf("стратегия сменена: %s → %s (Discord %s, YouTube %s)", strategy, best.Strategy, best.Discord, best.YouTube)
		} else {
			a.Log.Eventf("текущая стратегия %s по-прежнему лучшая (Discord %s, YouTube %s)", best.Strategy, best.Discord, best.YouTube)
		}
		if best.Healthy {
			return
		}
	}
	if a.Cfg.AutoUpdate {
		a.Log.Infof("проверяю, не вышла ли новая версия zapret…")
		if _, err := a.updateRelease(ctx, false); err != nil {
			a.Log.Warnf("обновление: %v", err)
		}
	}
}

// ---------- обновления ----------

// updateRelease ставит новый релиз Flowseal: качает, переносит настройки, ставит ту же
// стратегию (или подбирает), сравнивает с тем, что было, и откатывается, если стало хуже.
func (a *App) updateRelease(ctx context.Context, force bool) (bool, error) {
	latest, err := a.fetchLatestVersion(ctx)
	if err != nil {
		return false, fmt.Errorf("не узнать последнюю версию: %w", err)
	}
	a.updateState(func(st *State) { st.LatestVersion = latest })
	relDir, strategy := a.currentInstall()
	cur := ""
	if relDir != "" {
		cur = releaseVersion(relDir)
	}
	st := a.loadState()
	if !force && cur != "" && compareVersions(latest, cur) <= 0 {
		a.Log.Infof("zapret %s — последняя версия", cur)
		return false, nil
	}
	if !force {
		for _, bad := range st.BadVersions {
			if bad == latest {
				a.Log.Infof("версия %s уже откатывалась как нерабочая — пропускаю (update --force поставит)", latest)
				return false, nil
			}
		}
	}
	if cur != "" && cur != latest {
		a.Log.Eventf("вышел zapret %s (стоит %s) — обновляю", latest, cur)
	}
	newDir, err := a.downloadRelease(ctx, latest, force)
	if err != nil {
		return false, err
	}
	if relDir != "" && !samePath(relDir, newDir) {
		migrateUserFiles(relDir, newDir)
	}
	if err := ensureUserLists(newDir); err != nil {
		return false, err
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
		return err == nil, err
	}
	if samePath(relDir, newDir) {
		// та же папка (force): просто переставим текущую стратегию
		return true, a.applyStrategy(newDir, strategy)
	}

	var base *HealthReport
	if queryService(zapretSvcName).State == svc.Running {
		base = runHealth(ctx, a.targets(), a.Cfg.HealthyRatio, checkTimeout)
	}
	var rep *HealthReport
	if _, ferr := a.findStrategy(newDir, strategy); ferr == nil {
		if err := a.applyStrategy(newDir, strategy); err != nil {
			a.Log.Warnf("стратегия %s в новой версии не запустилась: %v", strategy, err)
		} else {
			_ = sleepCtx(ctx, 2*time.Second)
			rep = runHealth(ctx, a.targets(), a.Cfg.HealthyRatio, checkTimeout)
		}
	} else {
		a.Log.Infof("в новой версии нет стратегии %s", strategy)
	}
	if rep == nil || !rep.Healthy {
		a.Log.Infof("подбираю стратегию в новой версии…")
		if _, _, err := a.selectStrategy(ctx, newDir, strategy, selectOpts{apply: true}); err == nil {
			rep = runHealth(ctx, a.targets(), a.Cfg.HealthyRatio, checkTimeout)
		} else if !errors.Is(err, errNothingWorks) {
			a.Log.Warnf("подбор: %v", err)
		}
	}
	worse := base != nil && !base.NetworkDown && base.Healthy &&
		(rep == nil || (!rep.NetworkDown && (!rep.Healthy || rep.Score < base.Score)))
	if worse {
		a.Log.Eventf("zapret %s работает хуже, чем %s — откатываюсь", latest, cur)
		if err := a.applyStrategy(relDir, strategy); err != nil {
			return false, fmt.Errorf("откат не удался: %w", err)
		}
		a.updateState(func(st *State) { st.BadVersions = appendUnique(st.BadVersions, latest) })
		return false, errors.New("новая версия хуже — оставил прежнюю")
	}
	_, nowStrategy := a.currentInstall()
	a.Log.Eventf("zapret обновлён: %s → %s, стратегия %s", cur, latest, nowStrategy)
	a.pruneReleases(newDir)
	return true, nil
}

// updateAll — плановое обновление: релиз, ipset, списки, hosts. manual=true — по команде
// пользователя (тогда выключенные в конфиге пункты тоже выполняются, кроме hosts).
func (a *App) updateAll(ctx context.Context, manual, force bool) {
	if a.Cfg.AutoUpdate || manual {
		if _, err := a.updateRelease(ctx, force); err != nil {
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
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
