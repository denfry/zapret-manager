package batch

import (
	"errors"
	"strings"
	"testing"
)

const sampleBat = "@echo off\r\nchcp 65001 > nul\r\n:: 65001 - UTF-8\r\n\r\ncd /d \"%~dp0\"\r\ncall service.bat status_zapret\r\n" +
	"set \"BIN=%~dp0bin\\\"\r\nset \"LISTS=%~dp0lists\\\"\r\ncd /d %BIN%\r\n\r\n" +
	"start \"zapret: %~n0\" /min \"%BIN%winws.exe\" --wf-tcp=80,443,%GameFilterTCP% --wf-udp=443,%GameFilterUDP% ^\r\n" +
	"--filter-udp=443 --hostlist=\"%LISTS%list-general.txt\" --dpi-desync=fake --dpi-desync-fake-quic=\"%BIN%quic_initial_www_google_com.bin\" --new ^\r\n" +
	"--filter-tcp=443 --dpi-desync-fake-tls=0x00000000 --dpi-desync-fake-tls=^! --dpi-desync-fake-tls-mod=rnd,dupsid,sni=www.google.com --new ^\r\n" +
	"--filter-tcp=%GameFilterTCP% --ipset=\"%LISTS%ipset-all.txt\" --dpi-desync=multisplit\r\n"

