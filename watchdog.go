package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	pbtApmResumeSuspend   = 0x7
	pbtApmResumeAutomatic = 0x12
)

type managerService struct{ app *App }

func runAsService() {
	a := newApp("", false)
	if err := svc.Run(managerSvcName, &managerService{app: a}); err != nil {
		a.Log.Warnf("служба не запустилась: %v", err)
		os.Exit(1)
	}
}

func (m *managerService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if rec := recover(); rec != nil {
				m.app.Log.Warnf("сторож упал: %v", rec)
			}
		}()
		m.app.watchdog(ctx, wake)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPowerEvent}
	for {
		select {
		case <-done:
			// сторож завершился сам (паника) — пусть SCM перезапустит по recovery
			cancel()
			return true, 1
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, WaitHint: 30000}
				cancel()
				select {
				case <-done:
				case <-time.After(25 * time.Second):
				}
				m.app.Log.Infof("служба остановлена")
				return false, 0
			case svc.PowerEvent:
				if c.EventType == pbtApmResumeAutomatic || c.EventType == pbtApmResumeSuspend {
					select {
					case wake <- "выход из сна":
					default:
					}
				}
			}
		}
	}
}

// watchdog — главный цикл службы.
func (a *App) watchdog(ctx context.Context, wake <-chan string) {
	a.Log.Eventf("сторож запущен (zapret-manager %s, папка %s)", appVersion, a.Root)
	fails := 0
	now := time.Now()
	nextCheck := now.Add(45 * time.Second)
	st := a.loadState()
	nextUpdate := st.LastUpdateCheck.Add(time.Duration(a.Cfg.UpdateIntervalHours) * time.Hour)
	if nextUpdate.Before(now.Add(2 * time.Minute)) {
		nextUpdate = now.Add(2 * time.Minute)
	}
	nextAlive := now.Add(30 * time.Second)
	for {
		wait := time.Until(nextCheck)
		if u := time.Until(nextUpdate); u < wait {
			wait = u
		}
		if u := time.Until(nextAlive); u < wait {
			wait = u
		}
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case reason := <-wake:
			timer.Stop()
			a.Log.Infof("%s — проверю zapret через 20 секунд", reason)
			nextCheck = time.Now().Add(20 * time.Second)
			continue
		case <-timer.C:
		}
		a.reloadConfig()
		now = time.Now()

		if a.handlePause(now) {
			nextCheck = now.Add(time.Minute)
			nextAlive = now.Add(30 * time.Second)
			continue
		}
		if !now.Before(nextAlive) {
			nextAlive = now.Add(30 * time.Second)
			a.ensureRunning()
		}
		if !now.Before(nextUpdate) {
			err := a.withLock(0, func() error {
				a.updateAll(ctx, false, false)
				return nil
			})
			if errors.Is(err, errBusy) {
				nextUpdate = now.Add(5 * time.Minute)
			} else {
				nextUpdate = now.Add(time.Duration(a.Cfg.UpdateIntervalHours) * time.Hour)
			}
			if ctx.Err() != nil {
				return
			}
		}
		if !time.Now().Before(nextCheck) {
			nextCheck = time.Now().Add(a.periodicCheck(ctx, &fails))
		}
	}
}

// handlePause: true — менеджер на паузе (команда off), ничего не трогаем.
func (a *App) handlePause(now time.Time) bool {
	st := a.loadState()
	if st.PausedUntil.IsZero() {
		return false
	}
	if now.Before(st.PausedUntil) {
		return true
	}
	a.Log.Eventf("пауза закончилась — включаю zapret")
	a.updateState(func(s *State) { s.PausedUntil = time.Time{} })
	_ = setServiceStartType(zapretSvcName, mgr.StartAutomatic)
	if err := startService(zapretSvcName, 15*time.Second); err != nil {
		a.Log.Warnf("zapret: %v", err)
	}
	return false
}

