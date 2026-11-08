package main

import (
	"strings"
	"testing"
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
