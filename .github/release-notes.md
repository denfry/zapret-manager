## Как поставить

1. Скачайте **zapret-manager.exe** из списка файлов ниже (Assets).
2. Запустите его двойным щелчком и разрешите запрос прав администратора.
3. В меню выберите **«Установить»** — программа скачает zapret, подберёт рабочую стратегию
   и включит автопочинку. Дальше открывайте `C:\zapret\zapret-manager.exe`.

Если zapret у вас уже стоит (через `service.bat`), программа подхватит его стратегию и настройки.

**Windows SmartScreen** может написать «Windows защитил ваш компьютер»: exe не подписан
сертификатом. Нажмите «Подробнее» → «Выполнить в любом случае».

### Проверка подлинности

Exe собран GitHub Actions из исходников этого репозитория — это можно проверить:

```powershell
gh attestation verify zapret-manager.exe -R {{REPO}}
```

или сверить SHA-256 с файлом `zapret-manager.exe.sha256`:

```powershell
Get-FileHash zapret-manager.exe -Algorithm SHA256
```
