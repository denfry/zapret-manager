package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"zapret-manager/internal/safezip"
	"zapret-manager/internal/textutil"
	"zapret-manager/internal/version"
	"zapret-manager/internal/zapretfmt"
)

// ---------- HTTP для скачивания ----------

// Лимиты на то, что приходит из репозитория.
const (
	maxReleaseZip  = 100 << 20 // релиз весит несколько МБ
	maxIpsetBytes  = 32 << 20
	maxListBytes   = 4 << 20
	maxHostsBytes  = 1 << 20
	maxVersionText = 256
	maxRedirects   = 10
)

var errTooLarge = errors.New("ответ больше лимита")

// httpStatusError — ответ не 200.
type httpStatusError struct {
	URL  string
	Code int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.URL, e.Code) }

// downloader скачивает файлы из GitHub с ограниченными повторами. Повторяются только
// временные ошибки (сеть, 408, 425, 429, 5xx); 404 и прочие 4xx — сразу ошибка.
type downloader struct {
	client   *http.Client
	attempts int
	backoff  time.Duration // первая пауза; дальше ×2 плюс случайная добавка до backoff/2
	stall    time.Duration // сколько ждать следующих байт тела; 0 — defaultStall
}

const (
	// defaultStall — тело перестало приходить: соединение считаем зависшим.
	defaultStall = time.Minute
	// minRate — скорость, ниже которой ответ целиком уже не ждём: общий срок запроса
	// считается от размера лимита (релиз 100 МБ — около получаса), а не один на всё,
	// иначе на медленном канале релиз обрывался бы всегда в одном месте.
	minRate = 64 << 10
	// baseRequestTimeout — на соединение, заголовки и небольшие файлы.
	baseRequestTimeout = 3 * time.Minute
)

var (
	errStalled        = errors.New("загрузка встала: данные не приходят")
	errUnsafeRedirect = errors.New("небезопасный редирект")
	errSink           = errors.New("не записать скачанное")
)

func newDownloader() *downloader {
	return &downloader{
		client: &http.Client{
			// без общего Timeout: срок запроса задаёт fetchOnce по размеру ответа
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConns:          4,
				ForceAttemptHTTP2:     true,
			},
			CheckRedirect: httpsOnlyRedirect,
		},
		attempts: 3,
		backoff:  2 * time.Second,
	}
}

// defaultDownloader общий для процесса: один пул соединений на все запросы.
var defaultDownloader = newDownloader()

// httpsOnlyRedirect не даёт GitHub-редиректу увести запрос на http:// и ограничивает цепочку.
func httpsOnlyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w: больше %d редиректов", errUnsafeRedirect, maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w: адрес %s отклонён", errUnsafeRedirect, req.URL.Redacted())
	}
	return nil
}

func retryable(err error) bool {
	if se, ok := errors.AsType[*httpStatusError](err); ok {
		return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooEarly ||
			se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	// Не повторяем то, что повтор не исправит: лимит, отклонённый редирект, неверный
	// сертификат, ошибку записи на диск. Остальное — сбои сети, их повторяем.
	if errors.Is(err, errTooLarge) || errors.Is(err, errUnsafeRedirect) || errors.Is(err, errSink) {
		return false
	}
	var certErr *tls.CertificateVerificationError
	var uaErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invErr x509.CertificateInvalidError
	return !errors.As(err, &certErr) && !errors.As(err, &uaErr) && !errors.As(err, &hostErr) && !errors.As(err, &invErr)
}

// requestTimeout — общий срок одного запроса с телом не больше limit байт.
func requestTimeout(limit int64) time.Duration {
	return baseRequestTimeout + time.Duration(limit/minRate)*time.Second
}

// stallReader отменяет запрос, если между порциями тела прошло больше stall.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	stall time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.stall)
	}
	return n, err
}

func (d *downloader) delay(attempt int) time.Duration {
	if d.backoff <= 0 {
		return 0
	}
	base := d.backoff << (attempt - 1)
	return base + rand.N(d.backoff/2+1) // #nosec G404 -- retry jitter, not security-relevant
}

// fetch выполняет GET и отдаёт тело в sink (не больше limit байт). sink вызывается
// заново на каждой попытке и должен начинать с чистого листа.
func (d *downloader) fetch(ctx context.Context, url string, limit int64, sink func(io.Reader) error) error {
	var lastErr error
	for attempt := range d.attempts {
		if attempt > 0 {
			if err := sleepCtx(ctx, d.delay(attempt)); err != nil {
				return err
			}
		}
		err := d.fetchOnce(ctx, url, limit, sink)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil || !retryable(err) {
			return err
		}
	}
	return lastErr
}

