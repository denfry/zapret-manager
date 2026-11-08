package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"zapret-manager/internal/batch"
)

// ---------- службы ----------

type svcInfo struct {
	Exists    bool
	State     svc.State
	PID       uint32
	BinPath   string
	StartType uint32
	ExitCode  uint32
}

func stateName(s svc.State) string {
	switch s {
	case svc.Running:
		return "работает"
	case svc.Stopped:
		return "остановлена"
	case svc.StartPending:
		return "запускается"
	case svc.StopPending:
		return "останавливается (STOP_PENDING)"
	case svc.Paused:
		return "на паузе"
	}
	return fmt.Sprintf("состояние %d", s)
}

// queryService читает состояние и конфиг службы с минимальными правами (работает без админа).
func queryService(name string) svcInfo {
	var info svcInfo
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return info
	}
	defer windows.CloseServiceHandle(h)
	n, _ := windows.UTF16PtrFromString(name)
	sh, err := windows.OpenService(h, n, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return info
	}
	s := &mgr.Service{Name: name, Handle: sh}
	defer s.Close()
	info.Exists = true
	if st, err := s.Query(); err == nil {
		info.State = st.State
		info.PID = st.ProcessId
		info.ExitCode = st.Win32ExitCode
	}
	// QueryServiceConfig отдаёт не больше 8 КБ, а binPath службы zapret — около 10 КБ
	// (RPC_S_INVALID_BOUND). Поэтому binPath и тип запуска читаем из реестра.
	if bin, start, ok := serviceFromRegistry(name); ok {
		info.BinPath, info.StartType = bin, start
	} else if cfg, err := s.Config(); err == nil {
		info.BinPath, info.StartType = cfg.BinaryPathName, cfg.StartType
	}
	return info
}

func serviceFromRegistry(name string) (bin string, start uint32, ok bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+name, registry.QUERY_VALUE)
	if err != nil {
		return "", 0, false
	}
	defer k.Close()
	bin, _, err = k.GetStringValue("ImagePath")
	if err != nil {
		return "", 0, false
	}
	if v, _, err := k.GetIntegerValue("Start"); err == nil && v <= math.MaxUint32 {
		start = uint32(v)
	}
	return bin, start, true
}

// changeService меняет конфиг без предварительного чтения (см. выше про 8 КБ).
// nil и SERVICE_NO_CHANGE — «не трогать».
func changeService(s *mgr.Service, startType uint32, binPath, displayName, description string) error {
	var bin, disp *uint16
	if binPath != "" {
		bin, _ = windows.UTF16PtrFromString(binPath)
	}
	if displayName != "" {
		disp, _ = windows.UTF16PtrFromString(displayName)
	}
	svcType, errCtl := uint32(windows.SERVICE_NO_CHANGE), uint32(windows.SERVICE_NO_CHANGE)
	if binPath != "" {
		svcType, errCtl = windows.SERVICE_WIN32_OWN_PROCESS, windows.SERVICE_ERROR_NORMAL
	}
	if err := windows.ChangeServiceConfig(s.Handle, svcType, startType, errCtl, bin, nil, nil, nil, nil, nil, disp); err != nil {
		return err
	}
	if description != "" {
		d, _ := windows.UTF16PtrFromString(description)
		sd := windows.SERVICE_DESCRIPTION{Description: d}
		_ = windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_DESCRIPTION, (*byte)(unsafe.Pointer(&sd)))
	}
	return nil
}

func connectSCM() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("нет доступа к диспетчеру служб (нужны права администратора): %w", err)
	}
	return m, nil
}

func isNoService(err error) bool {
	return errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
}

