# Безопасность

- [THREAT_MODEL.md](THREAT_MODEL.md) — активы, границы доверия, STRIDE, остаточные риски.
- [../engineering/SECURITY_AUDIT.md](../engineering/SECURITY_AUDIT.md) — найденные проблемы и исправления.
- [../../SECURITY.md](../../SECURITY.md) — как сообщить об уязвимости.
- [../ai/SECURITY_RULES.md](../ai/SECURITY_RULES.md) — правила для изменений кода и для AI-агентов.

## Механизмы защиты в коде

| Механизм | Где |
|---|---|
| ACL корневой папки: SYSTEM/Администраторы — полный, Users — чтение; владелец — Администраторы | `security.go` (`secureTree`, `checkDirACL`, `ensureRootSecure`) |
| Проверка «своя папка» перед закрытием | `rootIsDedicated` |
| Валидация всего, что идёт в командную строку службы | `internal/batch`, `internal/zapretfmt` |
| Безопасная распаковка | `internal/safezip` |
| TLS 1.2+, https-only редиректы, лимиты размера, ограниченные повторы | `release.go` (`downloader`) |
| Проверка repo/branch/targets из config.json | `Config.normalize` |
| Системные программы по абсолютному пути, с таймаутом | `systemBinary`, `runHidden` |
| hosts: копия перед записью, только свой блок | `hosts.go`, `internal/hostsfile` |
| Без recovery actions у службы zapret, откат binPath | `winsys.go`, `engine.go` |

## Проверки в CI

`golangci-lint` (включает gosec с обоснованными исключениями в `.golangci.yml`),
`govulncheck`, fuzz-прогоны всех парсеров, `go mod verify`. Локально — `scripts\check.ps1`.
Поиск секретов — `gitleaks git .` (ручной запуск; секретов в проекте нет).