func (d *downloader) fetchOnce(ctx context.Context, url string, limit int64, sink func(io.Reader) error) error {
	ctx, cancelTimeout := context.WithTimeout(ctx, requestTimeout(limit))
	defer cancelTimeout()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := d.stall
	if stall <= 0 {
		stall = defaultStall
	}
	timer := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer timer.Stop()
	err := d.fetchBody(ctx, url, limit, timer, stall, sink)
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		return fmt.Errorf("%s: %w", url, errStalled)
	}
	return err
}

func (d *downloader) fetchBody(ctx context.Context, url string, limit int64, timer *time.Timer, stall time.Duration, sink func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "zapret-manager/"+appVersion)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // дать соединению вернуться в пул
		return &httpStatusError{URL: url, Code: resp.StatusCode}
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("%s: %w (%d > %d байт)", url, errTooLarge, resp.ContentLength, limit)
	}
	timer.Reset(stall)
	cr := &countingReader{r: &stallReader{r: io.LimitReader(resp.Body, limit+1), timer: timer, stall: stall}}
	if err := sink(cr); err != nil {
		if cr.err != nil {
			return err // оборвалось чтение из сети — это можно повторить
		}
		return fmt.Errorf("%w: %w", errSink, err)
	}
	if cr.n > limit {
		return fmt.Errorf("%s: %w (%d байт)", url, errTooLarge, limit)
	}
	return nil
}

