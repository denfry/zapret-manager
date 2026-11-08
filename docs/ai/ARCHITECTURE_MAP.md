# Карта архитектуры

Полное описание — [docs/architecture/README.md](../architecture/README.md). Здесь — сжатая
карта для навигации.

## Слои

```
menu.go → main.go (CLI)  watchdog.go (служба)
        \                  /
         engine.go — бизнес-логика: install/select/heal/update
        /     |      \        \
 strategy.go health.go release.go hosts.go security.go doctor.go sites.go
        \     |      /        /
          winsys.go — SCM, реестр, процессы, Job Object, UAC   app.go — Config/State/locks/log
                 \
   internal/: batch  hostsfile  safezip  sitelist  version  zapretfmt  textutil   (чистые, без x/sys/windows)
```

Зависимости направлены вниз. `internal/*` не импортируют пакет main и друг друга, кроме
`textutil`. Циклов нет (Go их не допускает между пакетами; внутри main их контролирует ревью).

## Точки входа

| Вход | Где | Что вызывает |
|---|---|---|
| `zapret-manager <команда>` | `main.runCLI` | таблица `commands`; команды с `admin: true` перезапускаются через UAC (`relaunchElevated`) |
| служба `zapret-manager` | `main` → `runAsService` → `managerService.Execute` | `App.watchdog` |
| двойной клик | `runCLI` без аргументов → `cmdMenu` (`menu.go`) | один запрос UAC (`runElevated`), затем меню; пункты вызывают те же `cmdX` через `findCommand` |
| `runCLI` без аргументов из консоли | `main.runCLI` | справка + `cmdStatus` |

## Основные потоки

**Плановая проверка (служба, раз в `check_interval_sec`):**
`watchdog` → `periodicCheck` (под `.lock`) → `currentInstall` → поднять службу, если
лежит → `checkNow` → `runHealth(targetsFor(relDir), skippedGroups())` → `summarize` → после
`failures_before_heal` неудач → `heal`: перезапуск → `selectStrategy{quick}` → полный `sweep`
→ `updateRelease`.

**Что проверять:** `targetsFor(relDir)` = стандартные адреса (или `targets` из config.json)
+ проверки включённых сервисов каталога + первые 10 своих доменов из `list-general-user.txt`.
Полный `sweep` считает `unfixableGroups` — группы, которые не открыла ни одна стратегия; они
пишутся в `LastSweep.Unfixable` и сутки не влияют на `Healthy` (`skippedGroups`). Discord,
YouTube и control туда не попадают никогда.

**Что разблокировать (`sites`, пункт меню):** `planSites` (разбор и проверка ввода, до UAC) →
`withLock` → `applySites`: `editSiteFiles` (списки через `sitelist`, режим ipset через
`setIpsetMode`) → `forgetUnfixable` для затронутых групп → `restartZapretIfRunning`; игры —
`setGameFilter` → `applyStrategy` (порты в командной строке, откат файла при неудаче).
Плановое обновление вызывает `syncCatalog` — домены включённых сервисов по каталогу.

**Обновление (служба, раз в `update_interval_hours`; `update`):**
`updateAll` → `updateRelease` (version.txt → `downloadRelease` → `safezip.Extract` →
`migrateUserFiles` → ipset/списки → та же стратегия → замер → `releaseIsWorse` → откат)
→ `updateIpset` → `updateMainLists` → `updateHosts`.

**Смена стратегии (`use`, подбор, обновление):**
`findStrategy` → `buildCommandLine` (`batch.ExtractWinwsArgs` + `zapretfmt.GameFilter`) →
`installZapretService` (ChangeServiceConfig, без пересоздания) → `startService` → при
неудаче откат binPath на прежний.

**Подбор (`select`, `heal`):** остановить службу → для каждой стратегии `startWinws`
(Job Object, kill-on-close) → `runHealth` → `sortRanking` → перепроверка тройки →
`applyStrategy` лучшей; при любой ошибке служба возвращается как была (defer).

## Состояние и файлы

| Файл | Владелец | Доступ |
|---|---|---|
| `C:\zapret\config.json` | пользователь (админ) | перечитывается на каждой проверке |
| `C:\zapret\state.json` | менеджер | RMW только через `updateState` под `.state.lock` |
| `C:\zapret\.lock` | менеджер | долгие операции (подбор, обновление, смена стратегии) |
| `C:\zapret\logs\manager.log` | менеджер | `[ ]` info, `[*]` событие (видно в status), `[!]` warn, `[.]` debug; ротация 2 МБ |
| `releases\<версия>\` | Flowseal + менеджер | релиз как есть; новые версии кладутся рядом, старые чистит `pruneReleases` |
| `releases\<версия>\lists\list-general-user.txt` | пользователь + менеджер (блок между маркерами) | единственное место, где записано, какие сервисы включены; переносится `migrateUserFiles` |
| служба `zapret`, `HKLM\...\Services\zapret` | менеджер / service.bat | источник правды о текущей стратегии |
