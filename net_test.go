package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Сетевой прогон обновлений во временную папку (без админа, без записи в настоящий hosts).
// Запуск: ZM_NET=1 go test -run TestNetworkUpdateFlow -v
func TestNetworkUpdateFlow(t *testing.T) {
	if os.Getenv("ZM_NET") == "" {
		t.Skip("ZM_NET не задан")
	}
	ctx := context.Background()
	root := t.TempDir()
	a := &App{Root: root, Cfg: defaultConfig(), Log: &Logger{console: true}}
	ver, err := a.fetchLatestVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("последняя версия: %s", ver)
	dir, err := a.downloadRelease(ctx, ver, false)
	if err != nil {
		t.Fatal(err)
	}
	if !validRelease(dir) || releaseVersion(dir) != ver {
		t.Fatalf("релиз %s невалиден или версия %s", dir, releaseVersion(dir))
	}
	t.Logf("релиз в %s, ipset в архиве: %s", dir, ipsetMode(dir))
	home, _ := os.UserHomeDir()
	old := filepath.Join(home, "Desktop", "zapret-discord-youtube-1.10.4")
	if validRelease(old) {
		migrateUserFiles(old, dir)
		t.Logf("после переноса: game filter %s, ipset %s", readGameFilter(dir), ipsetMode(dir))
		if readGameFilter(dir) != readGameFilter(old) || ipsetMode(dir) != ipsetMode(old) {
			t.Fatal("настройки не перенеслись")
		}
	}
	ensureUserLists(dir)
	strategies, _ := a.listAllStrategies(dir)
	for _, s := range strategies {
		if _, err := buildCommandLine(dir, s, readGameFilter(dir)); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
	}
	t.Logf("стратегий: %d", len(strategies))
	changed, err := a.updateIpset(ctx, dir)
	t.Logf("ipset: changed=%v err=%v строк=%d", changed, err, countIpsetLines(dir))
	if err != nil {
		t.Fatal(err)
	}
	lists, err := a.updateMainLists(ctx, dir)
	t.Logf("списки из main: %v err=%v", lists, err)
	data, err := httpGet(ctx, a.rawURL(".service/hosts"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	up, err := parseUpstreamHosts(data)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := os.ReadFile(hostsPath())
	merged, ch := mergeHosts(string(local), up)
	t.Logf("hosts: изменится=%v, строк было %d, станет %d", ch, strings.Count(string(local), "\n"), strings.Count(merged, "\n"))
	if strings.Count(merged, "140.82.121.3 github.com") != 1 {
		t.Fatal("github.com продублирован")
	}
	if out := os.Getenv("ZM_OUT"); out != "" {
		os.WriteFile(filepath.Join(out, "hosts.preview"), []byte(merged), 0o644)
	}
	again, ch2 := mergeHosts(merged, up)
	if ch2 || again != merged {
		t.Fatal("слияние не идемпотентно на настоящем hosts")
	}
}
