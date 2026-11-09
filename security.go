package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	rootFinal, err := finalPathOf(root)
	if err != nil {
		return fmt.Errorf("путь %s: %w", root, err)
	}
	access := uint32(windows.READ_CONTROL | windows.WRITE_DAC)
	if setOwner {
		access |= windows.WRITE_OWNER
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
		if err := resetChildSecurity(p, rootFinal, access, childInfo, owner, empty); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			if errors.Is(err, errOutsideRoot) && d.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	})
	return errors.Join(walkErr, errors.Join(errs...))
}

var errOutsideRoot = errors.New("ссылка или объект вне папки — пропущен")

// resetChildSecurity меняет права по открытому дескриптору, а не по пути: между
// обходом и вызовом папку можно подменить ссылкой (junction), и SYSTEM поменял бы
// права на чужой объект. Дескриптор открывается без перехода по ссылке, а его
// настоящий путь (с учётом ссылок выше по пути) обязан лежать внутри rootFinal.
func resetChildSecurity(p, rootFinal string, access uint32, info windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
	h, err := openNoFollow(p, access)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only handle close
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return err
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errOutsideRoot
	}
	final, err := finalPathByHandle(h)
	if err != nil {
		return err
	}
	if samePath(final, rootFinal) || !isWithin(final, rootFinal) {
		return errOutsideRoot
	}
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil)
}

// openNoFollow открывает файл или папку, не переходя по ссылке в последнем элементе пути.
func openNoFollow(p string, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// finalPathOf — настоящий путь p после всех ссылок.
func finalPathOf(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only handle close
	return finalPathByHandle(h)
}

func finalPathByHandle(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	if int(n) >= len(buf) {
		return "", errors.New("слишком длинный путь")
	}
	p := windows.UTF16ToString(buf[:n])
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(p, `\\?\`), nil
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
// в ней только наши файлы и, может быть, распакованный релиз zapret (его часто кладут
// в C:\zapret). Файлы релиза чужими не считаются, только если это действительно релиз;
// всё прочее (документы, другие программы) — чужое.
func rootIsDedicated(root string) (bool, []string, error) {
	foreign, err := foreignRootEntries(root)
	if err != nil {
		return false, nil, err
	}
	if len(foreign) > 0 && validRelease(root) {
		foreign = slices.DeleteFunc(foreign, isReleaseEntry)
	}
	return len(foreign) == 0, foreign, nil
}

// isReleaseEntry — то, что лежит в корне релиза Flowseal: bin, lists, utils, .service,
// service.bat и стратегии *.bat, README/LICENSE и прочие текстовые файлы.
func isReleaseEntry(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "bin", "lists", "utils", ".service", ".github", ".gitattributes", ".gitignore":
		return true
	}
	ext := filepath.Ext(n)
	return ext == ".bat" || ext == ".cmd" || ext == ".txt" || ext == ".md" || strings.HasPrefix(n, "license")
}

// rootACLProblem проверяет корень и то, что SYSTEM исполняет или читает как настройки:
// явный ACE «пользователь: запись» на вложенном файле корень не показывает.
// Возвращает первый отчёт с посторонними писателями, иначе первый с чужим владельцем,
// иначе пустой.
func (a *App) rootACLProblem() (aclReport, error) {
	paths := []string{a.Root, a.path("zapret-manager.exe"), a.path("config.json"), a.releasesDir(), a.customDir()}
	if relDir, _ := a.currentInstall(); relDir != "" && isWithin(relDir, a.Root) {
		paths = append(paths, relDir, filepath.Join(relDir, "bin"), filepath.Join(relDir, "bin", "winws.exe"))
	}
	var ownerProblem *aclReport
	for i, p := range paths {
		if _, statErr := os.Lstat(p); statErr != nil && i > 0 {
			continue
		}
		r, err := checkDirACL(p)
		if err != nil {
			return r, err
		}
		if len(r.Writers) > 0 {
			return r, nil
		}
		if !r.ok() && ownerProblem == nil {
			ownerProblem = &r
		}
	}
	if ownerProblem != nil {
		return *ownerProblem, nil
	}
	return aclReport{}, nil
}

// ensureRootSecure вызывается службой при старте: если права на корневую папку
// ослаблены (старая установка или правка руками), исправляет их.
func (a *App) ensureRootSecure() {
	rep, err := a.rootACLProblem()
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
