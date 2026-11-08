package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"zapret-manager/internal/sitelist"
	"zapret-manager/internal/zapretfmt"
)

// testRelease — папка релиза с файлами, которые трогает команда sites.
func testRelease(t *testing.T, a *App) string {
	t.Helper()
	dir := filepath.Join(a.releasesDir(), "1.10.4")
	if err := os.MkdirAll(filepath.Join(dir, "lists"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lists", "ipset-all.txt"), []byte("1.1.1.0/24\r\n2.2.2.0/24\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureUserLists(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCatalogIsValid(t *testing.T) {
	ids := map[string]bool{}
	for _, s := range siteCatalog {
		if ids[s.ID] || s.ID == "" || coreGroup(s.ID) || s.ID == "all" {
			t.Errorf("ID %q пустой, повторяется или занят", s.ID)
		}
		ids[s.ID] = true
		if f := sitelist.Parse(""); !f.SetService(s.ID, s.Domains) || !f.HasService(s.ID) {
			t.Errorf("%s: ID не годится для заголовка раздела", s.ID)
		}
		for _, d := range s.Domains {
			if n, err := sitelist.NormalizeDomain(d); err != nil || n != d {
				t.Errorf("%s: домен %q → %q, %v", s.ID, d, n, err)
			}
		}
		if len(s.Domains) > 0 {
			if err := zapretfmt.ValidateList("list-general-user.txt", s.Domains); err != nil {
				t.Errorf("%s: %v", s.ID, err)
			}
		}
		if len(s.Checks) == 0 {
			t.Errorf("%s: нечего проверять", s.ID)
		}
		for _, c := range s.Checks {
			u, err := url.Parse(c.URL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || c.Group != s.ID || c.Name == "" {
				t.Errorf("%s: плохой адрес проверки %+v", s.ID, c)
			}
			// свой CA Signal: проверка всегда выглядела бы как подмена сертификата
			if u.Hostname() == "chat.signal.org" {
				t.Errorf("%s: %s нельзя проверять", s.ID, c.URL)
			}
		}
		if groupTitle(s.ID) != s.Name {
			t.Errorf("%s: заголовок группы %q", s.ID, groupTitle(s.ID))
		}
	}
}

func TestTargetsFor(t *testing.T) {
	a := testApp(t)
	rel := testRelease(t, a)
	f := sitelist.Parse(readFile(t, userListPath(rel)))
	ig, _ := catalogService("instagram")
	f.SetService(ig.ID, ig.Domains)
	f.SetService("unknown-from-newer-version", []string{"new.example"})
	f.AddDomain("^exact.example") // точное совпадение в hostlist — не проверяем
	for i := range 12 {
		f.AddDomain(fmt.Sprintf("site%02d.example", i))
	}
	if _, err := writeSiteList(userListPath(rel), f); err != nil {
		t.Fatal(err)
	}

	got := a.targetsFor(rel)
	var urls []string
	groups := map[string]int{}
	for _, tg := range got {
		urls = append(urls, tg.URL)
		groups[tg.Group]++
	}
	if groups["discord"] == 0 || groups["control"] == 0 {
		t.Fatalf("стандартные адреса пропали: %v", groups)
	}
	if groups["instagram"] != len(ig.Checks) {
		t.Fatalf("instagram: %d адресов", groups["instagram"])
	}
	if !slices.Contains(urls, "https://site00.example/") || slices.Contains(urls, "https://site10.example/") {
		t.Fatalf("своих сайтов должно быть первые %d: %v", maxUserDomainChecks, urls)
	}
	for _, u := range urls {
		if strings.Contains(u, "exact.example") || strings.Contains(u, "new.example") {
			t.Fatalf("лишний адрес %s", u)
		}
	}

	// свои адреса из config.json заменяют стандартные, сайты добавляются, повторы — нет
	a.Cfg.Targets = []Target{{"Instagram", "https://www.instagram.com/", "mine"}}
	got = a.targetsFor(rel)
	n := 0
	for _, tg := range got {
		if tg.URL == "https://www.instagram.com/" {
			n++
		}
		if tg.Group == "discord" {
			t.Fatal("стандартные адреса должны замениться config.json")
		}
	}
	if n != 1 {
		t.Fatalf("адрес повторяется %d раз", n)
	}
	if len(a.targetsFor("")) != 1 {
		t.Fatal("без релиза — только адреса из config.json")
	}
}

func TestPlanSites(t *testing.T) {
	p, err := planSites([]string{"add", "Instagram", "https://www.Example.com/x", "all"})
	if err != nil || p.sub != "add" || len(p.services) != 1+len(siteCatalog) || !slices.Equal(p.domains, []string{"example.com"}) {
		t.Fatalf("%+v, %v", p, err)
	}
	if p, err := planSites([]string{"mode", "all"}); err != nil || p.mode != zapretfmt.IpsetAny {
		t.Fatalf("%+v, %v", p, err)
	}
	if p, err := planSites([]string{"games", "on"}); err != nil || p.mode != zapretfmt.ModeAll {
		t.Fatalf("%+v, %v", p, err)
	}
	if p, err := planSites([]string{"exclude", "bank.example"}); err != nil || !slices.Equal(p.domains, []string{"bank.example"}) {
		t.Fatalf("%+v, %v", p, err)
	}
	for _, bad := range [][]string{
		{"add"}, {"add", "1.2.3.4"}, {"add", "instagramm"}, {"exclude", "instagram"},
		{"mode", "everything"}, {"mode", "all", "lists"}, {"games", "maybe"}, {"frobnicate", "x"},
	} {
		if _, err := planSites(bad); err == nil {
			t.Errorf("%q: должна быть ошибка", bad)
		}
	}
}

func TestEditSiteFiles(t *testing.T) {
	a := testApp(t)
	rel := testRelease(t, a)
	ctx := context.Background()
	original := readFile(t, userListPath(rel))

	plan := func(args ...string) sitesPlan {
		t.Helper()
		p, err := planSites(args)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	e, err := a.editSiteFiles(ctx, rel, plan("add", "instagram", "example.com"))
	if err != nil || !e.changed || !slices.Equal(e.recheck, []string{"instagram", "example.com"}) {
		t.Fatalf("%+v, %v", e, err)
	}
	got := readFile(t, userListPath(rel))
	if !strings.HasPrefix(got, original) || !strings.Contains(got, "example.com\r\n") ||
		!strings.Contains(got, sitelist.Begin+"\r\n# service: instagram\r\ninstagram.com\r\n") {
		t.Fatalf("список:\n%s", got)
	}
	if e, _ := a.editSiteFiles(ctx, rel, plan("add", "instagram", "example.com")); e.changed {
		t.Fatalf("повторное добавление ничего не меняет: %+v", e)
	}
	if e, _ := a.editSiteFiles(ctx, rel, plan("remove", "instagram", "example.com")); !e.changed || len(e.recheck) != 0 {
		t.Fatalf("%+v", e)
	}
	if got := readFile(t, userListPath(rel)); got != original {
		t.Fatalf("после удаления файл должен стать как был:\n%q\n%q", got, original)
	}
	if e, _ := a.editSiteFiles(ctx, rel, plan("remove", "nothere.example")); e.changed || !strings.Contains(e.msgs[0], "нет среди своих") {
		t.Fatalf("%+v", e)
	}

	if e, _ := a.editSiteFiles(ctx, rel, plan("exclude", "bank.example")); !e.changed {
		t.Fatalf("%+v", e)
	}
	if got := readFile(t, excludeListPath(rel)); !strings.Contains(got, "bank.example\r\n") || !strings.Contains(got, sitelist.Placeholder) {
		t.Fatalf("исключения:\n%s", got)
	}
	if e, _ := a.editSiteFiles(ctx, rel, plan("include", "bank.example")); !e.changed || !slices.Equal(e.recheck, []string{"bank.example"}) {
		t.Fatalf("%+v", e)
	}

	e, err = a.editSiteFiles(ctx, rel, plan("mode", "all"))
	if err != nil || !e.changed || !e.recheckAll || ipsetMode(rel) != zapretfmt.IpsetAny {
		t.Fatalf("%+v, %v", e, err)
	}
	v := a.readSitesView(rel)
	if v.Mode != zapretfmt.IpsetAny || len(v.Services) != 0 || len(v.Excluded) != 0 {
		t.Fatalf("%+v", v)
	}
}

func TestSetIpsetModeCycle(t *testing.T) {
	a := testApp(t)
	rel := testRelease(t, a)
	ctx := context.Background()
	p := filepath.Join(rel, "lists", "ipset-all.txt")
	loaded := readFile(t, p)

	if ch, err := a.setIpsetMode(ctx, rel, zapretfmt.IpsetLoaded); err != nil || ch {
		t.Fatalf("тот же режим: %v %v", ch, err)
	}
	if _, err := a.setIpsetMode(ctx, rel, zapretfmt.IpsetAny); err != nil || readFile(t, p) != "" || readFile(t, p+".backup") != loaded {
		t.Fatalf("any: %v", err)
	}
	if _, err := a.setIpsetMode(ctx, rel, zapretfmt.IpsetNone); err != nil || ipsetMode(rel) != zapretfmt.IpsetNone || readFile(t, p+".backup") != loaded {
		t.Fatalf("none: %v", err)
	}
	if _, err := a.setIpsetMode(ctx, rel, zapretfmt.IpsetLoaded); err != nil || readFile(t, p) != loaded || fileExists(p+".backup") {
		t.Fatalf("loaded из копии: %v", err)
	}
}

func TestSetIpsetModeLoadedDownloadsWithoutBackup(t *testing.T) {
	g := newFakeGitHub(t, "1.10.4")
	a := g.app(t)
	rel := testRelease(t, a)
	ctx := context.Background()
	p := filepath.Join(rel, "lists", "ipset-all.txt")
	if err := os.WriteFile(p, nil, 0o644); err != nil { // any, копии нет
		t.Fatal(err)
	}
	if _, err := a.setIpsetMode(ctx, rel, zapretfmt.IpsetLoaded); err != nil || ipsetMode(rel) != zapretfmt.IpsetLoaded || countIpsetLines(rel) != 1200 {
		t.Fatalf("скачанный список: %v, %d строк", err, countIpsetLines(rel))
	}

	// без сети режим остаётся прежним, а не ломается
	if _, err := a.setIpsetMode(ctx, rel, zapretfmt.IpsetAny); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p + ".backup"); err != nil {
		t.Fatal(err)
	}
	g.srv.Close()
	if _, err := a.setIpsetMode(ctx, rel, zapretfmt.IpsetLoaded); err == nil || ipsetMode(rel) != zapretfmt.IpsetAny {
		t.Fatalf("без сети: %v, режим %s", err, ipsetMode(rel))
	}
}

func TestSkippedGroups(t *testing.T) {
	a := testApp(t)
	if a.skippedGroups() != nil {
		t.Fatal("без подбора пропускать нечего")
	}
	a.updateState(func(st *State) {
		st.LastSweep = &SweepSummary{Time: time.Now(), Unfixable: []string{"instagram", "discord", "example.com"}}
	})
	skip := a.skippedGroups()
	if !skip["instagram"] || !skip["example.com"] || skip["discord"] {
		t.Fatalf("Discord и YouTube не пропускаются никогда: %v", skip)
	}
	a.forgetUnfixable("instagram")
	if skip := a.skippedGroups(); skip["instagram"] || !skip["example.com"] {
		t.Fatalf("после forgetUnfixable(instagram): %v", skip)
	}
	a.forgetUnfixable()
	if len(a.skippedGroups()) != 0 {
		t.Fatal("forgetUnfixable() — забыть всё")
	}
	a.updateState(func(st *State) {
		st.LastSweep = &SweepSummary{Time: time.Now().Add(-unfixableTTL - time.Minute), Unfixable: []string{"instagram"}}
	})
	if a.skippedGroups() != nil {
		t.Fatal("через сутки группа снова чинится")
	}
}

func TestSitesViewLines(t *testing.T) {
	v := sitesView{
		Mode: zapretfmt.IpsetLoaded, Subnets: 33336, Builtin: 81,
		Services: []string{"instagram", "x"}, Domains: []string{"example.com"},
		Games: zapretfmt.DisabledGameFilter(), Unfixable: []string{"x"},
	}
	text := strings.Join(v.lines(), "\n")
	for _, want := range []string{"по спискам", "33336 подсетей", "доменов: 81", "Instagram, X (Twitter)", "example.com", "Игры:         выключен", "Не помогает:  X (Twitter)"} {
		if !strings.Contains(text, want) {
			t.Errorf("нет %q в\n%s", want, text)
		}
	}
	if strings.Contains(text, "Исключения") {
		t.Errorf("пустые исключения не показываем:\n%s", text)
	}
}

func TestSyncCatalog(t *testing.T) {
	a := testApp(t)
	rel := testRelease(t, a)
	f := sitelist.Parse(readFile(t, userListPath(rel)))
	f.SetService("instagram", []string{"instagram.com"}) // старая версия каталога
	f.SetService("future", []string{"future.example"})   // раздел из более новой версии
	f.AddDomain("my.example")
	if _, err := writeSiteList(userListPath(rel), f); err != nil {
		t.Fatal(err)
	}
	names, err := syncCatalog(rel)
	if err != nil || !slices.Equal(names, []string{"Instagram"}) {
		t.Fatalf("%v, %v", names, err)
	}
	got := sitelist.Parse(readFile(t, userListPath(rel)))
	ig, _ := catalogService("instagram")
	if !slices.Equal(got.Sections[0].Domains, ig.Domains) || !got.HasService("future") || !slices.Equal(got.UserDomains(), []string{"my.example"}) {
		t.Fatalf("%+v", got)
	}
	if names, err := syncCatalog(rel); err != nil || len(names) != 0 {
		t.Fatalf("повторно менять нечего: %v, %v", names, err)
	}
}
