package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const (
	hostsBegin = "# >>> zapret-manager: hosts from Flowseal/zapret-discord-youtube (auto-updated) >>>"
	hostsEnd   = "# <<< zapret-manager <<<"
)

func hostsPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "drivers", "etc", "hosts")
}

// parseUpstreamHosts проверяет .service/hosts и возвращает строки блока.
func parseUpstreamHosts(data []byte) ([]string, error) {
	lines := cleanLines(data)
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	entries := 0
	var out []string
	prevBlank := false
	for _, l := range lines {
		if l == "" {
			if !prevBlank {
				out = append(out, "")
			}
			prevBlank = true
			continue
		}
		prevBlank = false
		if strings.HasPrefix(l, "#") {
			out = append(out, l)
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 || net.ParseIP(f[0]) == nil {
			return nil, fmt.Errorf("строка %q не похожа на запись hosts", l)
		}
		entries++
		out = append(out, strings.Join(f, " "))
	}
	if entries < 3 {
		return nil, errors.New("в hosts из репозитория почти нет записей")
	}
	return out, nil
}

func hostsNames(lines []string) map[string]bool {
	set := map[string]bool{}
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		body, _, _ := strings.Cut(l, "#")
		f := strings.Fields(body)
		for _, h := range f[1:] {
			set[strings.ToLower(h)] = true
		}
	}
	return set
}

// mergeHosts убирает наш старый блок и чужие записи для тех же имён (иначе на одно
// имя будет два разных IP), остальное оставляет как было и дописывает свежий блок в конец.
func mergeHosts(local string, upstream []string) (string, bool) {
	names := hostsNames(upstream)
	text := strings.TrimPrefix(strings.ReplaceAll(local, "\r\n", "\n"), "\uFEFF")
	lines := strings.Split(text, "\n")

	begin, end := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == hostsBegin && begin < 0 {
			begin = i
		} else if t == hostsEnd && begin >= 0 {
			end = i
			break
		}
	}

	var kept []string
	for i, l := range lines {
		if begin >= 0 && end >= 0 && i >= begin && i <= end {
			continue
		}
		t := strings.TrimSpace(l)
		if t == hostsBegin || t == hostsEnd {
			continue // осиротевший маркер
		}
		if t == "" || strings.HasPrefix(t, "#") {
			kept = append(kept, strings.TrimRight(l, "\r"))
			continue
		}
		body, comment, hasComment := strings.Cut(t, "#")
		f := strings.Fields(body)
		if len(f) < 2 {
			kept = append(kept, strings.TrimRight(l, "\r"))
			continue
		}
		var remain []string
		for _, h := range f[1:] {
			if !names[strings.ToLower(h)] {
				remain = append(remain, h)
			}
		}
		switch {
		case len(remain) == len(f)-1:
			kept = append(kept, strings.TrimRight(l, "\r"))
		case len(remain) == 0:
			// запись целиком про наши имена — выкидываем
		default:
			nl := f[0] + " " + strings.Join(remain, " ")
			if hasComment {
				nl += " #" + comment
			}
			kept = append(kept, nl)
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	var b strings.Builder
	for _, k := range kept {
		b.WriteString(k + "\r\n")
	}
	if len(kept) > 0 {
		b.WriteString("\r\n")
	}
	b.WriteString(hostsBegin + "\r\n")
	for _, u := range upstream {
		b.WriteString(u + "\r\n")
	}
	b.WriteString(hostsEnd + "\r\n")
	out := b.String()
	return out, normalizeText(out) != normalizeText(local)
}

// removeHostsBlock убирает только наш блок (для uninstall --all).
func removeHostsBlock(local string) (string, bool) {
	text := strings.ReplaceAll(local, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var kept []string
	in := false
	changed := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == hostsBegin {
			in, changed = true, true
			continue
		}
		if in {
			if t == hostsEnd {
				in = false
			}
			continue
		}
		kept = append(kept, l)
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	return strings.Join(kept, "\r\n") + "\r\n", changed
}

func (a *App) updateHosts(ctx context.Context) (bool, error) {
	data, err := httpGet(ctx, a.rawURL(".service/hosts"), 1<<20)
	if err != nil {
		return false, err
	}
	upstream, err := parseUpstreamHosts(data)
	if err != nil {
		return false, err
	}
	local, err := os.ReadFile(hostsPath())
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	merged, changed := mergeHosts(string(local), upstream)
	if !changed {
		return false, nil
	}
	if err := a.writeHosts(local, merged); err != nil {
		return false, err
	}
	return true, nil
}

func (a *App) writeHosts(old []byte, content string) error {
	p := hostsPath()
	if len(old) > 0 {
		backup := a.path("backups", "hosts-"+time.Now().Format("20060102-150405")+".bak")
		if err := os.MkdirAll(filepath.Dir(backup), 0o755); err == nil {
			_ = os.WriteFile(backup, old, 0o644)
			a.pruneBackups()
		}
	}
	err := os.WriteFile(p, []byte(content), 0o644)
	if err != nil {
		// бывает, что hosts помечен «только чтение»
		pp, _ := windows.UTF16PtrFromString(p)
		if attrs, aerr := windows.GetFileAttributes(pp); aerr == nil && attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
			_ = windows.SetFileAttributes(pp, attrs&^windows.FILE_ATTRIBUTE_READONLY)
			err = os.WriteFile(p, []byte(content), 0o644)
			_ = windows.SetFileAttributes(pp, attrs)
		}
	}
	if err != nil {
		return fmt.Errorf("не записать hosts: %w", err)
	}
	if back, rerr := os.ReadFile(p); rerr == nil && normalizeText(string(back)) != normalizeText(content) {
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
	for len(names) > 10 {
		_ = os.Remove(a.path("backups", names[0]))
		names = names[1:]
	}
}
