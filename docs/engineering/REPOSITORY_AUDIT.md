# Аудит репозитория

Дата: 2026-11-08. База: коммит `479e9d7` (main). Итог работы — [FINAL_REPORT.md](FINAL_REPORT.md).

## Назначение и масштаб

Windows-утилита и служба LocalSystem, управляющая zapret-discord-youtube (Flowseal):
установка службы `zapret`, автообновление релизов/ipset/списков/hosts, проверка доступности
Discord/YouTube и автоподбор стратегии.

| Показатель | До |
|---|---|
| Язык / модуль | Go, `module zapret-manager`, `go 1.26.0`, toolchain go1.26.1 |
| Зависимости | `golang.org/x/sys v0.48.0` |
| Пакеты | 1 (`main`), 10 файлов, ≈ 4 000 строк кода + 470 строк тестов |
| Тесты | 23 функции, покрытие 16,4 %, fuzz — 0 |
| CI / линтеры / сканеры | нет |
| Документация | README.md |
| Docker / БД / миграции / API-сервер | нет (не требуются: desktop-служба Windows) |

## Изученные артефакты

Все `.go`-файлы полностью; `go.mod/go.sum`, `.gitignore`, `build.cmd`, `README.md`, история
git (4 коммита), ACL `C:\zapret` и `C:\` на машине разработчика, работающая служба `zapret`
(стратегия ALT3 из `~/Desktop/zapret-discord-youtube-1.10.4`), релизы Flowseal 1.10.0–1.10.4.

## Базовые проверки (до изменений)

| Проверка | Результат |
|---|---|
| `gofmt -l .` | чисто |
| `go vet ./...` | чисто |
| `go test ./...` | PASS, 16,4 % |
| `staticcheck -checks all` | 1: неиспользуемая `toCRLF` |
| `govulncheck` | **23 достижимые уязвимости stdlib** (crypto/tls, crypto/x509, net/http, net/url, net/textproto, encoding/asn1), исправлены в go1.26.2–go1.26.10 |
| `gosec` | 104 срабатывания; реальных — 0 из тех, что он нашёл; главную проблему (ACL) gosec не видит |
| `golangci-lint` (курируемый набор) | 83: 49 errcheck на Close, 3 nilerr (намеренные), unparam (неиспользуемый результат `updateRelease`), modernize |
| ACL `C:\zapret` | `NT AUTHORITY\Прошедшие проверку:(I)(M)` и `(OI)(CI)(IO)(M)` — **запись для любого пользователя** |

## Сводка находок

Подробно — в тематических отчётах. Критичность: CRITICAL / HIGH / MEDIUM / LOW / INFO.

| ID | Критичность | Кратко | Отчёт | Статус |
|---|---|---|---|---|
| SEC-01 | **HIGH** (CVSS 7.8) | `C:\zapret` доступна на запись всем → подмена бинарника службы LocalSystem | SECURITY_AUDIT | исправлено |
| SEC-02 | MEDIUM | заранее созданная пользователем папка оставляет ему права владельца | SECURITY_AUDIT | исправлено |
| SEC-05 | MEDIUM | 23 уязвимости stdlib в go1.26.1 | SECURITY_AUDIT | исправлено |
| SEC-08 | MEDIUM | паника на `.bat` в CP1251 (DoS сторожа) | SECURITY_AUDIT | исправлено |
| SEC-06 | MEDIUM | распаковка без общих лимитов, молчаливое обрезание файлов | SECURITY_AUDIT | исправлено |
| SEC-03 | MEDIUM | `netsh` ищется через PATH от имени SYSTEM | SECURITY_AUDIT | исправлено |
| REL-01 | MEDIUM | гонка чтения-изменения-записи state.json между CLI и службой | TECHNICAL_DEBT | исправлено |
| NET-01 | MEDIUM | релиз (до 100 МБ) целиком в памяти | NETWORK_AUDIT | исправлено |
| ARC-01 | MEDIUM | «God package»: парсеры недоверенных данных смешаны с системными вызовами, не тестируются отдельно | ARCHITECTURE | исправлено частично |
| TD-01 | LOW | нет CI, линтеров, сканеров | TECHNICAL_DEBT | исправлено |
| … | LOW/INFO | ещё 20+ находок | см. отчёты | |

## Пользовательские сценарии (проверены трассировкой кода)

install → status → check → select/use → update → off/on → doctor → uninstall; служба:
периодическая проверка → починка → подбор → обновление с откатом. Все сценарии описаны в
[docs/architecture/README.md](../architecture/README.md).