type countingReader struct {
	r   io.Reader
	n   int64
	err error // последняя ошибка чтения, кроме EOF: отличает сбой сети от сбоя sink
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

// get скачивает небольшой файл в память.
func (d *downloader) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	err := d.fetch(ctx, url, limit, func(r io.Reader) error {
		buf.Reset()
		_, err := buf.ReadFrom(r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// download пишет файл потоком на диск (релиз не держим целиком в памяти).
func (d *downloader) download(ctx context.Context, url, dst string, limit int64) error {
	err := d.fetch(ctx, url, limit, func(r io.Reader) error {
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

func (a *App) dl() *downloader {
	if a.downloader != nil {
		return a.downloader
	}
	return defaultDownloader
}

// Адреса GitHub. App.rawBase/App.githubBase подменяют их в тестах.
const (
	defaultRawBase    = "https://raw.githubusercontent.com"
	defaultGithubBase = "https://github.com"
)

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// rawURL — файл из ветки репозитория; ?t= обходит кэш CDN raw.githubusercontent.com.
func (a *App) rawURL(file string) string {
	return fmt.Sprintf("%s/%s/%s/%s?t=%d", orDefault(a.rawBase, defaultRawBase), a.Cfg.Repo, a.Cfg.Branch, file, time.Now().Unix())
}

func (a *App) releaseURL(ver string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/zapret-discord-youtube-%s.zip",
		orDefault(a.githubBase, defaultGithubBase), a.Cfg.Repo, ver, ver)
}

// ---------- версии ----------

func releaseVersion(relDir string) string {
	if data, err := os.ReadFile(filepath.Join(relDir, "service.bat")); err == nil {
		if v, ok := version.FromServiceBat(data); ok {
			return v
		}
	}
	return version.FromDirName(filepath.Base(relDir))
}

func (a *App) fetchLatestVersion(ctx context.Context) (string, error) {
	data, err := a.dl().get(ctx, a.rawURL(".service/version.txt"), maxVersionText)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(textutil.StripBOM(data)))
	if !version.Valid(v) {
		return "", fmt.Errorf("странная версия в version.txt: %q", v)
	}
	return v, nil
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

// downloadRelease скачивает zip релиза в releases\<версия>. Уже скачанный и целый
// не трогает (если не force). Существующую папку никогда не удаляет: в ней может
// работать winws и лежать *-user.txt — новый релиз кладётся рядом в
// releases\<версия>-<время>, старый уберёт pruneReleases.
func (a *App) downloadRelease(ctx context.Context, ver string, force bool) (string, error) {
	if !version.Valid(ver) {
		return "", fmt.Errorf("странная версия %q", ver)
	}
	dest := filepath.Join(a.releasesDir(), ver)
	if !force && validRelease(dest) {
		return dest, nil
	}
	if dirExists(dest) {
		dest += "-" + time.Now().Format("20060102-150405")
	}
	url := a.releaseURL(ver)
	a.Log.Infof("скачиваю %s", url)
	tmpDir := a.path("tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	tmpZip := filepath.Join(tmpDir, "zapret-"+ver+".zip")
	if err := a.dl().download(ctx, url, tmpZip, maxReleaseZip); err != nil {
		return "", fmt.Errorf("не скачать релиз %s: %w", ver, err)
	}
	defer os.Remove(tmpZip) //nolint:errcheck // temp file
	staging := dest + ".new"
	_ = os.RemoveAll(staging)
	if err := safezip.Extract(tmpZip, staging, safezip.DefaultLimits); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("архив релиза %s битый: %w", ver, err)
	}
	if !validRelease(staging) {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("в архиве %s нет bin\\winws.exe, WinDivert или стратегий", ver)
	}
	if err := os.Rename(staging, dest); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return dest, nil
}

// migrateUserFiles переносит в новый релиз то, что пользователь настраивал в старом:
// свои списки, game filter и режим ipset (как и делают руками при обновлении).
func migrateUserFiles(oldDir, newDir string) error {
	var errs []error
	for _, rel := range []string{
		`lists\list-general-user.txt`,
		`lists\list-exclude-user.txt`,
		`lists\ipset-exclude-user.txt`,
		`utils\game_filter.enabled`,
	} {
		src := filepath.Join(oldDir, rel)
		if fileExists(src) {
			if err := copyFile(src, filepath.Join(newDir, rel)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	ipsetPath := filepath.Join(newDir, "lists", "ipset-all.txt")
	var err error
	switch ipsetMode(oldDir) {
	case zapretfmt.IpsetAny:
		err = os.WriteFile(ipsetPath, nil, 0o644)
	case zapretfmt.IpsetNone:
		err = os.WriteFile(ipsetPath, []byte(zapretfmt.IpsetNoneMarker+"\r\n"), 0o644)
	case zapretfmt.IpsetLoaded:
		err = copyFile(filepath.Join(oldDir, "lists", "ipset-all.txt"), ipsetPath)
	}
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
		// трогаем только свои папки: releases\<версия>[-время] и их .new/.old;
		// всё остальное (папку, положенную руками) не удаляем
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".new"), ".old")
		if !version.Valid(base) {
			continue
		}
		if base != name {
			_ = os.RemoveAll(filepath.Join(a.releasesDir(), name)) // best effort: недокачанная или старая копия
			continue
		}
		vers = append(vers, name)
	}
	sort.Slice(vers, func(i, j int) bool { return version.Compare(vers[i], vers[j]) > 0 })
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

// updateIpset кладёт свежий .service/ipset-service.txt. Режим не меняется: при loaded
// пишется ipset-all.txt, при none/any — ipset-all.txt.backup (его поднимет service.bat
// при включении loaded). winws перечитывает файл сам при изменении.
func (a *App) updateIpset(ctx context.Context, relDir string) (bool, error) {
	data, err := a.dl().get(ctx, a.rawURL(".service/ipset-service.txt"), maxIpsetBytes)
	if err != nil {
		return false, err
	}
	lines, err := zapretfmt.ValidateIpset(textutil.CleanLines(data))
	if err != nil {
		return false, err
	}
	target := filepath.Join(relDir, "lists", "ipset-all.txt")
	if ipsetMode(relDir) != zapretfmt.IpsetLoaded {
		target += ".backup"
	}
	content := strings.Join(lines, "\r\n") + "\r\n"
	if old, err := os.ReadFile(target); err == nil && textutil.NormalizeText(string(old)) == textutil.NormalizeText(content) {
		return false, nil
	}
	if err := writeFileAtomic(target, []byte(content)); err != nil {
		return false, err
	}
	return true, nil
}

// mainLists — списки, которые Flowseal правит в main между релизами.
var mainLists = []string{"list-general.txt", "list-google.txt", "list-exclude.txt", "ipset-exclude.txt"}

func (a *App) updateMainLists(ctx context.Context, relDir string) ([]string, error) {
	var changed []string
	var errs []string
	for _, name := range mainLists {
		data, err := a.dl().get(ctx, a.rawURL("lists/"+name), maxListBytes)
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		lines := textutil.CleanLines(data)
		if err := zapretfmt.ValidateList(name, lines); err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		target := filepath.Join(relDir, "lists", name)
		old, err := os.ReadFile(target)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		content := strings.Join(lines, "\r\n") + "\r\n"
		if textutil.NormalizeText(string(old)) == textutil.NormalizeText(content) {
			continue
		}
		// защита от обрезанного файла: новый список не может быть меньше половины старого
		if oldLines := len(textutil.CleanLines(old)); oldLines > 10 && len(lines)*2 < oldLines {
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
