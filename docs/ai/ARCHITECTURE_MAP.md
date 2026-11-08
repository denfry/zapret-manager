# Карта архитектуры

Полное описание — [docs/architecture/README.md](../architecture/README.md). Здесь — сжатая
карта для навигации.

## Слои

```
main.go (CLI)            watchdog.go (служба)
        \                  /
         engine.go — бизнес-логика: install/select/heal/update
        /     |      \        \
 strategy.go health.go release.go hosts.go security.go doctor.go
        \     |      /        /
          winsys.go — SCM, реестр, процессы, Job Object, UAC   app.go — Config/State/locks/log
                 \
   internal/: batch  hostsfile  safezip  version  zapretfmt  textutil   (чистые, без x/sys/windows)
```

Зависимости направлены вниз. `internal/*` не импортируют пакет main и друг друга, кроме
`textutil`. Циклов нет (Go их не допускает между пакетами; внутри main их контролирует ревью).

## Точки входа

| Вход | Где | Что вызывает |
|---|---|---|
| `zapret-manager <команда>` | `main.runCLI` | таблица `commands`; команды с `admin: true` перезапускаются через UAC (`relaunchElevated`) |
| служба `zapret-manager` | `main` → `runAsService` → `managerService.Execute` | `App.watchdog` |
| двойной клик | `runCLI` без аргументов | справка + `cmdStatus`, ожидание Enter |

## Основные потоки

**Плановая проверка (служба, раз в `check_interval_sec`):**
`watchdog` → `periodicCheck` (под `.lock`) → `currentInstall` → поднять службу, если
лежит → `checkNow` → `runHealth` → `summarize` → после `failures_before_heal` неудач →
`heal`: перезапуск → `selectStrategy{quick}` → полный `sweep` → `updateRelease`.

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
| служба `zapret`, `HKLM\...\Services\zapret` | менеджер / service.bat | источник правды о текущей стратегии |
