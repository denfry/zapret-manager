package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const sampleBat = "@echo off\r\nchcp 65001 > nul\r\n:: 65001 - UTF-8\r\n\r\ncd /d \"%~dp0\"\r\ncall service.bat status_zapret\r\n" +
	"set \"BIN=%~dp0bin\\\"\r\nset \"LISTS=%~dp0lists\\\"\r\ncd /d %BIN%\r\n\r\n" +
	"start \"zapret: %~n0\" /min \"%BIN%winws.exe\" --wf-tcp=80,443,%GameFilterTCP% --wf-udp=443,%GameFilterUDP% ^\r\n" +
	"--filter-udp=443 --hostlist=\"%LISTS%list-general.txt\" --dpi-desync=fake --dpi-desync-fake-quic=\"%BIN%quic_initial_www_google_com.bin\" --new ^\r\n" +
	"--filter-tcp=443 --dpi-desync-fake-tls=0x00000000 --dpi-desync-fake-tls=^! --dpi-desync-fake-tls-mod=rnd,dupsid,sni=www.google.com --new ^\r\n" +
	"--filter-tcp=%GameFilterTCP% --ipset=\"%LISTS%ipset-all.txt\" --dpi-desync=multisplit\r\n"

func TestExtractWinwsArgs(t *testing.T) {
	vars := map[string]string{
		"bin": `C:\zapret\releases\1.10.4\bin\`, "lists": `C:\zapret\releases\1.10.4\lists\`,
		"gamefilter": "1024-65535", "gamefiltertcp": "1024-65535", "gamefilterudp": "12",
	}
	args, err := extractWinwsArgs(sampleBat, vars, `C:\zapret\releases\1.10.4\`)
	if err != nil {
		t.Fatal(err)
	}
	want := `--wf-tcp=80,443,1024-65535 --wf-udp=443,12 ` +
		`--filter-udp=443 --hostlist="C:\zapret\releases\1.10.4\lists\list-general.txt" --dpi-desync=fake --dpi-desync-fake-quic="C:\zapret\releases\1.10.4\bin\quic_initial_www_google_com.bin" --new ` +
		`--filter-tcp=443 --dpi-desync-fake-tls=0x00000000 --dpi-desync-fake-tls=! --dpi-desync-fake-tls-mod=rnd,dupsid,sni=www.google.com --new ` +
		`--filter-tcp=1024-65535 --ipset="C:\zapret\releases\1.10.4\lists\ipset-all.txt" --dpi-desync=multisplit`
	if args != want {
		t.Fatalf("args:\n got %s\nwant %s", args, want)
	}
}

func TestExtractWinwsArgsUnknownVar(t *testing.T) {
	_, err := extractWinwsArgs(`start "" "%BIN%winws.exe" --wf-tcp=%Nope% --new`, map[string]string{"bin": `C:\b\`}, `C:\`)
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("Р¶РґР°Р» РѕС€РёР±РєСѓ РїСЂРѕ %%Nope%%, РїРѕР»СѓС‡РёР» %v", err)
	}
}

func TestUnescapeCaretsInQuotes(t *testing.T) {
	if got := unescapeCarets(`a^&b "x^y" ^^`); got != `a&b "x^y" ^` {
		t.Fatalf("got %q", got)
	}
}

func TestContinuationWithEscapedCaret(t *testing.T) {
	content := "start \"\" \"x\\winws.exe\" --a=1 ^^\r\n--b=2\r\n"
	args, err := extractWinwsArgs(content, map[string]string{}, `C:\`)
	if err != nil {
		t.Fatal(err)
	}
	if args != "--a=1 ^" {
		t.Fatalf("^^ РІ РєРѕРЅС†Рµ вЂ” РЅРµ РїРµСЂРµРЅРѕСЃ, got %q", args)
	}
}

// Р’СЃРµ РЅР°СЃС‚РѕСЏС‰РёРµ СЃС‚СЂР°С‚РµРіРёРё РёР· СЂРµР»РёР·Р° РґРѕР»Р¶РЅС‹ СЂР°Р·Р±РёСЂР°С‚СЊСЃСЏ Р±РµР· РѕС€РёР±РѕРє Рё Р±РµР· РѕСЃС‚Р°С‚РєРѕРІ %VAR%.
func TestRealStrategies(t *testing.T) {
	dir := os.Getenv("ZAPRET_TEST_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Desktop", "zapret-discord-youtube-1.10.4")
	}
	if !validRelease(dir) {
		t.Skip("РЅРµС‚ СЂР°СЃРїР°РєРѕРІР°РЅРЅРѕРіРѕ СЂРµР»РёР·Р° РґР»СЏ РїСЂРѕРІРµСЂРєРё:", dir)
	}
	list, err := listStrategies(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 10 {
		t.Fatalf("СЃС‚СЂР°С‚РµРіРёР№ РјР°Р»Рѕ: %d", len(list))
	}
	for _, gf := range []GameFilter{{Mode: "disabled"}, {Mode: "all", TCPRange: "1024-65535", UDPRange: "1024-65535"}} {
		for _, s := range list {
			cmd, err := buildCommandLine(dir, s, gf)
			if err != nil {
				t.Errorf("%s: %v", s.Name, err)
				continue
			}
			if strings.Contains(cmd, "%") || strings.Contains(cmd, "^") {
				t.Errorf("%s: РѕСЃС‚Р°Р»РёСЃСЊ %% РёР»Рё ^: %s", s.Name, cmd)
			}
			if !strings.HasPrefix(cmd, `"`+filepath.Join(dir, "bin", "winws.exe")+`" --wf-`) {
				t.Errorf("%s: СЃС‚СЂР°РЅРЅРѕРµ РЅР°С‡Р°Р»Рѕ: %.120s", s.Name, cmd)
			}
			if strings.Count(cmd, `"`)%2 != 0 {
				t.Errorf("%s: РЅРµРїР°СЂРЅС‹Рµ РєР°РІС‹С‡РєРё", s.Name)
			}
		}
	}
}