// ensureRunning — дешёвая проверка раз в 30 с: служба zapret должна работать.
// Во время подбора блокировка занята, и сюда не попадаем.
func (a *App) ensureRunning() {
	info := queryService(zapretSvcName)
	if !info.Exists || info.State == svc.Running || info.State == svc.StartPending {
		return
	}
	if time.Since(a.lastStartFail) < 5*time.Minute {
		return
	}
	_ = a.withLock(0, func() error {
		info := queryService(zapretSvcName)
		if !info.Exists || info.State == svc.Running {
			return nil
		}
		a.Log.Eventf("служба zapret была %s — запускаю", stateName(info.State))
		if info.StartType == mgr.StartDisabled {
			_ = setServiceStartType(zapretSvcName, mgr.StartAutomatic)
		}
		if err := startService(zapretSvcName, 15*time.Second); err != nil {
			a.lastStartFail = time.Now()
			a.Log.Warnf("zapret не запускается (%v) — следующая попытка через 5 минут", err)
		}
		return nil
	})
}

// periodicCheck проверяет Discord/YouTube и при подтверждённой поломке лечит.
// Возвращает, через сколько проверять снова.
func (a *App) periodicCheck(ctx context.Context, fails *int) time.Duration {
	delay := time.Duration(a.Cfg.CheckIntervalSec) * time.Second
	err := a.withLock(0, func() error {
		relDir, strategy := a.currentInstall()
		if relDir == "" {
			a.Log.Debugf("zapret не установлен — проверять нечего")
			return nil
		}
		info := queryService(zapretSvcName)
		switch {
		case !info.Exists:
			if strategy == "" {
				return nil
			}
			a.Log.Eventf("служба zapret пропала — ставлю заново (%s)", strategy)
			if err := a.applyStrategy(relDir, strategy); err != nil {
				a.Log.Warnf("%v", err)
				return nil
			}
		case info.State != svc.Running:
			a.Log.Eventf("служба zapret была %s — запускаю", stateName(info.State))
			if info.StartType == mgr.StartDisabled {
				_ = setServiceStartType(zapretSvcName, mgr.StartAutomatic)
			}
			if err := startService(zapretSvcName, 15*time.Second); err != nil {
				a.Log.Warnf("%v", err)
			}
			_ = sleepCtx(ctx, 3*time.Second)
		case len(processPIDs("winws.exe")) > 1:
			a.Log.Warnf("запущено несколько winws.exe — похоже, рядом запущен general*.bat или другой обход")
		}

		rep := a.checkNow(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case rep.NetworkDown:
			a.Log.Debugf("интернета нет (контрольные сайты не открываются) — жду")
			*fails = 0
			return nil
		case rep.Healthy:
			if *fails > 0 {
				a.Log.Infof("снова работает (%s)", rep.shortText())
			}
			if a.loadState().HealStreak > 0 {
				a.updateState(func(s *State) { s.HealStreak = 0 })
			}
			a.Log.Debugf("ok: %s", rep.shortText())
			*fails = 0
			return nil
		}
		*fails++
		a.Log.Infof("проверка %d/%d не прошла: %s", *fails, a.Cfg.FailuresBeforeHeal, rep.shortText())
		if *fails < a.Cfg.FailuresBeforeHeal {
			delay = 30 * time.Second
			return nil
		}
		*fails = 0
		if !a.Cfg.AutoHeal {
			a.Log.Eventf("Discord/YouTube не работают (%s), автопочинка выключена в config.json", rep.shortText())
			return nil
		}
		a.heal(ctx, rep)
		return nil
	})
	if errors.Is(err, errBusy) {
		return time.Minute
	}
	if err != nil {
		a.Log.Warnf("проверка: %v", err)
	}
	return delay
}

// stopManagerIfRunning нужен install: exe службы занят, пока она работает.
func stopManagerIfRunning() error {
	info := queryService(managerSvcName)
	if !info.Exists || info.State == svc.Stopped {
		return nil
	}
	if err := stopService(managerSvcName, 40*time.Second); err != nil {
		return fmt.Errorf("не остановить службу %s: %w", managerSvcName, err)
	}
	return nil
}
