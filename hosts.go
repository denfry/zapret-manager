package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"zapret-manager/internal/hostsfile"
	"zapret-manager/internal/textutil"
)

// maxHostsBackups — сколько копий hosts хранить в backups\.
const maxHostsBackups = 10

// hostsPath берёт папку System32 у Windows, а не из переменной SystemRoot: окружение
// процесса подконтрольно пользователю, а пишем в hosts мы с правами администратора.
func hostsPath() string {
	sys, err := windows.GetSystemDirectory()
	if err != nil || sys == "" {
		sys = `C:\Windows\System32`
	}
	return filepath.Join(sys, "drivers", "etc", "hosts")
}

func (a *App) updateHosts(ctx context.Context) (bool, error) {
	data, err := a.dl().get(ctx, a.rawURL(".service/hosts"), maxHostsBytes)
	if err != nil {
		return false, err
	}
	upstream, err := hostsfile.ParseUpstream(data)
	if err != nil {
		return false, err
	}
	local, err := os.ReadFile(hostsPath())
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	merged, changed := hostsfile.Merge(string(local), upstream)
	if !changed {
		return false, nil
	}
	if err := a.writeHosts(local, merged); err != nil {
		return false, err
	}
	return true, nil
}

// writeHosts сохраняет копию старого hosts в backups\ и пишет новый. Копия
// обязательна: без неё hosts не трогаем.
func (a *App) writeHosts(old []byte, content string) error {
	p := hostsPath()
	if len(old) > 0 {
		// миллисекунды: две записи за секунду не должны затирать копию исходного hosts
		backup := a.path("backups", "hosts-"+time.Now().Format("20060102-150405.000")+".bak")
		if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
			return fmt.Errorf("копия hosts: %w", err)
		}
		if err := writeFileReplace(backup, old); err != nil {
			return fmt.Errorf("копия hosts: %w", err)
		}
		a.pruneBackups()
	}
	// через временный файл и замену: сбой посреди записи не оставит hosts обрезанным
	// (если заменить нельзя — файл занят, — writeFileAtomic пишет поверх)
	err := writeFileAtomic(p, []byte(content))
	if err != nil {
		// бывает, что hosts помечен «только чтение»
		pp, _ := windows.UTF16PtrFromString(p)
		if attrs, aerr := windows.GetFileAttributes(pp); aerr == nil && attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
			_ = windows.SetFileAttributes(pp, attrs&^windows.FILE_ATTRIBUTE_READONLY)
			err = writeFileAtomic(p, []byte(content))
			_ = windows.SetFileAttributes(pp, attrs)
		}
	}
	if err != nil {
		return fmt.Errorf("не записать hosts: %w", err)
	}
	if back, rerr := os.ReadFile(p); rerr == nil && textutil.NormalizeText(string(back)) != textutil.NormalizeText(content) {
		return errors.New("hosts записан, но содержимое другое — его, вероятно, откатил антивирус")
	}
	flushDNS()
	return nil
}

func (a *App) pruneBackups() {
	entries, err := os.ReadDir(a.path("backups"))
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hosts-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > maxHostsBackups {
		_ = os.Remove(a.path("backups", names[0]))
		names = names[1:]
	}
}