func TestNaturalOrder(t *testing.T) {
	if !(naturalKey("general (ALT2)") < naturalKey("general (ALT10)")) {
		t.Fatal("ALT2 РґРѕР»Р¶РЅР° РёРґС‚Рё СЂР°РЅСЊС€Рµ ALT10")
	}
}

func TestGameFilter(t *testing.T) {
	cases := []struct {
		in            string
		any, tcp, udp string
		mode          string
	}{
		{"mode=all\r\ntcp=1024-65535\r\nudp=1024-65535\r\n", "1024-65535", "1024-65535", "1024-65535", "all"},
		{"mode=tcp\ntcp=1024-1934,1936-65535\n", "1024-1934,1936-65535", "1024-1934,1936-65535", "12", "tcp"},
		{"mode=udp\nudp=5000\n", "5000", "12", "5000", "udp"},
		{"all\n", "1024-65535", "1024-65535", "1024-65535", "all"},
		{"mode=all\ntcp=70000\n", "1024-65535", "1024-65535", "1024-65535", "all"},
		{"mode=disabled\n", "12", "12", "12", "disabled"},
		{"ENABLED\n", "12", "12", "12", "disabled"},
	}
	for _, c := range cases {
		gf := parseGameFilter(c.in)
		a, tc, u := gf.Vars()
		if gf.Mode != c.mode || a != c.any || tc != c.tcp || u != c.udp {
			t.Errorf("%q: mode=%s vars=%s/%s/%s", c.in, gf.Mode, a, tc, u)
		}
	}
}

func TestValidatePortRanges(t *testing.T) {
	good := []string{"1024-65535", "80", "1024-1934,1936-65535", " 443 , 8443 "}
	bad := []string{"", "0", "65536", "100-50", "abc", "1-2-3", "123456"}
	for _, g := range good {
		if _, ok := validatePortRanges(g); !ok {
			t.Errorf("%q РґРѕР»Р¶РµРЅ Р±С‹С‚СЊ РІР°Р»РёРґРµРЅ", g)
		}
	}
	for _, b := range bad {
		if _, ok := validatePortRanges(b); ok {
			t.Errorf("%q РґРѕР»Р¶РµРЅ Р±С‹С‚СЊ РЅРµРІР°Р»РёРґРµРЅ", b)
		}
	}
}

