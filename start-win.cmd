@echo off
setlocal
set "ROOT=%~dp0"
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"

set "EXE=%ROOT%\bin\godw-server.exe"
rem Console listen port, keep in sync with server.addr in config.win.yaml.
set "PORT=8080"

if not exist "%EXE%" (
  echo [x] bin\godw-server.exe not found. Build it first:
  echo     go build -trimpath -o bin/godw-server.exe ./cmd/godw-server
  pause
  exit /b 1
)

rem An old instance still holding the port would make the new one fail to bind,
rem so release the port first: kill the listener, then wait until it is gone.
call :free_port %PORT%
if errorlevel 1 (
  pause
  exit /b 1
)

rem Metadata DB is the local PostgreSQL 17 (portable build, registered as Windows service godw-pg).
rem If it is already running, net start just returns without error.
net start godw-pg >nul 2>&1

cd /d "%ROOT%"
echo [*] GoDW console : http://localhost:%PORT%
echo [*] Login        : admin@godw.local / admin12345
echo [*] Press Ctrl+C to stop
echo.
"%EXE%" --config "%ROOT%\config.win.yaml"
echo.
echo [*] server exited.
pause
exit /b 0

rem ---------------------------------------------------------------
rem free_port <port> - kill whatever is listening on <port>, then poll
rem until the socket is actually released (max ~5s).
rem One process can appear twice (0.0.0.0:PORT and [::]:PORT), so PIDs are
rem de-duplicated before taskkill. taskkill's exit code is ignored on
rem purpose: killing the same PID twice reports 128 (not found), and the
rem only thing that matters is whether the port ends up free.
rem ---------------------------------------------------------------
:free_port
setlocal enabledelayedexpansion
set "FP_PORT=%~1"
set "FP_SEEN=;"
set "FP_ANY="
for /f "tokens=5" %%P in ('netstat -ano ^| findstr /i "LISTENING" ^| findstr /c:":%FP_PORT% "') do (
  if not "%%P"=="0" (
    if "!FP_SEEN:;%%P;=!"=="!FP_SEEN!" (
      set "FP_SEEN=!FP_SEEN!%%P;"
      set "FP_ANY=1"
      echo [*] Port %FP_PORT% is held by PID %%P - stopping it...
      taskkill /f /t /pid %%P >nul 2>&1
    )
  )
)
if not defined FP_ANY (
  endlocal
  exit /b 0
)

echo [*] Waiting for port %FP_PORT% to be released...
set "FP_OK="
for /l %%I in (1,1,10) do (
  if not defined FP_OK (
    netstat -ano | findstr /i "LISTENING" | findstr /c:":%FP_PORT% " >nul 2>&1
    if errorlevel 1 (
      set "FP_OK=1"
    ) else (
      ping -n 2 127.0.0.1 >nul
    )
  )
)
if defined FP_OK (
  echo [*] Port %FP_PORT% released.
  endlocal
  exit /b 0
)

echo [x] Port %FP_PORT% is still held after 5s:
netstat -ano | findstr /i "LISTENING" | findstr /c:":%FP_PORT% "
echo [x] It could not be killed - run this script as Administrator, or stop
echo [x] the process above manually, then retry.
endlocal
exit /b 1
