package hostsfile

import (
	"strings"
	"testing"
)

const upstreamHosts = "140.82.121.3 github.com\n146.75.22.132 raw.githubusercontent.com\n\n149.154.167.220 t.me\n149.154.167.220 web.telegram.org\n\n162.159.138.232 discord.com\n162.159.137.232 discord.com\n"

func mustParse(t testing.TB) []string {
	t.Helper()
	up, err := ParseUpstream([]byte(upstreamHosts))
	if err != nil {
		t.Fatal(err)
	}
	return up
}

func TestMergeMigratesUnmarkedBlock(t *testing.T) {
	up := mustParse(t)
	local := "# Copyright (c) 1993-2009 Microsoft Corp.\r\n#\r\n127.0.0.1 my.local\r\n" +
		"140.82.121.3 github.com\r\n146.75.22.132 raw.githubusercontent.com\r\n\r\n" +
		"1.2.3.4 discord.com # старый IP\r\n10.0.0.5 nas.home t.me\r\n"
	out, changed := Merge(local, up)
	if !changed {
		t.Fatal("должен измениться")
	}
	if !strings.Contains(out, "127.0.0.1 my.local\r\n") || !strings.Contains(out, "# Copyright") {
		t.Fatal("чужие записи потерялись:\n" + out)
	}
	if strings.Contains(out, "1.2.3.4 discord.com") {
		t.Fatal("старая запись discord.com осталась")
	}
	if !strings.Contains(out, "10.0.0.5 nas.home\r\n") || strings.Contains(out, "nas.home t.me") {
		t.Fatal("t.me должен уйти из смешанной строки, nas.home остаться:\n" + out)
	}
	if strings.Count(out, "140.82.121.3 github.com") != 1 {
		t.Fatal("github.com должен быть ровно один раз:\n" + out)
	}
	if !strings.Contains(out, Begin+"\r\n140.82.121.3 github.com") || !strings.HasSuffix(out, End+"\r\n") {
		t.Fatal("блок не в конце:\n" + out)
	}
	again, changed2 := Merge(out, up)
	if changed2 || again != out {
		t.Fatalf("повторное слияние должно быть без изменений:\n%s\n---\n%s", out, again)
	}
}

func TestMergeKeepsCommentOnPartiallyOwnedLine(t *testing.T) {
	out, _ := Merge("10.0.0.5 nas.home t.me # NAS\r\n", mustParse(t))
	if !strings.Contains(out, "10.0.0.5 nas.home # NAS\r\n") {
		t.Fatalf("комментарий должен сохраниться:\n%s", out)
	}
}

func TestMergeReplacesOwnBlockAndKeepsTail(t *testing.T) {
	up := mustParse(t)
	local := "127.0.0.1 a\r\n\r\n" + Begin + "\r\n9.9.9.9 discord.com\r\n" + End + "\r\n192.168.1.1 router\r\n"
	out, _ := Merge(local, up)
	if strings.Contains(out, "9.9.9.9") {
		t.Fatal("старый блок не удалён")
	}
	if !strings.Contains(out, "192.168.1.1 router") {
		t.Fatal("запись после блока потерялась")
	}
	if strings.Count(out, Begin) != 1 {
		t.Fatal("маркер должен быть один")
	}
}

func TestMergeBrokenMarkerKeepsEntries(t *testing.T) {
	local := Begin + "\r\n192.168.1.1 router\r\n"
	out, _ := Merge(local, mustParse(t))
	if !strings.Contains(out, "192.168.1.1 router") {
		t.Fatal("без закрывающего маркера ничего удалять нельзя")
	}
}

func TestMergeEmptyLocal(t *testing.T) {
	out, changed := Merge("", mustParse(t))
	if !changed || !strings.HasPrefix(out, Begin+"\r\n") {
		t.Fatalf("got changed=%v:\n%s", changed, out)
	}
}