func TestIpsetMode(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "lists"), 0o755)
	p := filepath.Join(dir, "lists", "ipset-all.txt")
	check := func(content, want string) {
		t.Helper()
		os.WriteFile(p, []byte(content), 0o644)
		if got := ipsetMode(dir); got != want {
			t.Errorf("%q: %s, want %s", content, got, want)
		}
	}
	check("", "any")
	check("\uFEFF\r\n", "any")
	check("203.0.113.113/32\r\n", "none")
	check("1.0.0.0/24\r\n1.1.1.0/24\r\n", "loaded")
}

const upstreamHosts = "140.82.121.3 github.com\n146.75.22.132 raw.githubusercontent.com\n\n149.154.167.220 t.me\n149.154.167.220 web.telegram.org\n\n162.159.138.232 discord.com\n162.159.137.232 discord.com\n"

func TestMergeHostsMigratesUnmarkedBlock(t *testing.T) {
	up, err := parseUpstreamHosts([]byte(upstreamHosts))
	if err != nil {
		t.Fatal(err)
	}
	local := "# Copyright (c) 1993-2009 Microsoft Corp.\r\n#\r\n127.0.0.1 my.local\r\n" +
		"140.82.121.3 github.com\r\n146.75.22.132 raw.githubusercontent.com\r\n\r\n" +
		"1.2.3.4 discord.com # СЃС‚Р°СЂС‹Р№ IP\r\n10.0.0.5 nas.home t.me\r\n"
	out, changed := mergeHosts(local, up)
	if !changed {
		t.Fatal("РґРѕР»Р¶РµРЅ РёР·РјРµРЅРёС‚СЊСЃСЏ")
	}
	if !strings.Contains(out, "127.0.0.1 my.local\r\n") || !strings.Contains(out, "# Copyright") {
		t.Fatal("С‡СѓР¶РёРµ Р·Р°РїРёСЃРё РїРѕС‚РµСЂСЏР»РёСЃСЊ:\n" + out)
	}
	if strings.Contains(out, "1.2.3.4 discord.com") {
		t.Fatal("СЃС‚Р°СЂР°СЏ Р·Р°РїРёСЃСЊ discord.com РѕСЃС‚Р°Р»Р°СЃСЊ")
	}
	if !strings.Contains(out, "10.0.0.5 nas.home\r\n") || strings.Contains(out, "nas.home t.me") {
		t.Fatal("t.me РґРѕР»Р¶РµРЅ СѓР№С‚Рё РёР· СЃРјРµС€Р°РЅРЅРѕР№ СЃС‚СЂРѕРєРё, nas.home РѕСЃС‚Р°С‚СЊСЃСЏ:\n" + out)
	}
	if strings.Count(out, "140.82.121.3 github.com") != 1 {
		t.Fatal("github.com РґРѕР»Р¶РµРЅ Р±С‹С‚СЊ СЂРѕРІРЅРѕ РѕРґРёРЅ СЂР°Р·:\n" + out)
	}
	if !strings.Contains(out, hostsBegin+"\r\n140.82.121.3 github.com") || !strings.HasSuffix(out, hostsEnd+"\r\n") {
		t.Fatal("Р±Р»РѕРє РЅРµ РІ РєРѕРЅС†Рµ:\n" + out)
	}
	again, changed2 := mergeHosts(out, up)
	if changed2 || again != out {
		t.Fatalf("РїРѕРІС‚РѕСЂРЅРѕРµ СЃР»РёСЏРЅРёРµ РґРѕР»Р¶РЅРѕ Р±С‹С‚СЊ Р±РµР· РёР·РјРµРЅРµРЅРёР№:\n%s\n---\n%s", out, again)
	}
}