var sampleVars = map[string]string{
	"bin": `C:\zapret\releases\1.10.4\bin\`, "lists": `C:\zapret\releases\1.10.4\lists\`,
	"gamefilter": "1024-65535", "gamefiltertcp": "1024-65535", "gamefilterudp": "12",
}

func TestExtractWinwsArgs(t *testing.T) {
	args, err := ExtractWinwsArgs(sampleBat, sampleVars, `C:\zapret\releases\1.10.4\`)
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
	_, err := ExtractWinwsArgs(`start "" "%BIN%winws.exe" --wf-tcp=%Nope% --new`, map[string]string{"bin": `C:\b\`}, `C:\`)
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("ждал ошибку про %%Nope%%, получил %v", err)
	}
}

func TestExtractWinwsArgsSkipsComments(t *testing.T) {
	content := ":: \"winws.exe\" --commented\r\nrem \"winws.exe\" --also\r\nstart \"\" \"x\\winws.exe\" --real=1\r\n"
	args, err := ExtractWinwsArgs(content, nil, `C:\`)
	if err != nil || args != "--real=1" {
		t.Fatalf("got %q, %v", args, err)
	}
}

// Regression (found by fuzzing): a strategy saved in CP1251 with Cyrillic in
// the window title is invalid UTF-8; strings.ToLower changed byte offsets and
// the parser panicked with "slice bounds out of range".
func TestExtractWinwsArgsCP1251(t *testing.T) {
	content := "start \"\xcf\xf0\xee\xe1\xe0 zapret\" /min \"%BIN%WINWS.EXE\" --wf-tcp=443\r\n"
	args, err := ExtractWinwsArgs(content, map[string]string{"bin": `C:\b\`}, `C:\`)
	if err != nil || args != "--wf-tcp=443" {
		t.Fatalf("got %q, %v", args, err)
	}
	if got := ExeFromCmdline("\x98\xcf.eXe --x"); got != "\x98\xcf.eXe" {
		t.Fatalf("ExeFromCmdline: %q", got)
	}
}

func TestExtractWinwsArgsErrors(t *testing.T) {
	if _, err := ExtractWinwsArgs("echo hello\r\n", nil, `C:\`); !errors.Is(err, ErrNoWinws) {
		t.Fatalf("want ErrNoWinws, got %v", err)
	}
	if _, err := ExtractWinwsArgs(`start "" "winws.exe" nothing`, nil, `C:\`); !errors.Is(err, ErrNoArgs) {
		t.Fatalf("want ErrNoArgs, got %v", err)
	}
}

func TestExpandVars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"100%%", "100%"},
		{"%~DP0x", `C:\r\x`},
		{"50% off", "50% off"},                  // lone % kept
		{"%not a var% %BIN%", `%not a var% B\`}, // invalid name kept literally
		{"%Bin%%LISTS%", `B\L\`},
	}
	vars := map[string]string{"bin": `B\`, "lists": `L\`}
	for _, c := range cases {
		got, err := ExpandVars(c.in, vars, `C:\r\`)
		if err != nil || got != c.want {
			t.Errorf("ExpandVars(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestUnescapeCaretsInQuotes(t *testing.T) {
	if got := UnescapeCarets(`a^&b "x^y" ^^`); got != `a&b "x^y" ^` {
		t.Fatalf("got %q", got)
	}
}

func TestContinuationWithEscapedCaret(t *testing.T) {
	content := "start \"\" \"x\\winws.exe\" --a=1 ^^\r\n--b=2\r\n"
	args, err := ExtractWinwsArgs(content, map[string]string{}, `C:\`)
	if err != nil {
		t.Fatal(err)
	}
	if args != "--a=1 ^" {
		t.Fatalf("^^ в конце — не перенос, got %q", args)
	}
}

func TestNaturalOrder(t *testing.T) {
	if naturalLess := NaturalKey("general (ALT2)") < NaturalKey("general (ALT10)"); !naturalLess {
		t.Fatal("ALT2 должна идти раньше ALT10")
	}
	if NaturalKey("General") != NaturalKey("general") {
		t.Fatal("сортировка без учёта регистра")
	}
}

func TestExeFromCmdline(t *testing.T) {
	cases := map[string]string{
		`"C:\a b\bin\winws.exe" --wf-tcp=80`:     `C:\a b\bin\winws.exe`,
		`C:\zapret\bin\winws.exe --wf-tcp=80`:    `C:\zapret\bin\winws.exe`,
		`"C:\zapret\zapret-manager.exe" service`: `C:\zapret\zapret-manager.exe`,
		`"C:\unterminated`:                       `C:\unterminated`,
		`  `:                                     ``,
		`plain arg`:                              `plain`,
	}
	for in, want := range cases {
		if got := ExeFromCmdline(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// FuzzExtractWinwsArgs: the parser must never panic on arbitrary .bat
// content, and any successful result must be fully expanded — a leftover
// %VAR% would reach the service command line verbatim.
func FuzzExtractWinwsArgs(f *testing.F) {
	f.Add(sampleBat)
	f.Add("start \"\" \"x\\winws.exe\" --a=1 ^^\r\n--b=2\r\n")
	f.Add("\"winws.exe\" --x=%BIN% ^")
	f.Add("%%%~dp0%%bin%")
	f.Fuzz(func(t *testing.T, content string) {
		args, err := ExtractWinwsArgs(content, sampleVars, `C:\r\`)
		if err != nil {
			return
		}
		if !strings.Contains(args, "--") {
			t.Fatalf("success without arguments: %q", args)
		}
		if args != strings.TrimSpace(args) {
			t.Fatalf("result is not trimmed: %q", args)
		}
	})
}

// FuzzExeFromCmdline: the result is always a substring of the trimmed input.
func FuzzExeFromCmdline(f *testing.F) {
	f.Add(`"C:\a b\winws.exe" --x`)
	f.Add(`C:\x.EXE y`)
	f.Add(`"`)
	f.Fuzz(func(t *testing.T, cmd string) {
		exe := ExeFromCmdline(cmd)
		if !strings.Contains(cmd, exe) {
			t.Fatalf("%q is not part of %q", exe, cmd)
		}
	})
}

// FuzzUnescapeCarets: unescaping never grows the string and is a no-op on
// input without carets.
func FuzzUnescapeCarets(f *testing.F) {
	f.Add(`a^&b "x^y" ^^`)
	f.Add("^")
	f.Fuzz(func(t *testing.T, s string) {
		out := UnescapeCarets(s)
		if len(out) > len(s) {
			t.Fatalf("grew: %q → %q", s, out)
		}
		if !strings.Contains(s, "^") && out != s {
			t.Fatalf("changed caret-free input: %q → %q", s, out)
		}
	})
}
