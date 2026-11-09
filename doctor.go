package main

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"

	"zapret-manager/internal/hostsfile"
)

var cyrillicRe = regexp.MustCompile(`[А-Яа-яЁё]`)

// uniquePaths — пути без повторов (сравнение без учёта регистра), пустые пропускаются.
func uniquePaths(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		dup := false
		for _, o := range out {
			if samePath(o, p) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// runDoctor — то же, что «Run Diagnostics» в service.bat, плюс VPN-туннели и прокси.
func (a *App) runDoctor() int {
	ok := func(s string) { fmt.Println("  ✓ " + s) }
	warn := func(s string) { fmt.Println("  ! " + s) }
	bad := func(s string) { fmt.Println("  ✗ " + s) }
	problems := 0

	relDir, strategy := a.currentInstall()
	if relDir == "" {
		bad("zapret не установлен (zapret-manager install)")
		problems++
	} else {
		ok(fmt.Sprintf("zapret %s, стратегия %s, папка %s", releaseVersion(relDir), strategy, relDir))
		if cyrillicRe.MatchString(relDir) {
			warn("в пути к zapret кириллица — winws может не найти файлы")
		}
		if od := os.Getenv("OneDrive"); od != "" && isWithin(relDir, od) {
			bad("zapret лежит в OneDrive — перенесите (zapret-manager install кладёт в C:\\zapret)")
			problems++
		}
		// winws.exe и стратегии исполняются от SYSTEM: писать в их папку должен только администратор
		for _, dir := range uniquePaths(a.Root, relDir) {
			if !dirExists(dir) {
				continue
			}
			switch rep, err := checkDirACL(dir); {
			case err != nil:
				warn("права на " + dir + " не прочитать: " + err.Error())
			case !rep.ok():
				bad(fmt.Sprintf("папку %s может менять не только администратор (%s) — это путь к правам SYSTEM; "+
					"zapret-manager install или перезапуск службы-сторожа исправит права на свою папку", dir, rep))
				problems++
			default:
				ok("права на " + dir + " — запись только у администраторов")
			}
		}
	}

	if info := queryService("BFE"); info.State == svc.Running {
		ok("служба Base Filtering Engine работает")
	} else {
		bad("Base Filtering Engine не работает — без неё WinDivert не стартует")
		problems++
	}

	zinfo := queryService(zapretSvcName)
	winws := processPIDs("winws.exe")
	switch {
	case !zinfo.Exists:
		bad("службы zapret нет")
		problems++
	case zinfo.State != svc.Running:
		bad("служба zapret: " + stateName(zinfo.State))
		problems++
	default:
		ok(fmt.Sprintf("служба zapret работает (winws.exe pid %d)", zinfo.PID))
	}
	if len(winws) > 1 {
		bad(fmt.Sprintf("запущено %d winws.exe — закройте general*.bat или другой обход", len(winws)))
		problems++
	}
	if wd := queryService("WinDivert"); wd.Exists && wd.State == svc.Running && len(winws) == 0 {
		warn("WinDivert работает без winws — его держит другая программа (GoodbyeDPI и т.п.)")
	}

	services, err := activeServices()
	if err != nil {
		warn("список служб не прочитать: " + err.Error())
	}
	has := func(substrs ...string) []string {
		var hits []string
		for _, s := range services {
			full := strings.ToLower(s[0] + " " + s[1])
			all := true
			for _, sub := range substrs {
				if !strings.Contains(full, strings.ToLower(sub)) {
					all = false
					break
				}
			}
			if all {
				hits = append(hits, s[0])
			}
		}
		return hits
	}
	for _, name := range []string{"GoodbyeDPI", "discordfix_zapret", "winws1", "winws2"} {
		if h := has(name); len(h) > 0 {
			bad("другой обход в службах: " + strings.Join(h, ", ") + " — конфликтует с zapret")
			problems++
		}
	}
	conflicts := []struct {
		subs []string
		msg  string
	}{
		{[]string{"Killer"}, "службы Killer конфликтуют с zapret"},
		{[]string{"Intel", "Connectivity", "Network"}, "Intel Connectivity Network Service конфликтует с zapret"},
		{[]string{"TracSrvWrapper"}, "Check Point конфликтует с zapret"},
		{[]string{"EPWD"}, "Check Point конфликтует с zapret"},
		{[]string{"SmartByte"}, "SmartByte конфликтует с zapret — отключите в services.msc"},
	}
	for _, c := range conflicts {
		if h := has(c.subs...); len(h) > 0 {
			bad(c.msg + " (" + strings.Join(h, ", ") + ")")
			problems++
		}
	}
	if len(processPIDs("AdguardSvc.exe")) > 0 {
		bad("работает AdGuard — мешает Discord (issue #417)")
		problems++
	}
	var vpn []string
	vpn = append(vpn, has("VPN")...)
	vpn = append(vpn, has("HappService")...)
	if len(vpn) > 0 {
		warn("VPN-службы: " + strings.Join(unique(vpn), ", ") + " — некоторые VPN конфликтуют с zapret")
	}
	if tuns := tunAdapters(); len(tuns) > 0 {
		warn("активные туннели: " + strings.Join(tuns, ", ") + ". Что уходит в туннель, мимо zapret; если Discord идёт напрямую, а VPN перехватывает DNS или маршруты — бывают обрывы")
	}

	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil && v == 1 {
			srv, _, _ := k.GetStringValue("ProxyServer")
			warn("включён системный прокси " + srv + " — браузер и Discord ходят через него, а не напрямую")
		} else {
			ok("системный прокси выключен")
		}
		k.Close()
	}

	if dohInterfaces() > 0 {
		ok("DNS-over-HTTPS в Windows настроен")
	} else {
		warn("DNS-over-HTTPS в Windows не настроен — провайдер может подменять DNS (Параметры → Сеть → DNS → Шифрование)")
	}

	// Только читаем: диагностика систему не меняет. Включает их applyStrategy.
	switch tcpTimestampsState() {
	case "enabled":
		ok("TCP timestamps включены (нужны стратегиям с fooling=ts)")
	case "disabled":
		bad("TCP timestamps выключены — стратегии с fooling=ts не работают; включатся при установке стратегии (use / select)")
		problems++
	default:
		warn("TCP timestamps: состояние не определить (netsh interface tcp show global)")
	}

	if data, err := os.ReadFile(hostsPath()); err == nil {
		if hostsHasYouTube(string(data)) {
			warn("в hosts есть записи youtube.com/youtu.be — они ломают YouTube")
		}
		if strings.Contains(string(data), hostsfile.Begin) {
			ok("блок Flowseal в hosts на месте")
		}
	}

	fmt.Println()
	if problems == 0 {
		fmt.Println("Серьёзных проблем не нашёл.")
	} else {
		fmt.Printf("Найдено проблем: %d.\n", problems)
	}
	return 0
}

func tunAdapters() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp != 0 && isTunnelAdapter(i.Name) {
			out = append(out, i.Name)
		}
	}
	return out
}

