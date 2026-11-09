# Гид по коду: инварианты и опасные места

## Инварианты (нарушение = регрессия)

| # | Инвариант | Где обеспечен | Чем проверен |
|---|---|---|---|
| I1 | Командная строка службы = то, что выполнил бы `.bat` (переменные, `^`, кавычки) | `internal/batch`, `buildCommandLine` | `TestExtractWinwsArgs`, `TestRealStrategies`, `TestMatchesServiceBatInstall`, `FuzzExtractWinwsArgs` |
| I2 | Неизвестная `%VAR%` — ошибка, не пустая строка | `batch.ExpandVars` | `TestExtractWinwsArgsUnknownVar` |
| I3 | Значения game filter в командной строке — только `[0-9,-]` | `zapretfmt.ValidatePortRanges` | `FuzzParseGameFilter` |
| I4 | Служба zapret не остаётся со сломанным binPath | `applyStrategy` → `rollbackZapret` (прежняя строка, а если службы не было — удаление), `selectStrategy` (defer: после успешной установки winws службы не трогает; поднимает службу, только если она работала — `wasRunning`) | `TestWasRunning`, ручной сценарий; см. TESTING_GUIDE |
| I5 | У службы zapret нет recovery actions | `installZapretService` → `ResetRecoveryActions` | ревью |
| I6 | Корневая папка: писать могут только SYSTEM и администраторы, владелец — администраторы | `secureTree`, `ensureRootSecure`, `install` | `TestSecureTreeLocksOutNonAdmins` |
| I7 | Долгие операции под `.lock`, RMW state.json под `.state.lock` | `withLock`, `updateState` | `TestUpdateStateConcurrent`, `TestFileLockExcludesOtherHolders` |
| I8 | `hostsfile.Merge` идемпотентен, блок один и в конце, чужие строки целы | `internal/hostsfile` | `FuzzMerge`, `TestMerge*` |
| I9 | Перед записью hosts — копия в backups\ (без копии не пишем) | `writeHosts` | ревью |
| I10 | Распаковка не выходит за папку, без ADS/зарезервированных имён, с лимитами | `internal/safezip` | `FuzzEntryPath`, `TestExtract*` |
| I11 | Папку релиза, которая может быть в работе, не удаляем — новая кладётся рядом | `downloadRelease` | `TestUpdateFlowOffline` |
| I12 | Новый релиз откатывается, только если старый был здоров, а новый хуже; без сети не судим | `releaseIsWorse` | `TestReleaseIsWorse` |
| I13 | «Нет интернета» (control недоступны) — ничего не чиним | `summarize`, `periodicCheck` | `TestSummarize` |
| I14 | Любой HTTP-ответ (и 4xx/5xx) = путь открыт | `checkTarget` | `TestCheckTargetHTTPErrorStatusIsReachable` |
| I15 | HTTP: лимит размера, https-редиректы, повтор только временных ошибок | `downloader` | `TestDownloader*` |
| I16 | Блок сервисов в list-general-user.txt: запись идемпотентна, чужие строки не теряются, файл не пустой | `internal/sitelist` | `FuzzRender`, `TestParseKeepsForeignLines` |
| I17 | Домен от пользователя попадает в список только как `[a-z0-9.-]` (без IP, кириллицы, масок) | `sitelist.NormalizeDomain` | `FuzzNormalizeDomain`, `TestNormalizeDomain` |
| I18 | Discord, YouTube и control никогда не пропускаются; прочая группа пропускается ≤ 24 ч после полного подбора, где её не открыла ни одна стратегия | `unfixableGroups`, `skippedGroups` | `TestUnfixableGroups`, `TestSkippedGroups`, `TestSummarizeSkip` |
| I19 | Смена режима ipset не теряет список подсетей: он уходит в `.backup` и возвращается оттуда (или скачивается) | `setIpsetMode` | `TestSetIpsetMode*` |

## Опасные места

- **`winsys.go` / SCM.** binPath zapret ~10 КБ, а `QueryServiceConfig` отдаёт ≤ 8 КБ — поэтому
  binPath читается из реестра (`serviceFromRegistry`) и меняется `ChangeServiceConfig` без
  предварительного чтения (`changeService`). Не переходи на `mgr.Service.Config()/UpdateConfig()`.
- **WinDivert.** При смене папки релиза драйвер нужно выгрузить (`removeWinDivert`) и дождаться
  исчезновения службы, иначе новый winws не стартует (ERROR_SERVICE_MARKED_FOR_DELETE).
- **`killProcesses("winws.exe")`** убивает все winws в системе — так делает и service.bat.
- **Строки и регистр.** Для поиска по `.bat` используй `asciiLower`, не `strings.ToLower`:
  у файлов в CP1251 ToLower меняет длину строки, и индексы съезжают (была паника, найдена fuzzing'ом).
- **Блокировки.** `updateState(fn)`: `fn` не должна вызывать `updateState` и долгие операции.
  `withLock(0, …)` — одна попытка: служба пропускает цикл, если CLI занят.
- **Пути.** Сравнение путей — только `samePath`/`isWithin` (регистронезависимо).
- **Время.** Бесконечная пауза хранится как 9999-01-01 (`PausedUntil.Year() > 3000`).
- **Тесты и живая служба.** `currentInstall`, `applyStrategy`, `restartZapretIfRunning` трогают
  настоящую службу `zapret` на машине разработчика. Логику, которую надо тестировать, держи в
  функциях с явным `relDir` (как `editSiteFiles`, `setIpsetMode`).

## Где бизнес-логика

`engine.go` — единственное место решений «что делать»: `heal`, `selectStrategy`,
`updateRelease`, `releaseIsWorse`. `health.summarize` — решение «здоров ли». Остальные файлы —
механизмы. Новую политику добавляй в engine/health, механизм — в соответствующий файл.

## Что может вызвать регрессии

| Изменение | Риск | Проверить |
|---|---|---|
| разбор .bat | служба не стартует у всех пользователей | `TestRealStrategies` с `ZAPRET_TEST_DIR`, `TestMatchesServiceBatInstall` |
| пороги/классификация в health | ложные подборы (мигает Discord) или пропуск поломок | `TestSummarize`, `TestClassifyErr`, `TestCheckTarget*` |
| каталог сервисов (`siteCatalog`) | лишние подборы, если адрес проверки не открывается сам по себе (свой CA, нужен вход) | `TestCatalogIsValid` + пробная `check` с этими адресами в `targets` во временной папке |
| `unfixableGroups` / TTL | бесконечные подборы ради сервиса, который zapret не открывает, или наоборот — непочиненный сервис | `TestUnfixableGroups`, `TestSkippedGroups`, `TestRehealth` |
| форматы state/config | потеря паузы, BadVersions | `TestStateRoundTrip` |
| ACL | lock-out администратора или LPE | `TestSecureTree*` + ручной `install` |
| hosts | поломка DNS у пользователя | `FuzzMerge` + `ZM_NET=1` предпросмотр |
