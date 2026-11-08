package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zapret-manager/internal/zapretfmt"
)

// fakeGitHub — локальный «GitHub» с релизом Flowseal: version.txt, zip релиза,
// ipset и списки из ветки main. Все пути — как у настоящего repo по умолчанию.
type fakeGitHub struct {
	srv        *httptest.Server
	mu         sync.Mutex
	files      map[string][]byte // путь → тело
	zipHits    atomic.Int32
	versionTxt string
}

const repoPath = "/Flowseal/zapret-discord-youtube"

func releaseZip(t *testing.T, ver string, extra map[string]string) []byte {
	t.Helper()
	files := map[string]string{
		"service.bat":               "@echo off\r\nset \"LOCAL_VERSION=" + ver + "\"\r\n",
		"bin/winws.exe":             "MZ",
		"bin/WinDivert.dll":         "MZ",
		"bin/WinDivert64.sys":       "MZ",
		"lists/list-general.txt":    "discord.com\r\n",
		"lists/ipset-all.txt":       "1.1.1.0/24\r\n",
		"general.bat":               "start \"\" \"%BIN%winws.exe\" --wf-tcp=443\r\n",
		"general (ALT).bat":         "start \"\" \"%BIN%winws.exe\" --wf-tcp=80,443\r\n",
		"utils/game_filter.example": "",
	}
	maps.Copy(files, extra)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create("zapret-discord-youtube-" + ver + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func ipsetBody(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "10.%d.%d.0/24\n", i/256, i%256)
	}
	return b.String()
}

func newFakeGitHub(t *testing.T, ver string) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{files: map[string][]byte{}, versionTxt: ver + "\n"}
	g.files[repoPath+"/releases/download/"+ver+"/zapret-discord-youtube-"+ver+".zip"] = releaseZip(t, ver, nil)
	g.files[repoPath+"/main/.service/ipset-service.txt"] = []byte(ipsetBody(1200))
	g.files[repoPath+"/main/lists/list-general.txt"] = []byte("discord.com\ndiscord.gg\n")
	g.files[repoPath+"/main/lists/list-google.txt"] = []byte("youtube.com\nytimg.com\n")
	g.files[repoPath+"/main/lists/list-exclude.txt"] = []byte("ya.ru\n")
	g.files[repoPath+"/main/lists/ipset-exclude.txt"] = []byte("10.0.0.0/8\n")
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.URL.Path == repoPath+"/main/.service/version.txt" {
			_, _ = w.Write([]byte(g.versionTxt))
			return
		}
		body, ok := g.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".zip") {
			g.zipHits.Add(1)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) set(path string, body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[path] = body
}

func (g *fakeGitHub) app(t *testing.T) *App {
	t.Helper()
	a := testApp(t)
	a.downloader = testDownloader(g.srv, time.Millisecond)
	a.rawBase, a.githubBase = g.srv.URL, g.srv.URL
	return a
}

