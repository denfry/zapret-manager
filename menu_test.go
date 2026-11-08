package main

import (
	"bufio"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

var testNow = time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local)

func viewNotInstalled() menuView {
	return menuView{Root: `C:\zapret`, Now: testNow}
}

func viewRunning() menuView {
	return menuView{
		Root: `C:\zapret`, ReleaseDir: `C:\zapret\releases\1.10.4`, Version: "1.10.4", Strategy: "general (ALT11)",
		ZapretExists: true, ZapretState: svc.Running, ManagerExists: true, ManagerState: svc.Running,
		LastCheck: &CheckSummary{Time: testNow, Discord: "6/6", YouTube: "4/4", Healthy: true},
		Now:       testNow,
	}
}

func viewPaused() menuView {
	v := viewRunning()
	v.ZapretState = svc.Stopped
	v.PausedUntil = testNow.Add(30 * time.Minute)
	return v
}

// viewForeignZapret — zapret поставлен через service.bat, сторожа ещё нет.
func viewForeignZapret() menuView {
	v := viewRunning()
	v.ManagerExists, v.ManagerState = false, 0
	return v
}

func titles(items []menuItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.title
	}
	return out
}

// key — номер пункта, чьё название начинается с prefix (как его введёт человек).
func key(t *testing.T, v menuView, prefix string) string {
	t.Helper()
	for i, it := range buildMenu(v) {
		if strings.HasPrefix(it.title, prefix) {
			return strconv.Itoa(i + 1)
		}
	}
	t.Fatalf("нет пункта %q в %q", prefix, titles(buildMenu(v)))
	return ""
}

func hasItem(items []menuItem, prefix string) bool {
	return slices.ContainsFunc(items, func(it menuItem) bool { return strings.HasPrefix(it.title, prefix) })
}

func TestBuildMenuNotInstalled(t *testing.T) {
	items := buildMenu(viewNotInstalled())
	if !strings.HasPrefix(items[0].title, "Установить —") {
		t.Fatalf("первым должен идти пункт установки: %q", titles(items))
	}
	for _, p := range []string{"Проверить", "Подобрать", "Выбрать", "Выключить", "Включить", "Обновить", "Удалить"} {
		if hasItem(items, p) {
			t.Errorf("без установки не нужен пункт %q: %q", p, titles(items))
		}
	}
	for _, p := range []string{"Журнал", "Диагностика"} {
		if !hasItem(items, p) {
			t.Errorf("нет пункта %q", p)
		}
	}
}

func TestBuildMenuInstalled(t *testing.T) {
	items := buildMenu(viewRunning())
	if hasItem(items, "Установить") {
		t.Errorf("сторож уже стоит — установка не нужна: %q", titles(items))
	}
	for _, p := range []string{"Проверить", "Подобрать", "Выбрать", "Выключить", "Обновить", "Подробный", "Журнал", "Диагностика", "Удалить"} {
		if !hasItem(items, p) {
			t.Errorf("нет пункта %q: %q", p, titles(items))
		}
	}
	if hasItem(items, "Включить") {
		t.Errorf("zapret работает — «Включить» не нужен")
	}
}

func TestBuildMenuPausedOffersOn(t *testing.T) {
	items := buildMenu(viewPaused())
	if !hasItem(items, "Включить zapret") || hasItem(items, "Выключить") {
		t.Fatalf("на паузе нужен «Включить», а не «Выключить»: %q", titles(items))
	}
	// остановленная (не на паузе) служба — тоже «Включить»
	v := viewRunning()
	v.ZapretState = svc.Stopped
	if !hasItem(buildMenu(v), "Включить zapret") {
		t.Fatalf("служба остановлена — нужен «Включить»")
	}
}

func TestBuildMenuForeignZapretOffersInstall(t *testing.T) {
	items := buildMenu(viewForeignZapret())
	if !strings.HasPrefix(items[0].title, "Установить zapret-manager") {
		t.Fatalf("zapret без сторожа — первым пунктом установка: %q", titles(items))
	}
	if !hasItem(items, "Проверить") || !hasItem(items, "Удалить") {
		t.Fatalf("zapret уже стоит — им можно управлять: %q", titles(items))
	}
}

