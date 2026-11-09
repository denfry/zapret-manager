package safezip

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// Папка, записанная архиватором Windows как «top\sub\», — папка, а не пустой файл.
func TestExtractBackslashDirEntry(t *testing.T) {
	z := makeZip(t, map[string]string{`top\sub\`: "", `top\sub\file.txt`: "x"})
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(z, dest, DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "sub", "file.txt")); err != nil || string(data) != "x" {
		t.Fatalf("%q %v", data, err)
	}
}

// Заявленный размер больше лимита — отказ до распаковки, на диск ничего не пишется.
func TestExtractRejectsDeclaredTotalUpFront(t *testing.T) {
	z := makeZip(t, map[string]string{"a.txt": strings.Repeat("a", 600), "b.txt": strings.Repeat("b", 600)})
	dest := filepath.Join(t.TempDir(), "out")
	lim := DefaultLimits
	lim.MaxTotalBytes = 1000
	if err := Extract(z, dest, lim); !errors.Is(err, ErrLimit) {
		t.Fatalf("ждал ErrLimit, получил %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("на диск что-то записано: %v", err)
	}
}

func TestEntryPathDriveRoot(t *testing.T) {
	got, err := EntryPath(`C:\`, "x/file.txt", "")
	if err != nil || got != `C:\x\file.txt` {
		t.Fatalf("%q %v", got, err)
	}
}

func TestExtractStripsTopDir(t *testing.T) {
	p := makeZip(t, map[string]string{
		"zapret-discord-youtube-1.10.5/service.bat":       "x",
		"zapret-discord-youtube-1.10.5/bin/winws.exe":     "x",
		"zapret-discord-youtube-1.10.5/lists/list.txt":    "x",
		"zapret-discord-youtube-1.10.5/general (ALT).bat": "x",
	})
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(p, dest, DefaultLimits); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"service.bat", `bin\winws.exe`, `lists\list.txt`, "general (ALT).bat"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("нет %s: %v", f, err)
		}
	}
}

func TestExtractKeepsRootWhenNoCommonDir(t *testing.T) {
	p := makeZip(t, map[string]string{"service.bat": "x", "bin/winws.exe": "y"})
	dest := filepath.Join(t.TempDir(), "out")
	if err := Extract(p, dest, DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "winws.exe")); err != nil {
		t.Fatal(err)
	}
}

func TestExtractRejectsHostileNames(t *testing.T) {
	for _, name := range []string{
		"top/../../evil.txt",
		"top/a/../../../evil.txt",
		`top\..\..\evil.txt`,
		"top/C:/Windows/evil.txt",
		"top/file.txt:stream",
		"top/NUL",
		"top/dir./x",
		"top/a /x",
	} {
		p := makeZip(t, map[string]string{"top/ok.txt": "x", name: "x"})
		dest := filepath.Join(t.TempDir(), "out")
		if err := Extract(p, dest, DefaultLimits); err == nil {
			t.Errorf("%q должен отвергаться", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); err == nil {
			t.Errorf("%q: файл записан вне папки распаковки", name)
		}
	}
}

func TestExtractLimits(t *testing.T) {
	big := strings.Repeat("A", 2048)
	cases := []struct {
		name  string
		files map[string]string
		lim   Limits
	}{
		{"file too big", map[string]string{"t/a": big}, Limits{MaxFiles: 10, MaxFileBytes: 1024, MaxTotalBytes: 1 << 20}},
		{"total too big", map[string]string{"t/a": big, "t/b": big}, Limits{MaxFiles: 10, MaxFileBytes: 4096, MaxTotalBytes: 3000}},
		{"too many files", map[string]string{"t/a": "1", "t/b": "2", "t/c": "3"}, Limits{MaxFiles: 2, MaxFileBytes: 10, MaxTotalBytes: 100}},
	}
	for _, c := range cases {
		p := makeZip(t, c.files)
		err := Extract(p, filepath.Join(t.TempDir(), "out"), c.lim)
		if !errors.Is(err, ErrLimit) {
			t.Errorf("%s: want ErrLimit, got %v", c.name, err)
		}
	}
}

func TestExtractCorruptArchive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(p, []byte("<html>not a zip</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Extract(p, filepath.Join(t.TempDir(), "out"), DefaultLimits); err == nil {
		t.Fatal("битый архив должен давать ошибку")
	}
}

// FuzzEntryPath: whatever the entry name, an accepted path stays strictly
// inside dest.
func FuzzEntryPath(f *testing.F) {
	for _, s := range []string{"top/a.txt", "top/../x", `..\x`, "top/C:x", "/abs", "top//./b", "top/a/..", "...."} {
		f.Add(s)
	}
	dest := filepath.Join(os.TempDir(), "zm-fuzz-dest")
	root := strings.ToLower(filepath.Clean(dest) + string(os.PathSeparator))
	f.Fuzz(func(t *testing.T, name string) {
		p, err := EntryPath(dest, name, "top/")
		if err != nil || p == "" {
			return
		}
		if !strings.HasPrefix(strings.ToLower(p), root) {
			t.Fatalf("%q escaped to %q", name, p)
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil || !filepath.IsLocal(rel) {
			t.Fatalf("%q → %q (rel %q, %v)", name, p, rel, err)
		}
	})
}
