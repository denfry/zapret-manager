@echo off
cd /d "%~dp0"
go test ./... || exit /b 1
go build -trimpath -ldflags "-s -w" -o dist\zapret-manager.exe . || exit /b 1
echo dist\zapret-manager.exe