func TestMergeHostsReplacesOwnBlockAndKeepsTail(t *testing.T) {
	up, _ := parseUpstreamHosts([]byte(upstreamHosts))
	local := "127.0.0.1 a\r\n\r\n" + hostsBegin + "\r\n9.9.9.9 discord.com\r\n" + hostsEnd + "\r\n192.168.1.1 router\r\n"
	out, _ := mergeHosts(local, up)
	if strings.Contains(out, "9.9.9.9") {
		t.Fatal("СЃС‚Р°СЂС‹Р№ Р±Р»РѕРє РЅРµ СѓРґР°Р»С‘РЅ")
	}
	if !strings.Contains(out, "192.168.1.1 router") {
		t.Fatal("Р·Р°РїРёСЃСЊ РїРѕСЃР»Рµ Р±Р»РѕРєР° РїРѕС‚РµСЂСЏР»Р°СЃСЊ")
	}
	if strings.Count(out, hostsBegin) != 1 {
		t.Fatal("РјР°СЂРєРµСЂ РґРѕР»Р¶РµРЅ Р±С‹С‚СЊ РѕРґРёРЅ")
	}
}

func TestMergeHostsBrokenMarkerKeepsEntries(t *testing.T) {
	up, _ := parseUpstreamHosts([]byte(upstreamHosts))
	local := hostsBegin + "\r\n192.168.1.1 router\r\n"
	out, _ := mergeHosts(local, up)
	if !strings.Contains(out, "192.168.1.1 router") {
		t.Fatal("Р±РµР· Р·Р°РєСЂС‹РІР°СЋС‰РµРіРѕ РјР°СЂРєРµСЂР° РЅРёС‡РµРіРѕ СѓРґР°Р»СЏС‚СЊ РЅРµР»СЊР·СЏ")
	}
}

func TestParseUpstreamHostsRejectsGarbage(t *testing.T) {
	if _, err := parseUpstreamHosts([]byte("<html><body>rate limited</body></html>")); err == nil {
		t.Fatal("HTML РЅРµ РґРѕР»Р¶РµРЅ РїСЂРёРЅРёРјР°С‚СЊСЃСЏ Р·Р° hosts")
	}
}

