@echo off
setlocal
set "ROOT=%~dp0"
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"

if not exist "%ROOT%\bin\godw-server.exe" (
  echo [x] bin\godw-server.exe not found. Build it first:
  echo     go build -trimpath -o bin/godw-server.exe ./cmd/godw-server
  pause
  exit /b 1
)

rem Metadata DB is the local PostgreSQL 17 (portable build, registered as Windows service godw-pg).
rem If it is already running, net start just returns without error.
net start godw-pg >nul 2>&1

cd /d "%ROOT%"
echo [*] GoDW console : http://localhost:8080
echo [*] Login        : admin@godw.local / admin12345
echo [*] Press Ctrl+C to stop
echo.
"%ROOT%\bin\godw-server.exe" --config "%ROOT%\config.win.yaml"
echo.
echo [*] server exited.
pause
