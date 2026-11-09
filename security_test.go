package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// restoreUserAccess возвращает текущему пользователю полный доступ к папке, чтобы
// t.TempDir смог её удалить. Владелец всегда может менять DACL.
func restoreUserAccess(t *testing.T, dir string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:PAI(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSecureTreeLocksOutNonAdmins(t *testing.T) {
	root := filepath.Join(t.TempDir(), "zapret")
	child := filepath.Join(root, "releases", "1.0", "bin", "winws.exe")
	if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, []byte("exe"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Явный ACE «Все: полный доступ» на вложенном файле — как после подкладывания атакующим.
	sd, _ := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	everyone, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, everyone, nil); err != nil {
		t.Fatal(err)
	}
	if rep, err := checkDirACL(root); err != nil || rep.ok() {
		t.Fatalf("папка во временном каталоге пользователя должна считаться открытой на запись: %+v %v", rep, err)
	}

	elevated := isElevated()
	t.Cleanup(func() { restoreUserAccess(t, root) })
	if err := secureTree(root, elevated); err != nil {
		t.Fatal(err)
	}

	rep, err := checkDirACL(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Writers) != 0 {
		t.Fatalf("после secureTree писать могут: %v", rep.Writers)
	}
	if elevated && rep.Owner != "" {
		t.Fatalf("владелец не администраторы: %s", rep.Owner)
	}
	if childRep, err := checkDirACL(child); err != nil || len(childRep.Writers) != 0 {
		t.Fatalf("явный ACE на вложенном файле не сброшен: %+v %v", childRep, err)
	}
	if !elevated {
		// процесс без прав администратора больше не может подменить файл
		if err := os.WriteFile(child, []byte("evil"), 0o644); err == nil {
			t.Fatal("запись от обычного пользователя должна быть запрещена")
		}
		if err := os.WriteFile(filepath.Join(root, "new.exe"), []byte("x"), 0o644); err == nil {
			t.Fatal("создание файлов от обычного пользователя должно быть запрещено")
		}
	}
	if data, err := os.ReadFile(child); err != nil || string(data) != "exe" {
		t.Fatalf("чтение должно остаться: %q %v", data, err)
	}
}

func TestSecureTreeRefusesFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := secureTree(f, false); err == nil {
		t.Fatal("файл вместо папки должен отвергаться")
	}
}

func TestRootIsDedicated(t *testing.T) {
	root := t.TempDir()
	ok, foreign, err := rootIsDedicated(filepath.Join(root, "missing"))
	if err != nil || !ok || len(foreign) != 0 {
		t.Fatalf("несуществующая папка подходит: %v %v %v", ok, foreign, err)
	}
	for _, n := range []string{"zapret-manager.exe", "config.json", "state.json.corrupt-1", "releases", "logs", ".lock"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ok, foreign, _ := rootIsDedicated(root); !ok {
		t.Fatalf("только наши файлы: %v", foreign)
	}
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, foreign, _ := rootIsDedicated(root); ok || len(foreign) != 1 || foreign[0] != "photo.jpg" {
		t.Fatalf("чужой файл: %v %v", ok, foreign)
	}
}

func TestACLReportString(t *testing.T) {
	r := aclReport{Writers: []string{`NT AUTHORITY\Authenticated Users`}, Owner: `PC\user`}
	if r.ok() || r.String() != `запись разрешена: NT AUTHORITY\Authenticated Users; владелец PC\user` {
		t.Fatal(r.String())
	}
}

// Ссылка (junction) внутри папки на чужую папку: права цели не меняются.
func TestSecureTreeDoesNotFollowJunction(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "zapret")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sd, _ := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	everyone, _, _ := sd.DACL()
	victim := filepath.Join(outside, "victim.txt")
	if err := windows.SetNamedSecurityInfo(victim, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, everyone, nil); err != nil {
		t.Fatal(err)
	}
	cmdExe, err := systemBinary("cmd.exe")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runHidden(cmdExe, "/c", "mklink", "/J", filepath.Join(root, "link"), outside); err != nil {
		t.Skipf("junction не создать: %v %s", err, out)
	}
	t.Cleanup(func() { restoreUserAccess(t, root) })
	_ = secureTree(root, isElevated()) // ошибка «ссылка пропущена» ожидаема
	rep, err := checkDirACL(victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Writers) == 0 {
		t.Fatal("secureTree прошёл по ссылке и сбросил права на файл вне папки")
	}
	// Гонка: папку подменили ссылкой уже после обхода — путь ведёт через junction.
	rootFinal, err := finalPathOf(root)
	if err != nil {
		t.Fatal(err)
	}
	err = resetChildSecurity(filepath.Join(root, "link", "victim.txt"), rootFinal, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.DACL_SECURITY_INFORMATION, nil, nil)
	if !errors.Is(err, errOutsideRoot) {
		t.Fatalf("объект за ссылкой должен отвергаться, получено %v", err)
	}
}

func TestRootIsDedicatedWithRelease(t *testing.T) {
	root := t.TempDir()
	makeFakeRelease(t, root)
	if ok, foreign, err := rootIsDedicated(root); err != nil || !ok {
		t.Fatalf("распакованный релиз в корне подходит: %v %v %v", ok, foreign, err)
	}
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, foreign, _ := rootIsDedicated(root); ok || len(foreign) != 1 {
		t.Fatalf("релиз не делает чужие файлы своими: %v %v", ok, foreign)
	}
}

// makeFakeRelease кладёт в dir минимальный релиз, который принимает validRelease.
func makeFakeRelease(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		`bin\winws.exe`: "x", `bin\WinDivert.dll`: "x", `bin\WinDivert64.sys`: "x",
		"service.bat": "@echo off\r\n", "README.md": "readme", "LICENSE.txt": "mit",
		"general.bat":            `start "" "%~dp0bin\winws.exe" --wf-tcp=80` + "\r\n",
		`lists\list-general.txt`: "discord.com\r\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !validRelease(dir) {
		t.Fatal("заготовка релиза не принята validRelease")
	}
}

// Корень закрыт, но на exe службы кто-то дал себе запись — это тоже непорядок.
func TestRootACLProblemSeesCriticalChild(t *testing.T) {
	a := testApp(t)
	a.Root = filepath.Join(a.Root, "zapret")
	exe := a.path("zapret-manager.exe")
	if err := os.MkdirAll(a.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restoreUserAccess(t, a.Root) })
	if err := secureTree(a.Root, isElevated()); err != nil {
		t.Fatal(err)
	}
	// без прав администратора владельцем остаётся пользователь — смотрим только на запись
	if rep, err := a.rootACLProblem(); err != nil || len(rep.Writers) != 0 {
		t.Fatalf("после secureTree всё в порядке: %+v %v", rep, err)
	}
	sd, _ := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	everyone, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(exe, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, everyone, nil); err != nil {
		t.Skipf("не изменить DACL exe: %v", err)
	}
	if rep, err := a.rootACLProblem(); err != nil || len(rep.Writers) == 0 {
		t.Fatalf("явный ACE на exe не замечен: %+v %v", rep, err)
	}
}
