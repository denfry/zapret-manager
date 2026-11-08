package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Права на корневую папку (CWE-732).
//
// Папка, созданная в корне диска, наследует от C:\ ACE «Прошедшие проверку: изменение».
// Тогда любой пользователь без прав администратора может подменить
// zapret-manager.exe (служба LocalSystem), winws.exe, стратегии или config.json и
// получить SYSTEM. Поэтому корневая папка получает защищённый DACL и владельца
// «Администраторы», а всё внутри — только унаследованные права.

// rootSDDL: SYSTEM и администраторы — полный доступ, пользователи — чтение и запуск
// (0x1200a9 = FILE_GENERIC_READ | FILE_GENERIC_EXECUTE). P — DACL не наследуется
// от родителя, OICI — ACE применяются к вложенным файлам и папкам.
const rootSDDL = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"

// fileDeleteChild — FILE_DELETE_CHILD (в x/sys/windows не объявлена).
const fileDeleteChild = 0x40

// writeMask — права, позволяющие изменить файл, папку или их права.
const writeMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
	fileDeleteChild | windows.FILE_WRITE_ATTRIBUTES | windows.DELETE |
	windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL

// trustedSIDs — кому можно писать в корневую папку.
var trustedSIDs = []string{
	"S-1-5-18",     // SYSTEM
	"S-1-5-32-544", // Administrators
	"S-1-3-0",      // CREATOR OWNER (раз создавать могут только админы — безопасно)
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464", // TrustedInstaller
}

// aclReport — что не так с правами папки.
type aclReport struct {
	Writers []string // кто, кроме доверенных, может писать (имя или SID)
	Owner   string   // владелец, если он не доверенный
}

func (r aclReport) ok() bool { return len(r.Writers) == 0 && r.Owner == "" }

func (r aclReport) String() string {
	var parts []string
	if len(r.Writers) > 0 {
		parts = append(parts, "запись разрешена: "+strings.Join(r.Writers, ", "))
	}
	if r.Owner != "" {
		parts = append(parts, "владелец "+r.Owner)
	}
	return strings.Join(parts, "; ")
}

func isTrustedSID(sid *windows.SID) bool {
	s := sid.String()
	for _, t := range trustedSIDs {
		if strings.EqualFold(s, t) {
			return true
		}
	}
	return false
}

func sidName(sid *windows.SID) string {
	if account, domain, _, err := sid.LookupAccount(""); err == nil {
		if domain != "" {
			return domain + `\` + account
		}
		return account
	}
	return sid.String()
}

// checkDirACL проверяет DACL и владельца папки. Наследуемые ACE (в том числе
// «только для вложенных») учитываются: они дают права на всё внутри.
func checkDirACL(dir string) (aclReport, error) {
	var rep aclReport
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return rep, err
	}
	if owner, _, err := sd.Owner(); err == nil && owner != nil && !isTrustedSID(owner) {
		rep.Owner = sidName(owner)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return rep, err
	}
	if dacl == nil {
		rep.Writers = append(rep.Writers, "все (пустой DACL)")
		return rep, nil
	}
	seen := map[string]bool{}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return rep, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&writeMask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if isTrustedSID(sid) || seen[sid.String()] {
			continue
		}
		seen[sid.String()] = true
		rep.Writers = append(rep.Writers, sidName(sid))
	}
	return rep, nil
}

// secureTree ставит корню защищённый DACL (и владельца «Администраторы», если
// setOwner), а всем вложенным объектам сбрасывает явные ACE и владельца, чтобы
// остались только унаследованные права — как icacls /reset /T плюс takeown.
// Ссылки (symlink, junction) не обходит и не трогает: через них можно было бы
// поменять права на чужие объекты.
func secureTree(root string, setOwner bool) error {
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return fmt.Errorf("%s — не обычная папка (ссылка или файл), права не меняю", root)
	}
	sd, err := windows.SecurityDescriptorFromString(rootSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if !setOwner {
		owner = nil
	} else {
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("права на %s: %w", root, err)
	}

	// Пустой DACL без флага защиты = «только унаследованное». (ACLFromEntries
	// с пустым списком в x/sys падает, поэтому берём пустой DACL из SDDL.)
	emptySD, err := windows.SecurityDescriptorFromString("D:")
	if err != nil {
		return err
	}
	empty, _, err := emptySD.DACL()
	if err != nil {
		return err
	}
	childInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if setOwner {
		childInfo |= windows.OWNER_SECURITY_INFORMATION
	}
	var errs []error
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			errs = append(errs, fmt.Errorf("%s — ссылка, пропущена", p))
			return nil
		}
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, childInfo, owner, nil, empty, nil); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
		return nil
	})
	return errors.Join(walkErr, errors.Join(errs...))
}

// ---------- своя ли это папка ----------

// knownRootEntries — что zapret-manager сам кладёт в корневую папку.
var knownRootEntries = map[string]bool{
	"zapret-manager.exe": true, "config.json": true, "state.json": true,
	"releases": true, "custom": true, "backups": true, "logs": true, "tmp": true,
	".lock": true, ".state.lock": true,
}

// foreignRootEntries — чужие файлы в корневой папке. Временные и испорченные
// копии наших файлов (state.json.*, config.json.*) чужими не считаются.
func foreignRootEntries(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if knownRootEntries[name] || strings.HasPrefix(name, "state.json.") ||
			strings.HasPrefix(name, "config.json.") || strings.HasPrefix(name, "zapret-manager.exe.") {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// rootIsDedicated: папку можно целиком отдать под zapret-manager и закрыть на запись —
// в ней только наши файлы или распакованный релиз zapret (его часто кладут в C:\zapret).
func rootIsDedicated(root string) (bool, []string, error) {
	foreign, err := foreignRootEntries(root)
	if err != nil {
		return false, nil, err
	}
	return len(foreign) == 0 || validRelease(root), foreign, nil
}

// ensureRootSecure вызывается службой при старте: если права на корневую папку
// ослаблены (старая установка или правка руками), исправляет их.
func (a *App) ensureRootSecure() {
	rep, err := checkDirACL(a.Root)
	if err != nil {
		a.Log.Warnf("права на %s не прочитать: %v", a.Root, err)
		return
	}
	if rep.ok() {
		return
	}
	ok, foreign, err := rootIsDedicated(a.Root)
	if err != nil || !ok {
		a.Log.Warnf("папку %s может менять не только администратор (%s), но в ней чужие файлы (%s) — права не трогаю",
			a.Root, rep, strings.Join(foreign, ", "))
		return
	}
	if err := secureTree(a.Root, true); err != nil {
		a.Log.Warnf("права на %s исправлены не полностью: %v", a.Root, err)
		return
	}
	a.Log.Eventf("права на %s исправлены (было: %s) — менять файлы теперь может только администратор", a.Root, rep)
}
