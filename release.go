package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- HTTP для скачивания ----------

var dlClient = &http.Client{
	Timeout: 3 * time.Minute,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

func httpGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*3) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "zapret-manager/"+appVersion)
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := dlClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s: 404", url)
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("%s: ответ больше %d байт", url, limit)
		}
		return data, nil
	}
	return nil, lastErr
}

func (a *App) rawURL(file string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s?t=%d", a.Cfg.Repo, a.Cfg.Branch, file, time.Now().Unix())
}

// ---------- версии ----------

var localVersionRe = regexp.MustCompile(`(?im)^\s*set\s+"LOCAL_VERSION=([^"]+)"`)
var versionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.\-]{0,30}$`)

func releaseVersion(relDir string) string {
	if data, err := os.ReadFile(filepath.Join(relDir, "service.bat")); err == nil {
		if m := localVersionRe.FindSubmatch(data); m != nil {
			return strings.TrimSpace(string(m[1]))
		}
	}
	base := filepath.Base(relDir)
	if i := strings.LastIndex(base, "-"); i >= 0 && versionRe.MatchString(base[i+1:]) {
		return base[i+1:]
	}
	return base
}

func (a *App) fetchLatestVersion(ctx context.Context) (string, error) {
	data, err := httpGet(ctx, a.rawURL(".service/version.txt"), 256)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(stripBOM(data)))
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("странная версия в version.txt: %q", v)
	}
	return v, nil
}

var versionTokenRe = regexp.MustCompile(`\d+|[A-Za-z]+`)

// compareVersions: 1.10.4 < 1.10.5 < 1.10.5b < 1.11.0.
func compareVersions(a, b string) int {
	ta := versionTokenRe.FindAllString(a, -1)
	tb := versionTokenRe.FindAllString(b, -1)
	for i := 0; i < len(ta) && i < len(tb); i++ {
		na, ea := strconv.Atoi(ta[i])
		nb, eb := strconv.Atoi(tb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		case ea == nil: // число новее суффикса
			return 1
		case eb == nil:
			return -1
		default:
			if c := strings.Compare(strings.ToLower(ta[i]), strings.ToLower(tb[i])); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(ta) < len(tb):
		return -1
	case len(ta) > len(tb):
		return 1
	}
	return 0
}

// ---------- релизы ----------

func validRelease(dir string) bool {
	for _, f := range []string{`bin\winws.exe`, `bin\WinDivert.dll`, `bin\WinDivert64.sys`, `service.bat`} {
		if !fileExists(filepath.Join(dir, f)) {
			return false
		}
	}
	if !dirExists(filepath.Join(dir, "lists")) {
		return false
	}
	strategies, err := listStrategies(dir, false)
	return err == nil && len(strategies) > 0
}

// downloadRelease скачивает zip релиза в releases\<версия>. Уже скачанный не трогает,
// если не force; с force кладёт рядом в releases\<версия>-<время>, потому что в старой
// папке может работать winws.
func (a *App) downloadRelease(ctx context.Context, ver string, force bool) (string, error) {
	dest := filepath.Join(a.releasesDir(), ver)
	if !force && validRelease(dest) {
		return dest, nil
	}
	if dirExists(dest) {
		if force || os.RemoveAll(dest) != nil {
			dest += "-" + time.Now().Format("20060102-150405")
		}
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/zapret-discord-youtube-%s.zip", a.Cfg.Repo, ver, ver)
	a.Log.Infof("скачиваю %s", url)
	data, err := httpGet(ctx, url, 100<<20)
	if err != nil {
		return "", fmt.Errorf("не скачать релиз %s: %w", ver, err)
	}
	tmpZip := a.path("tmp", "zapret-"+ver+".zip")
	if err := os.MkdirAll(filepath.Dir(tmpZip), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(tmpZip, data, 0o644); err != nil {
		return "", err
	}
	defer os.Remove(tmpZip)
	tmpDir := dest + ".new"
	_ = os.RemoveAll(tmpDir)
	if err := extractZip(tmpZip, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("архив релиза %s битый: %w", ver, err)
	}
	if !validRelease(tmpDir) {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("в архиве %s нет bin\\winws.exe, WinDivert или стратегий", ver)
	}
	if err := os.Rename(tmpDir, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// extractZip распаковывает архив, срезая общую верхнюю папку (zapret-discord-youtube-X/).
// Пути с .. и абсолютные отвергаются. CRC проверяет archive/zip при чтении.
func extractZip(zipPath, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	prefix := commonZipPrefix(zr.File)
	for _, f := range zr.File {
		name := strings.TrimPrefix(strings.ReplaceAll(f.Name, `\`, "/"), prefix)
		name = strings.Trim(name, "/")
		if name == "" {
			continue
		}
		if strings.Contains(name, ":") {
			return fmt.Errorf("подозрительный путь в архиве: %s", f.Name)
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return fmt.Errorf("подозрительный путь в архиве: %s", f.Name)
			}
		}
		target := filepath.Join(dest, filepath.FromSlash(path.Clean(name)))
		if !strings.HasPrefix(strings.ToLower(target), strings.ToLower(filepath.Clean(dest)+string(os.PathSeparator))) {
			return fmt.Errorf("путь вне папки распаковки: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func extractZipFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, 200<<20)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func commonZipPrefix(files []*zip.File) string {
	var top string
	for _, f := range files {
		name := strings.TrimPrefix(strings.ReplaceAll(f.Name, `\`, "/"), "/")
		first, _, found := strings.Cut(name, "/")
		if !found {
			return "" // файл в корне архива — срезать нечего
		}
		if top == "" {
			top = first
		} else if top != first {
			return ""
		}
	}
	if top == "" {
		return ""
	}
	return top + "/"
}

// migrateUserFiles переносит в новый релиз то, что пользователь настраивал в старом:
// свои списки, game filter и режим ipset (как и делают руками при обновлении).
func migrateUserFiles(oldDir, newDir string) {
	for _, rel := range []string{
		`lists\list-general-user.txt`,
		`lists\list-exclude-user.txt`,
		`lists\ipset-exclude-user.txt`,
		`utils\game_filter.enabled`,
	} {
		src := filepath.Join(oldDir, rel)
		if fileExists(src) {
			_ = copyFile(src, filepath.Join(newDir, rel))
		}
	}
	ipsetPath := filepath.Join(newDir, "lists", "ipset-all.txt")
	switch ipsetMode(oldDir) {
	case "any":
		_ = os.WriteFile(ipsetPath, nil, 0o644)
	case "none":
		_ = os.WriteFile(ipsetPath, []byte(ipsetNoneMarker+"\r\n"), 0o644)
	case "loaded":
		_ = copyFile(filepath.Join(oldDir, "lists", "ipset-all.txt"), ipsetPath)
	}
}

// pruneReleases оставляет KeepVersions свежих релизов и текущий.
func (a *App) pruneReleases(current string) {
	entries, err := os.ReadDir(a.releasesDir())
	if err != nil {
		return
	}
	var vers []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".new") || strings.HasSuffix(name, ".old") {
			_ = os.RemoveAll(filepath.Join(a.releasesDir(), name))
			continue
		}
		vers = append(vers, name)
	}
	sort.Slice(vers, func(i, j int) bool { return compareVersions(vers[i], vers[j]) > 0 })
	for i, v := range vers {
		dir := filepath.Join(a.releasesDir(), v)
		if i < a.Cfg.KeepVersions || samePath(dir, current) {
			continue
		}
		if err := os.RemoveAll(dir); err == nil {
			a.Log.Infof("удалил старый релиз %s", v)
		}
	}
}

// ---------- ipset и списки ----------

func cleanLines(data []byte) []string {
	text := strings.ReplaceAll(string(stripBOM(data)), "\r", "")
	var out []string
	for _, l := range strings.Split(text, "\n") {
		out = append(out, strings.TrimSpace(l))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func validateIpset(lines []string) error {
	total, good := 0, 0
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		total++
		if _, _, err := net.ParseCIDR(l); err == nil || net.ParseIP(l) != nil {
			good++
		}
	}
	if total < 1000 {
		return fmt.Errorf("в ipset всего %d строк — похоже на ошибку", total)
	}
	if good*100 < total*95 {
		return fmt.Errorf("в ipset %d из %d строк не похожи на IP/подсеть", total-good, total)
	}
	return nil
}

// updateIpset кладёт свежий .service/ipset-service.txt. Режим не меняется: при loaded
// пишется ipset-all.txt, при none/any — ipset-all.txt.backup (его поднимет service.bat
// при включении loaded). winws перечитывает файл сам при изменении.
func (a *App) updateIpset(ctx context.Context, relDir string) (bool, error) {
	data, err := httpGet(ctx, a.rawURL(".service/ipset-service.txt"), 32<<20)
	if err != nil {
		return false, err
	}
	lines := cleanLines(data)
	if err := validateIpset(lines); err != nil {
		return false, err
	}
	target := filepath.Join(relDir, "lists", "ipset-all.txt")
	if ipsetMode(relDir) != "loaded" {
		target += ".backup"
	}
	content := strings.Join(lines, "\r\n") + "\r\n"
	if old, err := os.ReadFile(target); err == nil && normalizeText(string(old)) == normalizeText(content) {
		return false, nil
	}
	if err := writeFileAtomic(target, []byte(content)); err != nil {
		return false, err
	}
	return true, nil
}

var domainLineRe = regexp.MustCompile(`^\^?[A-Za-z0-9*_.\-]+$`)

// mainLists — списки, которые Flowseal правит в main между релизами.
var mainLists = []string{"list-general.txt", "list-google.txt", "list-exclude.txt", "ipset-exclude.txt"}

func (a *App) updateMainLists(ctx context.Context, relDir string) ([]string, error) {
	var changed []string
	var errs []string
	for _, name := range mainLists {
		data, err := httpGet(ctx, a.rawURL("lists/"+name), 4<<20)
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		lines := cleanLines(data)
		if err := validateList(name, lines); err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		target := filepath.Join(relDir, "lists", name)
		old, _ := os.ReadFile(target)
		content := strings.Join(lines, "\r\n") + "\r\n"
		if normalizeText(string(old)) == normalizeText(content) {
			continue
		}
		// защита от обрезанного файла: новый список не может быть меньше половины старого
		if oldLines := len(cleanLines(old)); oldLines > 10 && len(lines)*2 < oldLines {
			errs = append(errs, fmt.Sprintf("%s: в main %d строк против %d у нас — пропускаю", name, len(lines), oldLines))
			continue
		}
		if err := writeFileAtomic(target, []byte(content)); err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		changed = append(changed, name)
	}
	if len(errs) > 0 {
		return changed, errors.New(strings.Join(errs, "; "))
	}
	return changed, nil
}

func validateList(name string, lines []string) error {
	n := 0
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		n++
		if strings.HasPrefix(name, "ipset") {
			if _, _, err := net.ParseCIDR(l); err != nil && net.ParseIP(l) == nil {
				return fmt.Errorf("строка %q не IP", l)
			}
			continue
		}
		if !domainLineRe.MatchString(l) {
			return fmt.Errorf("строка %q не похожа на домен", l)
		}
	}
	if n == 0 {
		return errors.New("пустой список")
	}
	return nil
}
