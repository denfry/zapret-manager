package version

import "testing"

func TestCompare(t *testing.T) {
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
		{"1.10.4-20261108-120000", "1.10.4-20261108-130000", -1},
		// numbers longer than int64 must still be ordered numerically
		{"1.100000000000000000000", "1.9", 1},
		{"1.007", "1.7", 0},
		{"1.10.5B", "1.10.5b", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	good := []string{"1.10.4", "1.10.5b", "1.10.4-20261108-120000", "2"}
	bad := []string{"", "v1.0", "1.0/../..", `1.0\x`, "1:0", "<html>", "1.0 ", "1234567890123456789012345678901234"}
	for _, v := range good {
		if !Valid(v) {
			t.Errorf("%q must be valid", v)
		}
	}
	for _, v := range bad {
		if Valid(v) {
			t.Errorf("%q must be invalid", v)
		}
	}
}

func TestFromServiceBat(t *testing.T) {
	v, ok := FromServiceBat([]byte("@echo off\r\nset \"LOCAL_VERSION=1.10.4\"\r\n"))
	if !ok || v != "1.10.4" {
		t.Fatalf("got %q %v", v, ok)
	}
	if _, ok := FromServiceBat([]byte("echo nothing")); ok {
		t.Fatal("no LOCAL_VERSION must report false")
	}
}

func TestFromDirName(t *testing.T) {
	cases := map[string]string{
		"zapret-discord-youtube-1.10.4": "1.10.4",
		"1.10.5":                        "1.10.5",
		"custom-folder":                 "custom-folder",
	}
	for in, want := range cases {
		if got := FromDirName(in); got != want {
			t.Errorf("FromDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzCompare checks the ordering laws release pruning and update decisions
// rely on: reflexivity and antisymmetry for any input.
func FuzzCompare(f *testing.F) {
	for _, s := range [][2]string{{"1.10.4", "1.10.5b"}, {"1.0", "a"}, {"99999999999999999999", "1"}, {"", "x"}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		if Compare(a, a) != 0 {
			t.Fatalf("Compare(%q, %q) != 0", a, a)
		}
		ab, ba := Compare(a, b), Compare(b, a)
		if ab != -ba {
			t.Fatalf("not antisymmetric: Compare(%q,%q)=%d, Compare(%q,%q)=%d", a, b, ab, b, a, ba)
		}
		if ab < -1 || ab > 1 {
			t.Fatalf("Compare returned %d", ab)
		}
	})
}
