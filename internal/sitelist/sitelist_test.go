package sitelist

import (
	"slices"
	"strings"
	"testing"
)

const flowsealDefault = "# Never leave this file empty\r\ndomain.example.abc\r\n"

func TestParseRenderKeepsFlowsealDefault(t *testing.T) {
	f := Parse(flowsealDefault)
	if got := f.Render(); got != flowsealDefault {
		t.Fatalf("файл service.bat должен остаться как был:\n%q", got)
	}
	if d := f.UserDomains(); len(d) != 0 {
		t.Fatalf("заглушка — не домен пользователя: %v", d)
	}
}

func TestServicesBlock(t *testing.T) {
	f := Parse(flowsealDefault + "my.site\r\n")
	if !f.SetService("instagram", []string{"instagram.com", "cdninstagram.com"}) {
		t.Fatal("сервис должен добавиться")
	}
	if !f.SetService("hosting", nil) || f.SetService("hosting", nil) { // сервис без доменов — только отметка
		t.Fatal("новый сервис без доменов — изменение, повтор — нет")
	}
	out := f.Render()
	want := flowsealDefault + "my.site\r\n\r\n" + Begin + "\r\n" +
		"# service: instagram\r\ninstagram.com\r\ncdninstagram.com\r\n" +
		"# service: hosting\r\n" + End + "\r\n"
	if out != want {
		t.Fatalf("получили:\n%s\nждали:\n%s", out, want)
	}
	g := Parse(out)
	if !slices.Equal(g.Services(), []string{"instagram", "hosting"}) || !g.HasService("Instagram") {
		t.Fatalf("сервисы после чтения: %v", g.Services())
	}
	if !slices.Equal(g.UserDomains(), []string{"my.site"}) {
		t.Fatalf("свои домены: %v", g.UserDomains())
	}
	if g.SetService("instagram", []string{"instagram.com", "cdninstagram.com"}) {
		t.Fatal("те же домены — не изменение")
	}
	if !g.RemoveService("instagram") || g.RemoveService("instagram") {
		t.Fatal("удаление сервиса")
	}
	g.RemoveService("hosting")
	if got := g.Render(); got != flowsealDefault+"my.site\r\n" {
		t.Fatalf("без сервисов блока быть не должно:\n%q", got)
	}
}

func TestAddRemoveDomain(t *testing.T) {
	f := Parse(flowsealDefault)
	if !f.AddDomain("example.com") || f.AddDomain("EXAMPLE.com") {
		t.Fatal("добавление без повторов")
	}
	if !f.RemoveDomain("example.com") || f.RemoveDomain("example.com") {
		t.Fatal("удаление")
	}
	// удалили всё, включая заглушку, — файл не должен стать пустым
	f.RemoveDomain(Placeholder)
	if out := f.Render(); !strings.Contains(out, Placeholder) {
		t.Fatalf("пустой список недопустим:\n%q", out)
	}
}

func TestParseKeepsForeignLines(t *testing.T) {
	in := "\uFEFF# мои\r\n  spaced.example  \r\n^exact.example\r\n\r\n" +
		Begin + "\r\nstray.example\r\n# service: x\r\nx.com\r\nx.com\r\n# service: BAD ID!\r\nlost.example\r\n" + End + "\r\n" +
		"after.example\r\n"
	f := Parse(in)
	for _, want := range []string{"spaced.example", "^exact.example", "stray.example", "lost.example", "after.example"} {
		if !slices.Contains(f.UserDomains(), want) {
			t.Errorf("потеряна строка %q: %v", want, f.UserDomains())
		}
	}
	if s := f.Services(); !slices.Equal(s, []string{"x"}) || len(f.Sections[0].Domains) != 1 {
		t.Fatalf("разделы: %+v", f.Sections)
	}
}

func TestOrphanMarkersDropped(t *testing.T) {
	for _, in := range []string{
		Begin + "\r\nkeep.example\r\n",            // нет конца блока
		End + "\r\nkeep.example\r\n",              // конец без начала
		"  " + Begin + "  \nkeep.example\n" + End, // отступы вокруг маркеров
	} {
		f := Parse(in)
		out := f.Render()
		if !strings.Contains(out, "keep.example") {
			t.Errorf("%q: строка потеряна:\n%q", in, out)
		}
		if f.HasService("keep") {
			t.Errorf("%q: появился сервис", in)
		}
	}
}

