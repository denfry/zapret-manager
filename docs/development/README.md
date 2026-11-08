# Разработка

Правила — [AGENTS.md](../../AGENTS.md) (общие для людей и AI-агентов),
процесс — [CONTRIBUTING.md](../../CONTRIBUTING.md), тесты — [docs/ai/TESTING_GUIDE.md](../ai/TESTING_GUIDE.md).

## Требования

- Windows 10/11 x64.
- Go 1.26 (`go.mod`: `toolchain go1.26.10` — при `GOTOOLCHAIN=auto`, по умолчанию, нужная
  версия скачается сама).
- Для `-race`: gcc (MinGW-w64) в PATH, `CGO_ENABLED=1`. Без gcc race-тесты идут только в CI.
- Права администратора не нужны ни для сборки, ни для тестов.

## Команды

| Задача | Команда |
|---|---|
| сборка | `go build ./...` |
| релизная сборка | `build.cmd` → `dist\zapret-manager.exe` (тесты + `-trimpath -ldflags "-s -w"`) |
| тесты | `go test -count=1 ./...` |
| покрытие | `go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out` |
| fuzz | `go test -run '^$' -fuzz '^FuzzMerge$' -fuzztime 60s ./internal/hostsfile` |
| бенчмарк | `go test -run '^$' -bench ReleaseDownload -benchmem .` |
| всё как в CI | `powershell -ExecutionPolicy Bypass -File scripts\check.ps1` |
| без сети | `scripts\check.ps1 -SkipTools -Fuzz 0` |

Версии инструментов закреплены в `scripts/check.ps1` и `.github/workflows/ci.yml`
(golangci-lint v2.15.0, staticcheck v0.8.2, govulncheck v1.9.0) и запускаются через
`go run …@версия`, поэтому отдельная установка не нужна.

## Структура

```
*.go                 пакет main: CLI, служба, оркестрация, системные вызовы
internal/batch       .bat → аргументы winws
internal/hostsfile   блок hosts
internal/safezip     распаковка релиза
internal/version     версии
internal/zapretfmt   game filter, ipset, списки
internal/textutil    BOM, строки
internal/*/testdata/fuzz  регрессионный корпус fuzz
scripts/check.ps1    локальный quality gate
.github/             CI и Dependabot
docs/                документация (architecture, security, network, development, deployment, ai, engineering)
```

## Стиль

- `gofmt`, `go vet`, `.golangci.yml` — обязательны, 0 замечаний.
- Ошибки оборачиваются `%w`; несколько — `errors.Join`.
- Сообщения пользователю и комментарии — по-русски; идентификаторы — по-английски.
- Комментарии объясняют «почему», а не пересказывают код.
- Новые зависимости — только с записью в `docs/ai/DECISIONS.md`.

## Обновление зависимостей

Dependabot раз в неделю предлагает обновления `golang.org/x/sys` и GitHub Actions.
Toolchain Go обновляется руками: `go get toolchain@go1.26.N`, затем `govulncheck ./...`.
Перед мержем — `scripts\check.ps1`.