// Скачали новую версию и открыли её — первым пунктом предлагаем заменить установленную.
func TestBuildMenuOffersReplaceFromOtherExe(t *testing.T) {
	v := viewRunning()
	v.ManagerExe = `C:\zapret\zapret-manager.exe`
	v.SelfExe = `C:\Users\u\Downloads\zapret-manager.exe`
	items := buildMenu(v)
	if !strings.HasPrefix(items[0].title, "Заменить установленный") {
		t.Fatalf("нет пункта замены: %q", titles(items))
	}
	calls, _ := runTestMenu(t, v, "1\n\n\n0\n")
	if len(calls) != 1 || calls[0].name != "install" {
		t.Fatalf("замена должна вызвать install: %+v", calls)
	}

	v.SelfExe = `c:\ZAPRET\zapret-manager.exe` // тот же файл, другой регистр
	if hasItem(buildMenu(v), "Заменить") {
		t.Fatalf("меню открыто из установленного exe — заменять нечем")
	}
	v.SelfExe, v.ManagerExe = `C:\Users\u\Downloads\zapret-manager.exe`, ""
	if hasItem(buildMenu(v), "Заменить") {
		t.Fatalf("путь службы неизвестен — пункт не нужен")
	}
}

func TestStatusLines(t *testing.T) {
	pausedForever := viewPaused()
	pausedForever.PausedUntil = time.Date(9999, 1, 1, 0, 0, 0, 0, time.Local)
	update := viewRunning()
	update.LatestVersion = "1.10.5"
	broken := viewRunning()
	broken.LastCheck = &CheckSummary{Time: testNow, Discord: "0/6", YouTube: "4/4"}
	stopped := viewRunning()
	stopped.ZapretState = svc.Stopped

	cases := []struct {
		name  string
		v     menuView
		label string
		text  string
		mood  mood
	}{
		{"не установлен", viewNotInstalled(), "zapret", "не установлен", moodBad},
		{"работает", viewRunning(), "zapret", "работает — стратегия general (ALT11), версия 1.10.4", moodOK},
		{"остановлена", stopped, "zapret", "служба остановлена", moodBad},
		{"пауза", viewPaused(), "zapret", "выключен до 14:30", moodWarn},
		{"пауза без срока", pausedForever, "zapret", "выключен, пока не включите", moodWarn},
		{"есть обновление", update, "zapret", "версия 1.10.4 (вышла 1.10.5)", moodOK},
		{"сторож", viewRunning(), "автопочинка", "включена", moodOK},
		{"без сторожа", viewForeignZapret(), "автопочинка", "не установлена", moodWarn},
		{"проверка", viewRunning(), "проверка", "Discord 6/6, YouTube 4/4 — всё открывается", moodOK},
		{"проблемы", broken, "проверка", "есть проблемы", moodBad},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, l := range statusLines(c.v) {
				if l.label != c.label {
					continue
				}
				if !strings.Contains(l.text, c.text) || l.mood != c.mood {
					t.Fatalf("%s: %q (mood %d), ждали %q (mood %d)", l.label, l.text, l.mood, c.text, c.mood)
				}
				return
			}
			t.Fatalf("нет строки %q в %+v", c.label, statusLines(c.v))
		})
	}
}

type execCall struct {
	name string
	args []string
}

func runTestMenu(t *testing.T, v menuView, input string) ([]execCall, string) {
	t.Helper()
	var calls []execCall
	var out strings.Builder
	m := &menu{in: bufio.NewReader(strings.NewReader(input)), out: &out, root: `C:\zapret`}
	m.snapshot = func() menuView { return v }
	m.exec = func(name string, args []string) int {
		calls = append(calls, execCall{name, args})
		return 0
	}
	m.strategies = func() ([]strategyRow, time.Time, error) {
		return []strategyRow{
			{Name: "general", Line: "general"},
			{Name: "general (ALT11)", Line: "general (ALT11)", Current: true},
		}, time.Time{}, nil
	}
	done := make(chan int, 1)
	go func() { done <- m.loop() }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("loop вернул %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("меню зависло; вывод:\n%s", out.String())
	}
	return calls, out.String()
}