func TestNormalizeDomain(t *testing.T) {
	ok := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"  Example.COM. ", "example.com"}, // пробелы вокруг — так вставляют из буфера
		{"https://www.example.com/a?b=1#c", "example.com"},
		{"http://user@sub.example.com:8080/", "sub.example.com"},
		{"www.example.com", "example.com"},
		{"www.www.example.com", "example.com"},
		{"www.com", "www.com"},
		{"*.example.com", "example.com"},
		{"example.com:443", "example.com"},
		{"xn--e1afmkfd.xn--p1ai", "xn--e1afmkfd.xn--p1ai"},
		{"a-b.c0m", "a-b.c0m"},
	}
	for _, c := range ok {
		in, want := c.in, c.want
		got, err := NormalizeDomain(in)
		if err != nil || got != want {
			t.Errorf("NormalizeDomain(%q) = %q, %v; ждали %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "localhost", "1.2.3.4", "[::1]:80", "::1", "пример.рф", "-a.com", "a-.com",
		"a..com", "a.123", "exa mple.com", "a_b.com", "https://", strings.Repeat("a", 64) + ".com",
		"x\r\ny.com", "#comment.com",
	} {
		if got, err := NormalizeDomain(in); err == nil {
			t.Errorf("NormalizeDomain(%q) = %q — должна быть ошибка", in, got)
		}
	}
}

// FuzzRender: запись идемпотентна, файл не пустой, чужие записи не теряются.
func FuzzRender(f *testing.F) {
	f.Add(flowsealDefault)
	f.Add(Begin + "\n# service: a\na.com\n" + End + "\n" + Begin + "\nb.com\n")
	f.Add("x.com\r" + End + "\n\n\n# service: q\n")
	f.Add("\uFEFF" + Begin + "\r\n# service:\r\n#\r\n" + End)
	f.Fuzz(func(t *testing.T, in string) {
		p := Parse(in)
		out := p.Render()
		again := Parse(out).Render()
		if again != out {
			t.Fatalf("не идемпотентно для %q:\n%q\n%q", in, out, again)
		}
		if !Parse(out).hasEntries() {
			t.Fatalf("пустой список для %q: %q", in, out)
		}
		markers := 0 // маркер — только строка целиком, подстрока в чужой строке не считается
		for l := range strings.SplitSeq(out, "\r\n") {
			if isMarker(strings.TrimSpace(l)) {
				markers++
			}
		}
		if markers != 0 && markers != 2 {
			t.Fatalf("блок не один или без пары маркеров: %q", out)
		}
		for _, d := range p.UserDomains() {
			if !strings.Contains(out, d) {
				t.Fatalf("потеряна запись %q: %q", d, out)
			}
		}
	})
}

func FuzzNormalizeDomain(f *testing.F) {
	for _, s := range []string{"example.com", "https://www.example.com/x", "1.2.3.4", "пример.рф", "a:b:c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		d, err := NormalizeDomain(in)
		if err != nil {
			return
		}
		// результат идёт строкой в hostlist: только [a-z0-9.-], и он устойчив
		for i := range len(d) {
			c := d[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '-' {
				t.Fatalf("%q → %q: символ %q", in, d, c)
			}
		}
		if d2, err := NormalizeDomain(d); err != nil || d2 != d {
			t.Fatalf("%q → %q → %q, %v", in, d, d2, err)
		}
	})
}

// Найдено fuzzing'ом (testdata/fuzz/FuzzRender/600235c6da64129d): маркер внутри чужой
// строки — не маркер. Строка остаётся своей, блока не появляется.
func TestMarkerSubstringIsUserLine(t *testing.T) {
	line := Begin + "0" + Begin + "0"
	f := Parse(line)
	if len(f.Sections) != 0 || !slices.Equal(f.Lines, []string{line}) {
		t.Fatalf("%+v", f)
	}
	// строка — комментарий, записей нет: добавляется заглушка, как в service.bat
	if out := f.Render(); out != line+"\r\n"+Placeholder+"\r\n" {
		t.Fatalf("%q", out)
	}
}