// stopService останавливает службу; если зависла в STOP_PENDING — добивает процесс.
func stopService(name string, timeout time.Duration) error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		if isNoService(err) {
			return nil
		}
		return err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	pid := st.ProcessId
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		// служба не принимает Stop — попробуем убить процесс
		if pid != 0 {
			_ = killPID(pid)
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err = s.Query(); err == nil && st.State == svc.Stopped {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	if pid != 0 {
		_ = killPID(pid)
		time.Sleep(time.Second)
		if st, err = s.Query(); err == nil && st.State == svc.Stopped {
			return nil
		}
	}
	return fmt.Errorf("служба %s не остановилась за %s", name, timeout)
}

// startTimeout — сколько ждать перехода службы в RUNNING.
const startTimeout = 15 * time.Second

// startService запускает службу и ждёт, пока она реально заработает (winws с кривыми
// аргументами падает сразу — это тоже ловим).
func startService(name string) error {
	timeout := startTimeout
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("служба %s не запустилась: %w", name, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil {
			return err
		}
		switch st.State {
		case svc.Running:
			// winws падает через долю секунды, если что-то не так с аргументами или WinDivert
			time.Sleep(1500 * time.Millisecond)
			st2, err := s.Query()
			if err == nil && st2.State != svc.Running {
				return fmt.Errorf("служба %s сразу остановилась (код %d)", name, st2.Win32ExitCode)
			}
			return nil
		case svc.Stopped:
			return fmt.Errorf("служба %s остановилась при запуске (код %d)", name, st.Win32ExitCode)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("служба %s не запустилась за %s", name, timeout)
}

func setServiceStartType(name string, startType uint32) error { //nolint:unparam // generic SCM helper like start/stopService
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return changeService(s, startType, "", "", "")
}

func deleteService(name string) error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		if isNoService(err) {
			return nil
		}
		return err
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	return s.Delete()
}

const strategyRegValue = "zapret-discord-youtube" // то же значение пишет service.bat

// installZapretService создаёт или перенастраивает службу zapret (как Install Service в
// service.bat, но через ChangeServiceConfig — без «служба помечена для удаления»).
func installZapretService(cmdline, strategyName string) error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(zapretSvcName)
	if err != nil {
		if !isNoService(err) {
			return err
		}
		name, _ := windows.UTF16PtrFromString(zapretSvcName)
		bin, _ := windows.UTF16PtrFromString(cmdline)
		h, err := windows.CreateService(m.Handle, name, name, windows.SERVICE_ALL_ACCESS,
			windows.SERVICE_WIN32_OWN_PROCESS, windows.SERVICE_AUTO_START, windows.SERVICE_ERROR_NORMAL,
			bin, nil, nil, nil, nil, nil)
		if err != nil {
			return fmt.Errorf("не создать службу zapret: %w", err)
		}
		s = &mgr.Service{Name: zapretSvcName, Handle: h}
	}
	defer s.Close()
	if err := changeService(s, mgr.StartAutomatic, cmdline, "zapret", "Zapret DPI bypass software (управляется zapret-manager)"); err != nil {
		return fmt.Errorf("не обновить службу zapret: %w", err)
	}
	// Без автоперезапуска от SCM: иначе он поднимет zapret посреди подбора, когда мы его
	// остановили. Упавшую службу через ≤30 с поднимет сторож.
	_ = s.ResetRecoveryActions()
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+zapretSvcName, registry.SET_VALUE)
	if err == nil {
		_ = k.SetStringValue(strategyRegValue, strategyName)
		k.Close()
	}
	return nil
}

func readRegistryStrategy() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+zapretSvcName, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(strategyRegValue)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func installManagerService(exePath string) error {
	m, err := connectSCM()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	bin := `"` + exePath + `" service`
	s, err := m.OpenService(managerSvcName)
	if err != nil {
		if !isNoService(err) {
			return err
		}
		s, err = m.CreateService(managerSvcName, exePath, mgr.Config{
			DisplayName: "Zapret Manager",
			Description: "Следит за zapret: обновляет релиз, ipset, hosts и сам меняет стратегию, когда Discord или YouTube отваливаются",
			StartType:   mgr.StartAutomatic,
		}, "service")
		if err != nil {
			return fmt.Errorf("не создать службу %s: %w", managerSvcName, err)
		}
	}
	defer s.Close()
	if err := changeService(s, mgr.StartAutomatic, bin, "Zapret Manager",
		"Следит за zapret: обновляет релиз, ipset, hosts и сам меняет стратегию, когда Discord или YouTube отваливаются"); err != nil {
		return err
	}
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 120 * time.Second},
	}, 86400)
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
	return nil
}

// removeWinDivert — как в service.bat: при смене папки zapret драйвер должен
// перезагрузиться из новой папки.
func removeWinDivert() {
	m, err := connectSCM()
	if err != nil {
		return
	}
	defer m.Disconnect()
	var touched []string
	for _, name := range []string{"WinDivert", "WinDivert14"} {
		s, err := m.OpenService(name)
		if err != nil {
			continue
		}
		_, _ = s.Control(svc.Stop)
		_ = s.Delete()
		s.Close()
		touched = append(touched, name)
	}
	// пока драйвер выгружается, служба «помечена на удаление», и новый winws не стартует
	// (ERROR_SERVICE_MARKED_FOR_DELETE) — ждём, пока она исчезнет
	deadline := time.Now().Add(10 * time.Second)
	for _, name := range touched {
		for queryService(name).Exists && time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
		}
	}
}

// activeServices — имена и отображаемые имена работающих служб (как `sc query`).
func activeServices() ([][2]string, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, err
	}
	defer windows.CloseServiceHandle(h)
	var needed, returned, resume uint32
	size := uint32(64 * 1024)
	for {
		buf := make([]byte, size)
		err = windows.EnumServicesStatusEx(h, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32,
			windows.SERVICE_ACTIVE, &buf[0], size, &needed, &returned, &resume, nil)
		if errors.Is(err, windows.ERROR_MORE_DATA) && needed > size {
			size = needed + 4096
			resume = 0
			continue
		}
		if err != nil {
			return nil, err
		}
		items := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), returned)
		out := make([][2]string, 0, returned)
		for _, it := range items {
			out = append(out, [2]string{windows.UTF16PtrToString(it.ServiceName), windows.UTF16PtrToString(it.DisplayName)})
		}
		return out, nil
	}
}

// ---------- процессы ----------