func TestMenuDispatch(t *testing.T) {
	running, paused, none := viewRunning(), viewPaused(), viewNotInstalled()
	cases := []struct {
		name  string
		v     menuView
		input []string
		want  []execCall
	}{
		{"проверка", running, []string{key(t, running, "Проверить"), "", "0"}, []execCall{{"check", nil}}},
		{"подбор", running, []string{key(t, running, "Подобрать"), "", "", "0"}, []execCall{{"select", nil}}},
		{"подбор: «д»", running, []string{key(t, running, "Подобрать"), "д", "", "0"}, []execCall{{"select", nil}}},
		{"подбор: отмена", running, []string{key(t, running, "Подобрать"), "0", "0"}, nil},
		// y на русской раскладке — «н»: не считаем согласием
		{"подбор: y не согласие", running, []string{key(t, running, "Подобрать"), "y", "0"}, nil},
		{"стратегия", running, []string{key(t, running, "Выбрать"), "2", "", "0"}, []execCall{{"use", []string{"general (ALT11)"}}}},
		{"стратегия: назад", running, []string{key(t, running, "Выбрать"), "", "0"}, nil},
		{"стратегия: нет номера", running, []string{key(t, running, "Выбрать"), "7", "", "0"}, nil},
		{"выключить", running, []string{key(t, running, "Выключить"), "", "", "0"}, []execCall{{"off", nil}}},
		{"выключить на 30", running, []string{key(t, running, "Выключить"), "30", "", "0"}, []execCall{{"off", []string{"30"}}}},
		{"выключить: отмена", running, []string{key(t, running, "Выключить"), "0", "0"}, nil},
		{"выключить: не число", running, []string{key(t, running, "Выключить"), "полчаса", "", "0"}, nil},
		{"включить", paused, []string{key(t, paused, "Включить"), "", "0"}, []execCall{{"on", nil}}},
		{"обновить", running, []string{key(t, running, "Обновить"), "", "0"}, []execCall{{"update", nil}}},
		{"удалить сторожа", running, []string{key(t, running, "Удалить"), "1", "", "", "0"}, []execCall{{"uninstall", nil}}},
		{"удалить всё", running, []string{key(t, running, "Удалить"), "2", "", "", "0"}, []execCall{{"uninstall", []string{"--all"}}}},
		{"удалить: назад", running, []string{key(t, running, "Удалить"), "0", "0"}, nil},
		{"удалить: отмена", running, []string{key(t, running, "Удалить"), "2", "0", "0"}, nil},
		{"установить", none, []string{key(t, none, "Установить"), "", "", "0"}, []execCall{{"install", nil}}},
		{"пустой ввод перерисовывает", running, []string{"", "", "0"}, nil},
		{"BOM в начале ввода", running, []string{string(rune(0xFEFF)) + key(t, running, "Проверить"), "", "0"}, []execCall{{"check", nil}}},
		{"нет такого пункта", running, []string{"99", "abc", "0"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, out := runTestMenu(t, c.v, strings.Join(c.input, "\n")+"\n")
			if !slices.EqualFunc(calls, c.want, func(a, b execCall) bool {
				return a.name == b.name && slices.Equal(a.args, b.args)
			}) {
				t.Fatalf("вызовы %+v, ждали %+v; вывод:\n%s", calls, c.want, out)
			}
		})
	}
}

// Закрытое окно (EOF) посреди вопроса не должно выполнять действие: пустая строка
// в вопросе про паузу значит «выключить без срока», но EOF — не пустая строка.
func TestMenuEOFNeverRunsAction(t *testing.T) {
	running := viewRunning()
	for _, prefix := range []string{"Подобрать", "Выбрать", "Выключить", "Удалить"} {
		t.Run(prefix, func(t *testing.T) {
			calls, out := runTestMenu(t, running, key(t, running, prefix)+"\n")
			if len(calls) != 0 {
				t.Fatalf("после EOF выполнено %+v; вывод:\n%s", calls, out)
			}
		})
	}
	// последняя строка без перевода строки — тоже ответ
	if calls, _ := runTestMenu(t, running, key(t, running, "Проверить")); len(calls) != 1 {
		t.Fatalf("ответ без \\n потерян: %+v", calls)
	}
}

func TestMenuRender(t *testing.T) {
	_, out := runTestMenu(t, viewRunning(), "99\n0\n")
	for _, want := range []string{
		"zapret-manager " + appVersion, `папка C:\zapret`,
		"работает — стратегия general (ALT11)", "Discord 6/6, YouTube 4/4",
		"Без прав администратора", "Нет пункта «99»", "0  Выход",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("без поддержки ANSI escape-последовательностей быть не должно:\n%q", out)
	}
}

func TestParseOffMinutes(t *testing.T) {
	if d, err := parseOffMinutes(" 30 "); err != nil || d != 30*time.Minute {
		t.Fatalf("30: %v %v", d, err)
	}
	if d, err := parseOffMinutes(strconv.Itoa(maxOffMinutes)); err != nil || d != maxOffMinutes*time.Minute {
		t.Fatalf("max: %v %v", d, err)
	}
	for _, s := range []string{"", "0", "-5", "abc", "1.5", strconv.Itoa(maxOffMinutes + 1), "99999999999999"} {
		if _, err := parseOffMinutes(s); err == nil {
			t.Errorf("%q принят", s)
		}
	}
}
