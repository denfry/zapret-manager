package textutil

import (
	"strings"
	"testing"
)

func TestStripBOM(t *testing.T) {
	if got := string(StripBOM([]byte(BOM + "x"))); got != "x" {
		t.Fatalf("%q", got)
	}
	if got := string(StripBOM([]byte("\xef\xbbx"))); got != "\xef\xbbx" {
		t.Fatalf("неполный BOM не трогаем: %q", got)
	}
}

func TestCleanLines(t *testing.T) {
	got := CleanLines([]byte(BOM + " a \r\n\r\n\tb\r\n\r\n\r\n"))
	if strings.Join(got, "|") != "a||b" {
		t.Fatalf("%q", got)
	}
	if got := CleanLines(nil); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestNormalizeText(t *testing.T) {
	a := NormalizeText(BOM + "a  \r\nb\t\r\n\r\n")
	b := NormalizeText("a\nb")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
}