func TestRemoveHostsBlock(t *testing.T) {
	up, _ := parseUpstreamHosts([]byte(upstreamHosts))
	merged, _ := mergeHosts("127.0.0.1 a\r\n", up)
	out, changed := removeHostsBlock(merged)
	if !changed || out != "127.0.0.1 a\r\n" {
		t.Fatalf("got %q", out)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.10.4", "1.10.4", 0},
		{"1.10.4", "1.10.5", -1},
		{"1.10.10", "1.10.9", 1},
		{"1.11.0", "1.10.99", 1},
		{"1.10.5b", "1.10.5", 1},
		{"1.10.5", "1.10.5b", -1},
		{"2.0", "1.99.99", 1},
		{"1.10.4-20261108-120000", "1.10.4", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestExeFromCmdline(t *testing.T) {
	cases := map[string]string{
		`"C:\a b\bin\winws.exe" --wf-tcp=80`:     `C:\a b\bin\winws.exe`,
		`C:\zapret\bin\winws.exe --wf-tcp=80`:    `C:\zapret\bin\winws.exe`,
		`"C:\zapret\zapret-manager.exe" service`: `C:\zapret\zapret-manager.exe`,
	}
	for in, want := range cases {
		if got := exeFromCmdline(in); got != want {
			t.Errorf("%s в†’ %s, want %s", in, got, want)
		}
	}
}

func makeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return p
}

func TestExtractZipStripsTopDir(t *testing.T) {
	p := makeZip(t, map[string]string{
		"zapret-discord-youtube-1.10.5/service.bat":       "x",
		"zapret-discord-youtube-1.10.5/bin/winws.exe":     "x",
		"zapret-discord-youtube-1.10.5/lists/list.txt":    "x",
		"zapret-discord-youtube-1.10.5/general (ALT).bat": "x",
	})
	dest := filepath.Join(t.TempDir(), "out")
	if err := extractZip(p, dest); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"service.bat", `bin\winws.exe`, `lists\list.txt`, "general (ALT).bat"} {
		if !fileExists(filepath.Join(dest, f)) {
			t.Errorf("РЅРµС‚ %s", f)
		}
	}
}

func TestExtractZipRejectsTraversal(t *testing.T) {
	p := makeZip(t, map[string]string{"top/ok.txt": "x", "top/../../evil.txt": "x"})
	if err := extractZip(p, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("РїСѓС‚СЊ СЃ .. РґРѕР»Р¶РµРЅ РѕС‚РІРµСЂРіР°С‚СЊСЃСЏ")
	}
}

func TestValidateIpset(t *testing.T) {
	var lines []string
	for i := 0; i < 1500; i++ {
		lines = append(lines, "10.0.0.0/24")
	}
	if err := validateIpset(lines); err != nil {
		t.Fatal(err)
	}
	if err := validateIpset([]string{"<html>"}); err == nil {
		t.Fatal("РјСѓСЃРѕСЂ РґРѕР»Р¶РµРЅ РѕС‚РІРµСЂРіР°С‚СЊСЃСЏ")
	}
}

func TestValidateList(t *testing.T) {
	if err := validateList("list-general.txt", []string{"^dns.google", "discord.com", "# c", ""}); err != nil {
		t.Fatal(err)
	}
	if err := validateList("list-general.txt", []string{"<!DOCTYPE html>"}); err == nil {
		t.Fatal("HTML РЅРµ СЃРїРёСЃРѕРє")
	}
	if err := validateList("ipset-exclude.txt", []string{"10.0.0.0/8", "::1", "fc00::/7"}); err != nil {
		t.Fatal(err)
	}
}

func TestSortRankingPrefersHealthyThenCurrent(t *testing.T) {
	r := []RankEntry{
		{Strategy: "A", Score: 9, Healthy: false, LatencyMs: 10},
		{Strategy: "B", Score: 8, Healthy: true, LatencyMs: 500},
		{Strategy: "C", Score: 8, Healthy: true, LatencyMs: 100},
		{Strategy: "D", Error: "boom"},
	}
	sortRanking(r, "B")
	if r[0].Strategy != "B" || r[1].Strategy != "C" || r[2].Strategy != "A" || r[3].Strategy != "D" {
		t.Fatalf("РїРѕСЂСЏРґРѕРє: %v", r)
	}
}

// Сверка с тем, что прописал в службу service.bat: аргументы должны совпасть один в один
// (service.bat пишет «--opt значение», мы — «--opt=значение», как в самом .bat).
func TestMatchesServiceBatInstall(t *testing.T) {
	info := queryService(zapretSvcName)
	strategy := readRegistryStrategy()
	if !info.Exists || strategy == "" {
		t.Skip("служба zapret не установлена")
	}
	exe := exeFromCmdline(info.BinPath)
	relDir := filepath.Dir(filepath.Dir(exe))
	if !validRelease(relDir) {
		t.Skip("папка службы не похожа на релиз")
	}
	s, err := (&App{Cfg: defaultConfig(), Root: t.TempDir()}).findStrategy(relDir, strategy)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := buildCommandLine(relDir, s, readGameFilter(relDir))
	if err != nil {
		t.Fatal(err)
	}
	norm := func(cmd string) []string {
		parts, err := windows.DecomposeCommandLine(cmd)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range parts[1:] {
			if strings.HasPrefix(p, "--") {
				if k, v, ok := strings.Cut(p, "="); ok {
					out = append(out, k, v)
					continue
				}
			}
			out = append(out, p)
		}
		return out
	}
	a, b := norm(ours), norm(info.BinPath)
	if strings.Join(a, "\x00") != strings.Join(b, "\x00") {
		for i := 0; i < len(a) && i < len(b); i++ {
			if a[i] != b[i] {
				t.Fatalf("расхождение на аргументе %d: у нас %q, в службе %q", i, a[i], b[i])
			}
		}
		t.Fatalf("разное число аргументов: %d против %d", len(a), len(b))
	}
	t.Logf("%s: %d аргументов совпали", strategy, len(a))
}
