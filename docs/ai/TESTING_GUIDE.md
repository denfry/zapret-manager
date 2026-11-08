# Тесты

## Уровни

| Уровень | Где | Что нужно | Запуск |
|---|---|---|---|
| unit | `internal/*/*_test.go`, `engine_test.go`, `app_test.go`, `sites_test.go`, `menu_test.go` | ничего | `go test ./...` |
| fuzz | `Fuzz*` в `internal/*` (10 целей) | ничего | `go test -run '^$' -fuzz '^FuzzMerge$' -fuzztime 30s ./internal/hostsfile` |
| сеть (локально) | `health_test.go`, `release_test.go`, `update_test.go` | ничего — `httptest`/`127.0.0.1` | `go test ./...` |
| ОС (ACL) | `security_test.go` | Windows; без админа проверяет ещё и запрет записи | `go test -run TestSecureTree .` |
| интеграция с релизом | `TestRealStrategies` | распакованный релиз (`ZAPRET_TEST_DIR` или Desktop) | skip без релиза |
| интеграция со службой | `TestMatchesServiceBatInstall` | установленная служба `zapret` | skip без службы |
| сеть (интернет) | `TestNetworkUpdateFlow` | интернет, `ZM_NET=1` | `ZM_NET=1 go test -run TestNetworkUpdateFlow -v .` |
| race | все | gcc (cgo) | `CGO_ENABLED=1 go test -race ./...` (в CI) |
| бенчмарк | `BenchmarkReleaseDownload` | ничего | `go test -run '^$' -bench ReleaseDownload .` |

## Что моделируют сетевые тесты

`health_test.go` воспроизводит поведение DPI на локальном сервере: заморозка тела на 20 КБ
(`freeze`), RST на ClientHello (`reset`), молчащий TLS (`tls-timeout`), сервер без ответа
(`no-response`), чужой сертификат (`cert`), отказ в соединении. `update_test.go` поднимает
«GitHub» на `httptest` и проходит весь поток обновления: version.txt → zip → распаковка →
перенос настроек → ipset → списки → очистка релизов, плюс битые архивы и мусор вместо списков.

## Правила

- Тест проверяет поведение, а не повторяет реализацию. Для решений (`summarize`,
  `releaseIsWorse`, `sortRanking`) — табличные тесты с объяснением каждого случая.
- Никаких реальных служб, hosts, реестра и сети в обычных тестах. Системные проверки — только
  со `t.Skip`, если окружения нет, и только чтение.
- Временные файлы — `t.TempDir()`; если тест меняет ACL — верни доступ в `t.Cleanup`
  (см. `restoreUserAccess`).
- Сетевые тесты — только `127.0.0.1`, таймауты ≤ нескольких секунд, без `time.Sleep` для
  синхронизации (каналы, `t.Cleanup`).
- Найденный fuzzing'ом контрпример остаётся в `testdata/fuzz/` и получает именованный
  регрессионный тест.
- Проверка на нестабильность: `go test -count=10 -run 'TestCheckTarget|TestDownloader' .`

## Что не покрыто автоматически (и почему)

SCM-операции (`installZapretService`, `startService`, `stopService`), `startWinws`,
`selectStrategy`/`sweep`/`heal` и цикл службы требуют прав администратора, WinDivert и
реальной сети. Их проверяют вручную по сценарию:

1. `zapret-manager install` (UAC) → `status`: служба zapret и сторож работают, папка `C:\zapret`
   закрыта (`doctor` → «права … запись только у администраторов»).
2. `select --dry-run` → таблица стратегий, служба после подбора снова работает.
3. `use ALT11` → `check`; `use` с неработающей стратегией → откат на прежнюю.
4. `off 1` → через минуту сторож включает zapret; `off` → `on`.
5. Остановить службу zapret руками → сторож поднимает её за ≤ 30 с (`log`).
6. Меню: двойной щелчок по exe → один запрос UAC → меню в новом окне, старое закрылось;
   отказ в UAC → меню с предупреждением, пункт «Подобрать» открывает отдельное окно.
   Ctrl+C во время подбора прерывает только подбор, служба zapret снова работает, меню на месте.
   Скопировать exe в другую папку и открыть → первым пунктом «Заменить установленный…».

Логика меню (какие пункты при каком состоянии, разбор ввода, отмены, EOF) покрыта
`menu_test.go` с подменённым `exec`; вызов UAC и консольные режимы — только вручную.