func processPIDs(exeName string) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var pids []uint32
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), exeName) {
			pids = append(pids, pe.ProcessID)
		}
	}
	return pids
}

func killPID(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return err
	}
	_, _ = windows.WaitForSingleObject(h, 5000)
	return nil
}

func killProcesses(exeName string) int { //nolint:unparam // mirrors processPIDs(exeName)
	n := 0
	for _, pid := range processPIDs(exeName) {
		if killPID(pid) == nil {
			n++
		}
	}
	return n
}

// winwsProc — winws.exe, запущенный для проверки стратегии. Сидит в Job Object
// с KILL_ON_JOB_CLOSE: если менеджер упадёт посреди подбора, winws умрёт вместе с ним.
type winwsProc struct {
	cmd   *exec.Cmd
	job   windows.Handle
	done  chan struct{}
	mu    sync.Mutex
	lines []string
}

func (p *winwsProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.lines, " | ")
}

func startWinws(cmdline, workDir string, readyTimeout time.Duration) (*winwsProc, error) {
	exe := batch.ExeFromCmdline(cmdline)
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// Без CommandContext: временем жизни процесса управляют Stop и Job Object.
	cmd := exec.Command(exe) //nolint:noctx // lifetime is managed by Stop and the job object
	cmd.Dir = workDir
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       cmdline,
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("не запустить winws.exe: %w", err)
	}
	w.Close()
	p := &winwsProc{cmd: cmd, done: make(chan struct{})}
	if job, err := windows.CreateJobObject(nil, nil); err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err == nil {
			pid := uint32(cmd.Process.Pid) // #nosec G115 -- Windows PIDs are DWORDs
			if ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid); err == nil {
				_ = windows.AssignProcessToJobObject(job, ph)
				windows.CloseHandle(ph)
			}
		}
		p.job = job
	}
	ready := make(chan struct{})
	var once sync.Once
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			p.mu.Lock()
			p.lines = append(p.lines, line)
			if len(p.lines) > 12 {
				p.lines = p.lines[len(p.lines)-12:]
			}
			p.mu.Unlock()
			if strings.Contains(line, "capture is started") {
				once.Do(func() { close(ready) })
			}
		}
		r.Close()
	}()
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	select {
	case <-ready:
		time.Sleep(300 * time.Millisecond)
		return p, nil
	case <-p.done:
		time.Sleep(100 * time.Millisecond) // дочитать вывод
		p.closeJob()
		out := p.output()
		if out == "" {
			out = "без вывода"
		}
		return nil, fmt.Errorf("winws.exe завершился сразу: %s", out)
	case <-time.After(readyTimeout):
		// сообщения нет, но процесс жив — считаем, что поймал трафик
		return p, nil
	}
}

func (p *winwsProc) closeJob() {
	if p.job != 0 {
		windows.CloseHandle(p.job)
		p.job = 0
	}
}

func (p *winwsProc) Stop() {
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
	p.closeJob()
}

// ---------- прочее системное ----------

// systemBinary — полный путь к программе из System32. Поиск по PATH от имени
// администратора или SYSTEM опасен: в PATH бывают папки, куда может писать
// обычный пользователь (подмена netsh.exe — CWE-427).
func systemBinary(name string) (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(sys, name), nil
}

const runHiddenTimeout = 30 * time.Second

func runHidden(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runHiddenTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// enableTCPTimestamps — стратегии с --dpi-desync-fooling=ts без этого не работают (tcp_enable в service.bat).
func enableTCPTimestamps() error {
	netsh, err := systemBinary("netsh.exe")
	if err != nil {
		return err
	}
	out, err := runHidden(netsh, "interface", "tcp", "set", "global", "timestamps=enabled")
	if err != nil {
		return fmt.Errorf("netsh: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func flushDNS() {
	proc := windows.NewLazySystemDLL("dnsapi.dll").NewProc("DnsFlushResolverCache")
	if proc.Find() == nil {
		_, _, _ = proc.Call()
	}
}

func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// launchedFromExplorer — консоль создана только для нас (двойной клик по exe).
func launchedFromExplorer() bool {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	if proc.Find() != nil {
		return false
	}
	var pids [4]uint32
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&pids[0])), 4)
	return n == 1
}

func relaunchElevated(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("Не найти свой exe:", err)
		return 1
	}
	hasPause := false
	for _, a := range args {
		if a == "--pause" {
			hasPause = true
		}
	}
	if !hasPause {
		args = append(args, "--pause")
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	cwd, _ := os.Getwd()
	dir, _ := windows.UTF16PtrFromString(cwd)
	if err := windows.ShellExecute(0, verb, file, params, dir, windows.SW_SHOWNORMAL); err != nil {
		fmt.Println("Нужны права администратора, а запрос UAC не прошёл:", err)
		return 1
	}
	fmt.Println("Команда открыта в отдельном окне с правами администратора.")
	return 0
}

func setupConsole() {
	_ = windows.SetConsoleOutputCP(65001)
	_ = windows.SetConsoleCP(65001)
}