// isTunnelAdapter — похоже ли имя сетевого адаптера на VPN/туннель. Сравнивает по
// словам имени: короткие ключи (tun, tap, sing, happ) — только целым словом или со
// счётчиком (tun0), иначе «Teredo Tunneling Pseudo-Interface» выглядел бы как VPN.
func isTunnelAdapter(name string) bool {
	short := []string{"tun", "tap", "sing", "happ"}
	long := []string{"vpn", "wintun", "wireguard", "amnezia", "outline", "nekoray", "v2ray", "clash"}
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, w := range words {
		for _, k := range short {
			if w == k || (strings.HasPrefix(w, k) && strings.Trim(w[len(k):], "0123456789") == "") {
				return true
			}
		}
		for _, k := range long {
			if strings.Contains(w, k) {
				return true
			}
		}
	}
	return false
}

// hostsHasYouTube — есть ли в hosts действующая (не закомментированная) запись для
// youtube.com, его поддоменов или youtu.be.
func hostsHasYouTube(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) < 2 {
			continue
		}
		for _, h := range fields[1:] {
			h = strings.TrimSuffix(h, ".")
			if h == "youtube.com" || strings.HasSuffix(h, ".youtube.com") || h == "youtu.be" || strings.HasSuffix(h, ".youtu.be") {
				return true
			}
		}
	}
	return false
}

func dohInterfaces() int {
	base := `SYSTEM\CurrentControlSet\Services\Dnscache\InterfaceSpecificParameters`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, base, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return 0
	}
	defer k.Close()
	ifaces, _ := k.ReadSubKeyNames(-1)
	n := 0
	for _, iface := range ifaces {
		n += dohUnder(base+`\`+iface, 0)
	}
	return n
}

func dohUnder(path string, depth int) int {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return 0
	}
	defer k.Close()
	n := 0
	if v, _, err := k.GetIntegerValue("DohFlags"); err == nil && v > 0 {
		n++
	}
	if depth < 3 {
		subs, _ := k.ReadSubKeyNames(-1)
		for _, s := range subs {
			n += dohUnder(path+`\`+s, depth+1)
		}
	}
	return n
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// tcpTimestampsState — "enabled", "disabled" или "" (не удалось узнать).
func tcpTimestampsState() string {
	netsh, err := systemBinary("netsh.exe")
	if err != nil {
		return ""
	}
	out, err := runHidden(netsh, "interface", "tcp", "show", "global")
	if err != nil {
		return ""
	}
	return parseTCPTimestamps(out)
}

// parseTCPTimestamps ищет в выводе «netsh interface tcp show global» строку
// «RFC 1323 Timestamps : enabled». Название в локализованной Windows переведено
// (и приходит в OEM-кодировке), поэтому строку ищем по «1323», а значение — по
// английскому слову; иначе "".
func parseTCPTimestamps(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.Contains(line, "1323") {
			continue
		}
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(line[i+1:])) {
		case "enabled":
			return "enabled"
		case "disabled":
			return "disabled"
		}
	}
	return ""
}
