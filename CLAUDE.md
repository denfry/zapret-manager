# Claude Code

Правила проекта — в AGENTS.md (единственный источник, импортируется ниже).

@AGENTS.md

## Дополнительно для Claude Code

- Оболочка — Windows. Для PowerShell-скриптов: `powershell -ExecutionPolicy Bypass -File scripts\check.ps1`.
- `.claude/settings.json` разрешает сборку, тесты и линтеры без подтверждения и требует
  подтверждения для команд, меняющих систему (службы, hosts, `zapret-manager install/...`).
- Сессия обычно запущена без прав администратора: команды с UAC откроются в отдельном окне,
  их вывод сюда не попадёт. Проверяй такие сценарии тестами, а живой прогон проси у пользователя.
- Контекст по модулям: `docs/ai/ARCHITECTURE_MAP.md`, инварианты: `docs/ai/CODEBASE_GUIDE.md`.
