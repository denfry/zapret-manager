package main

import (
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