// makeOldRelease — установленный релиз со своими настройками пользователя.
func makeOldRelease(t *testing.T, a *App, ver string) string {
	t.Helper()
	dir := filepath.Join(a.releasesDir(), ver)
	files := map[string]string{
		"service.bat":                 "set \"LOCAL_VERSION=" + ver + "\"\r\n",
		`bin\winws.exe`:               "MZ",
		`bin\WinDivert.dll`:           "MZ",
		`bin\WinDivert64.sys`:         "MZ",
		"general.bat":                 "x",
		`lists\list-general-user.txt`: "my.domain\r\n",
		`lists\list-exclude-user.txt`: "skip.me\r\n",
		`lists\ipset-all.txt`:         "", // режим any
		`lists\list-google.txt`:       strings.Repeat("a.example\r\n", 30),
		`utils\game_filter.enabled`:   "mode=tcp\r\ntcp=443\r\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestUpdateFlowOffline(t *testing.T) {
	g := newFakeGitHub(t, "1.10.5")
	a := g.app(t)
	ctx := context.Background()
	oldDir := makeOldRelease(t, a, "1.10.4")

	ver, err := a.fetchLatestVersion(ctx)
	if err != nil || ver != "1.10.5" {
		t.Fatalf("%q %v", ver, err)
	}

	newDir, err := a.downloadRelease(ctx, ver, false)
	if err != nil {
		t.Fatal(err)
	}
	if !validRelease(newDir) || releaseVersion(newDir) != "1.10.5" || filepath.Base(newDir) != "1.10.5" {
		t.Fatalf("релиз %s: valid=%v ver=%s", newDir, validRelease(newDir), releaseVersion(newDir))
	}
	// уже скачанный целый релиз повторно не качается
	if again, err := a.downloadRelease(ctx, ver, false); err != nil || again != newDir || g.zipHits.Load() != 1 {
		t.Fatalf("повторное скачивание: %s %v hits=%d", again, err, g.zipHits.Load())
	}

	if err := migrateUserFiles(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(newDir, "lists", "list-general-user.txt")); string(data) != "my.domain\r\n" {
		t.Fatalf("свой список не перенесён: %q", data)
	}
	if gf := readGameFilter(newDir); gf.Mode != zapretfmt.ModeTCP || gf.TCPRange != "443" {
		t.Fatalf("game filter: %+v", gf)
	}
	if m := ipsetMode(newDir); m != zapretfmt.IpsetAny {
		t.Fatalf("режим ipset %s, want any", m)
	}

	// режим any: свежий ipset кладётся в .backup, рабочий файл не трогается
	if changed, err := a.updateIpset(ctx, newDir); err != nil || !changed {
		t.Fatalf("ipset: %v %v", changed, err)
	}
	if ipsetMode(newDir) != zapretfmt.IpsetAny || !fileExists(filepath.Join(newDir, "lists", "ipset-all.txt.backup")) {
		t.Fatal("updateIpset сменил режим или не создал .backup")
	}
	if changed, _ := a.updateIpset(ctx, newDir); changed {
		t.Fatal("повторное обновление ipset без изменений должно возвращать false")
	}

	changed, err := a.updateMainLists(ctx, newDir)
	if err != nil || strings.Join(changed, ",") != "list-general.txt,list-google.txt,list-exclude.txt,ipset-exclude.txt" {
		t.Fatalf("списки: %v %v", changed, err)
	}
	if changed, err := a.updateMainLists(ctx, newDir); err != nil || len(changed) != 0 {
		t.Fatalf("повторно: %v %v", changed, err)
	}
	// в main список резко «похудел» (обрезанный ответ) — у нас 30 строк, там 1: не берём
	_ = os.WriteFile(filepath.Join(newDir, "lists", "list-google.txt"), []byte(strings.Repeat("b.example\r\n", 30)), 0o644)
	g.set(repoPath+"/main/lists/list-google.txt", []byte("only.one\n"))
	if changed, err := a.updateMainLists(ctx, newDir); err == nil || len(changed) != 0 {
		t.Fatalf("усохший список должен пропускаться: %v %v", changed, err)
	}
	// HTML вместо списка не принимается
	g.set(repoPath+"/main/lists/list-exclude.txt", []byte("<!DOCTYPE html><html>"))
	if _, err := a.updateMainLists(ctx, newDir); err == nil || !strings.Contains(err.Error(), "list-exclude.txt") {
		t.Fatalf("HTML должен отвергаться: %v", err)
	}

	// force кладёт рядом и не трогает работающую папку
	forced, err := a.downloadRelease(ctx, ver, true)
	if err != nil || samePath(forced, newDir) || !strings.HasPrefix(filepath.Base(forced), "1.10.5-") || !validRelease(newDir) {
		t.Fatalf("force: %s %v", forced, err)
	}

	// pruneReleases: остаются KeepVersions свежих и текущий, мусор .new удаляется
	if err := os.MkdirAll(filepath.Join(a.releasesDir(), "1.10.5.new"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Cfg.KeepVersions = 1
	a.pruneReleases(oldDir)
	left, _ := os.ReadDir(a.releasesDir())
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !dirExists(oldDir) || !dirExists(forced) {
		t.Fatalf("после очистки: %v", names)
	}
}

func TestDownloadReleaseRejectsBadArchives(t *testing.T) {
	g := newFakeGitHub(t, "2.0")
	a := g.app(t)
	ctx := context.Background()
	zipPath := repoPath + "/releases/download/2.0/zapret-discord-youtube-2.0.zip"

	g.set(zipPath, []byte("<html>rate limited</html>"))
	if _, err := a.downloadRelease(ctx, "2.0", false); err == nil || !strings.Contains(err.Error(), "битый") {
		t.Fatalf("битый архив: %v", err)
	}
	g.set(zipPath, releaseZip(t, "2.0", map[string]string{"../../evil.txt": "x"}))
	if _, err := a.downloadRelease(ctx, "2.0", false); err == nil {
		t.Fatal("архив с выходом за папку должен отвергаться")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("top/readme.txt")
	_, _ = w.Write([]byte("no winws here"))
	_ = zw.Close()
	g.set(zipPath, buf.Bytes())
	if _, err := a.downloadRelease(ctx, "2.0", false); err == nil || !strings.Contains(err.Error(), "winws") {
		t.Fatalf("архив без winws: %v", err)
	}
	if entries, _ := os.ReadDir(a.releasesDir()); len(entries) != 0 {
		t.Fatalf("после неудач в releases остался мусор: %v", entries)
	}
	if _, err := a.downloadRelease(ctx, "../evil", false); err == nil {
		t.Fatal("версия с путём должна отвергаться")
	}
}

func TestFetchLatestVersionRejectsGarbage(t *testing.T) {
	g := newFakeGitHub(t, "1.0")
	a := g.app(t)
	for _, body := range []string{"<html>", "1.0/../../x", ""} {
		g.mu.Lock()
		g.versionTxt = body
		g.mu.Unlock()
		if v, err := a.fetchLatestVersion(context.Background()); err == nil {
			t.Errorf("%q принят как версия %q", body, v)
		}
	}
}