func TestParseUpstreamRejectsGarbage(t *testing.T) {
	bad := []string{
		"<html><body>rate limited</body></html>",
		"1.1.1.1 a\n2.2.2.2 b\n",                         // too few entries
		"1.1.1.1 a\n2.2.2.2 b\n3.3.3.3 c\nnot-an-ip x\n", // bad IP
		"1.1.1.1 a\n2.2.2.2 b\n3.3.3.3 c\n" + End + "\n", // marker injection
	}
	for _, b := range bad {
		if _, err := ParseUpstream([]byte(b)); err == nil {
			t.Errorf("должно отвергаться: %q", b)
		}
	}
}

func TestParseUpstreamNormalizes(t *testing.T) {
	up, err := ParseUpstream([]byte("\uFEFF\r\n\r\n1.1.1.1\t a\r\n\r\n\r\n# c\r\n2.2.2.2 b\r\n3.3.3.3 c\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.1.1.1 a", "", "# c", "2.2.2.2 b", "3.3.3.3 c"}
	if strings.Join(up, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", up)
	}
}

func TestRemoveBlock(t *testing.T) {
	merged, _ := Merge("127.0.0.1 a\r\n", mustParse(t))
	out, changed := RemoveBlock(merged)
	if !changed || out != "127.0.0.1 a\r\n" {
		t.Fatalf("got %q", out)
	}
}

func TestRemoveBlockUnterminatedKeepsUserLines(t *testing.T) {
	out, changed := RemoveBlock("127.0.0.1 a\r\n" + Begin + "\r\n192.168.1.1 router\r\n")
	if !changed || !strings.Contains(out, "192.168.1.1 router") || strings.Contains(out, Begin) {
		t.Fatalf("got changed=%v %q", changed, out)
	}
}

func TestRemoveBlockNoBlock(t *testing.T) {
	if _, changed := RemoveBlock("127.0.0.1 a\r\n"); changed {
		t.Fatal("без блока менять нечего")
	}
}

// FuzzMerge checks the invariants hosts updates rely on for any local hosts
// file: idempotency, exactly one block at the end, and RemoveBlock leaving
// no marker behind.
func FuzzMerge(f *testing.F) {
	f.Add("127.0.0.1 a\r\n")
	f.Add(Begin + "\r\n192.168.1.1 router\r\n")
	f.Add(End + "\n" + Begin + "\n1.1.1.1 x\n" + End + "\n" + Begin)
	f.Add("\uFEFF10.0.0.5 nas.home t.me # c # d\r\n\r\n  \r\n")
	f.Add("1.2.3.4 DISCORD.com\r1.1.1.1 x\n")
	up := mustParse(f)
	f.Fuzz(func(t *testing.T, local string) {
		out, _ := Merge(local, up)
		again, changed := Merge(out, up)
		if changed || again != out {
			t.Fatalf("not idempotent for %q:\n%q\n%q", local, out, again)
		}
		begins := 0
		for l := range strings.SplitSeq(out, "\n") {
			if strings.TrimSpace(l) == Begin {
				begins++
			}
		}
		if begins != 1 || !strings.HasSuffix(out, End+"\r\n") {
			t.Fatalf("block is not unique or not at the end: %q", out)
		}
		removed, _ := RemoveBlock(out)
		for l := range strings.SplitSeq(removed, "\n") {
			if isMarker(l) {
				t.Fatalf("marker line left after RemoveBlock: %q", removed)
			}
		}
	})
}

// FuzzParseUpstream: arbitrary downloaded content never panics, and accepted
// content never contains the block markers.
func FuzzParseUpstream(f *testing.F) {
	f.Add([]byte(upstreamHosts))
	f.Add([]byte("<html>"))
	f.Fuzz(func(t *testing.T, data []byte) {
		lines, err := ParseUpstream(data)
		if err != nil {
			return
		}
		for _, l := range lines {
			if isMarker(l) {
				t.Fatalf("marker accepted: %q", l)
			}
		}
	})
}
