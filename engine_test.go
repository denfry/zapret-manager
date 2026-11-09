package main

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestSortRankingPrefersHealthyThenCurrent(t *testing.T) {
	r := []RankEntry{
		{Strategy: "A", Score: 9, Healthy: false, LatencyMs: 10},
		{Strategy: "B", Score: 8, Healthy: true, LatencyMs: 500},
		{Strategy: "C", Score: 8, Healthy: true, LatencyMs: 100},
		{Strategy: "D", Error: "boom"},
	}
	sortRanking(r, "B")
	if r[0].Strategy != "B" || r[1].Strategy != "C" || r[2].Strategy != "A" || r[3].Strategy != "D" {
		t.Fatalf("порядок: %v", r)
	}
	sortRanking(r, "")
	if r[0].Strategy != "C" {
		t.Fatalf("без текущей — быстрее лучше: %v", r)
	}
}

func TestRankEntryLine(t *testing.T) {
	e := RankEntry{Strategy: "general (ALT)", Score: 4, Healthy: true, Discord: "6/6", YouTube: "4/4", LatencyMs: 800}
	if l := e.Line(); !strings.HasPrefix(l, "✓ general (ALT)") || !strings.Contains(l, " 200 мс") {
		t.Fatalf("%q", l)
	}
	if l := (RankEntry{Strategy: "x", Error: "boom"}).Line(); !strings.Contains(l, "ошибка: boom") {
		t.Fatalf("%q", l)
	}
}

func TestReleaseIsWorse(t *testing.T) {
	healthy := &HealthReport{Healthy: true, Score: 10}
	cases := []struct {
		name      string
		base, rep *HealthReport
		want      bool
	}{
		{"старый не работал — не откатываемся", nil, nil, false},
		{"старый был сломан", &HealthReport{Healthy: false, Score: 3}, nil, false},
		{"старый без сети — не судим", &HealthReport{Healthy: true, NetworkDown: true}, nil, false},
		{"новый не встал", healthy, nil, true},
		{"новый хуже по здоровью", healthy, &HealthReport{Healthy: false, Score: 10}, true},
		{"новый открывает меньше", healthy, &HealthReport{Healthy: true, Score: 9}, true},
		{"новый не хуже", healthy, &HealthReport{Healthy: true, Score: 10}, false},
		{"при замере нового пропала сеть — не судим", healthy, &HealthReport{NetworkDown: true}, false},
	}
	for _, c := range cases {
		if got := releaseIsWorse(c.base, c.rep); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAppendUnique(t *testing.T) {
	l := appendUnique(nil, "1.0")
	l = appendUnique(l, "1.0")
	l = appendUnique(l, "1.1")
	if strings.Join(l, ",") != "1.0,1.1" {
		t.Fatal(l)
	}
}

func entry(name string, groups map[string]GroupStat) RankEntry {
	return RankEntry{Strategy: name, Groups: groups}
}

func TestUnfixableGroups(t *testing.T) {
	entries := []RankEntry{
		entry("A", map[string]GroupStat{"discord": {0, 6}, "instagram": {0, 2}, "x": {1, 2}, "site.example": {0, 1}}),
		entry("B", map[string]GroupStat{"discord": {0, 6}, "instagram": {0, 2}, "x": {2, 2}, "site.example": {0, 1}}),
		{Strategy: "broken", Error: "не стартует", Groups: map[string]GroupStat{"site.example": {1, 1}}},
	}
	got := unfixableGroups(entries, 0.75)
	// x открыла стратегия B; Discord не пропускается никогда; стратегия с ошибкой не в счёт
	if len(got) != 2 || !got["instagram"] || !got["site.example"] {
		t.Fatalf("%v", got)
	}
	if len(unfixableGroups(entries[2:], 0.75)) != 0 {
		t.Fatal("одни ошибки — судить не по чему")
	}
}

func TestRehealth(t *testing.T) {
	e := entry("A", map[string]GroupStat{"discord": {6, 6}, "youtube": {4, 4}, "instagram": {0, 2}})
	e.rehealth(0.75, nil)
	if e.Healthy {
		t.Fatal("Instagram не открылся — нездоров")
	}
	e.rehealth(0.75, map[string]bool{"instagram": true})
	if !e.Healthy || e.Other != "Instagram 0/2 (zapret не помогает)" {
		t.Fatalf("%+v", e)
	}
	bad := RankEntry{Strategy: "x", Error: "boom"}
	bad.rehealth(0.75, nil)
	if bad.Healthy {
		t.Fatal("стратегия с ошибкой здоровой не становится")
	}
	e.Discord, e.YouTube = "6/6", "4/4" // их заполняет fill()
	if s := e.groupsText(); s != "Discord 6/6, YouTube 4/4, Instagram 0/2 (zapret не помогает)" {
		t.Fatalf("groupsText: %q", s)
	}
}

func TestWasRunning(t *testing.T) {
	cases := []struct {
		info svcInfo
		want bool
	}{
		{svcInfo{}, false},
		{svcInfo{Exists: true, State: svc.Running}, true},
		{svcInfo{Exists: true, State: svc.StartPending}, true},
		// пауза (off) или выключена руками — после подбора не включаем
		{svcInfo{Exists: true, State: svc.Stopped, StartType: mgr.StartManual}, false},
		{svcInfo{Exists: true, State: svc.Stopped, StartType: mgr.StartAutomatic}, false},
	}
	for _, c := range cases {
		if got := wasRunning(c.info); got != c.want {
			t.Errorf("wasRunning(%+v) = %v, want %v", c.info, got, c.want)
		}
	}
}
