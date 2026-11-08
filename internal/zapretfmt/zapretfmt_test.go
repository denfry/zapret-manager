package zapretfmt

import (
	"strings"
	"testing"
)

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
		// injection attempt: a range with extra winws arguments must be ignored
		{"mode=tcp\ntcp=443 --debug=@C:\\x\n", "1024-65535", "1024-65535", "12", "tcp"},
	}
	for _, c := range cases {
		gf := ParseGameFilter(c.in)
		a, tc, u := gf.Vars()
		if gf.Mode != c.mode || a != c.any || tc != c.tcp || u != c.udp {
			t.Errorf("%q: mode=%s vars=%s/%s/%s", c.in, gf.Mode, a, tc, u)
		}
	}
}

func TestGameFilterString(t *testing.T) {
	if s := ParseGameFilter("mode=all").String(); !strings.Contains(s, "TCP") || !strings.Contains(s, "UDP") {
		t.Fatal(s)
	}
	if s := DisabledGameFilter().String(); s != "выключен" {
		t.Fatal(s)
	}
}

func TestValidatePortRanges(t *testing.T) {
	good := []string{"1024-65535", "80", "1024-1934,1936-65535", " 443 , 8443 ", "65535"}
	bad := []string{"", "0", "65536", "100-50", "abc", "1-2-3", "123456", "1,", ",1", "080", "1 2x"}
	for _, g := range good {
		if _, ok := ValidatePortRanges(g); !ok {
			t.Errorf("%q должен быть валиден", g)
		}
	}
	for _, b := range bad {
		if _, ok := ValidatePortRanges(b); ok {
			t.Errorf("%q должен быть невалиден", b)
		}
	}
}

func TestIpsetModeOf(t *testing.T) {
	cases := map[string]string{
		"":                                 IpsetAny,
		"\xef\xbb\xbf\r\n":                 IpsetAny,
		"203.0.113.113/32\r\n":             IpsetNone,
		"1.0.0.0/24\r\n1.1.1.0/24\r\n":     IpsetLoaded,
		"# comment only, still loaded\r\n": IpsetLoaded,
	}
	for in, want := range cases {
		if got := IpsetModeOf([]byte(in)); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

func TestCountEntries(t *testing.T) {
	if n := CountEntries([]byte("# c\r\n1.1.1.1\r\n\r\n  2.2.2.0/24  \r\n")); n != 2 {
		t.Fatalf("got %d", n)
	}
}

func TestValidateIpset(t *testing.T) {
	lines := make([]string, 0, 1500)
	for range 1500 {
		lines = append(lines, "10.0.0.0/24")
	}
	if err := ValidateIpset(lines); err != nil {
		t.Fatal(err)
	}
	if err := ValidateIpset([]string{"<html>"}); err == nil {
		t.Fatal("мусор должен отвергаться")
	}
	mostlyGarbage := append(append([]string{}, lines[:1000]...), make([]string, 0)...)
	for range 100 {
		mostlyGarbage = append(mostlyGarbage, "<p>error</p>")
	}
	if err := ValidateIpset(mostlyGarbage); err == nil {
		t.Fatal("больше 5% мусора должно отвергаться")
	}
}

func TestValidateList(t *testing.T) {
	if err := ValidateList("list-general.txt", []string{"^dns.google", "discord.com", "# c", ""}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateList("list-general.txt", []string{"<!DOCTYPE html>"}); err == nil {
		t.Fatal("HTML не список")
	}
	if err := ValidateList("ipset-exclude.txt", []string{"10.0.0.0/8", "::1", "fc00::/7"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateList("ipset-exclude.txt", []string{"discord.com"}); err == nil {
		t.Fatal("домен в ipset должен отвергаться")
	}
	if err := ValidateList("list-general.txt", []string{"# only comments"}); err == nil {
		t.Fatal("пустой список должен отвергаться")
	}
}

// FuzzParseGameFilter: whatever the file contains, the values substituted
// into the service command line consist only of digits, commas and dashes.
func FuzzParseGameFilter(f *testing.F) {
	f.Add("mode=all\ntcp=1024-65535\nudp=1024-65535\n")
	f.Add("tcp=443 --debug=@x")
	f.Add("udp\n")
	f.Fuzz(func(t *testing.T, content string) {
		gf := ParseGameFilter(content)
		switch gf.Mode {
		case ModeDisabled, ModeAll, ModeTCP, ModeUDP:
		default:
			t.Fatalf("mode %q", gf.Mode)
		}
		a, tc, u := gf.Vars()
		for _, v := range []string{a, tc, u} {
			if v == "" || strings.Trim(v, "0123456789,-") != "" {
				t.Fatalf("unsafe value %q from %q", v, content)
			}
		}
		// записанный файл читается обратно в то же значение
		if back := ParseGameFilter(gf.FileContent()); back != gf {
			t.Fatalf("round trip %q: %+v → %q → %+v", content, gf, gf.FileContent(), back)
		}
	})
}

func TestGameFilterFileContent(t *testing.T) {
	gf := GameFilter{Mode: ModeTCP, TCPRange: "443,1024-65535", UDPRange: DefaultPortRange}
	want := "mode=tcp\r\ntcp=443,1024-65535\r\nudp=1024-65535\r\n"
	if got := gf.FileContent(); got != want {
		t.Fatalf("%q", got)
	}
	// значения не из ParseGameFilter тоже не попадают в файл как есть
	bad := GameFilter{Mode: "all --x", TCPRange: "1 & calc", UDPRange: ""}
	if got := bad.FileContent(); got != "mode=disabled\r\ntcp=1024-65535\r\nudp=1024-65535\r\n" {
		t.Fatalf("%q", got)
	}
}
