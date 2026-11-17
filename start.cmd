@echo off
rem CodeArts2API one-click launcher for Windows.
rem Creates config.json + api_key.txt, guides first login, starts the gateway
rem hidden, then opens the panel. Keep this file ASCII-only (cmd uses ANSI).
setlocal EnableExtensions
cd /d "%~dp0"
title CodeArts2API

if not exist "bin\codearts2api.exe" (
  echo [ERROR] bin\codearts2api.exe not found.
  echo Download the Windows release, or run "make windows" with Go installed.
  pause
  exit /b 1
)

if not exist "config.json" (
  echo [1/3] creating config.json with a random api_key ...
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\make-config.ps1"
  if errorlevel 1 (
    echo [ERROR] failed to create config.json
    pause
    exit /b 1
  )
)

dir /b "auths\*.json" >nul 2>&1
if errorlevel 1 (
  echo [2/3] no account found, starting Huawei Cloud login ...
  "bin\codearts2api-login.exe" -auth-dir ".\auths"
  if errorlevel 1 (
    echo [ERROR] login failed or cancelled.
    pause
    exit /b 1
  )
)

echo [3/3] starting gateway ...
netstat -an | findstr /R /C:":7866 .*LISTENING" >nul 2>&1
if not errorlevel 1 goto ready

start "" wscript.exe "%~dp0start-hidden.vbs"
set /a TRIES=0
:wait
timeout /t 1 /nobreak >nul
netstat -an | findstr /R /C:":7866 .*LISTENING" >nul 2>&1
if not errorlevel 1 goto ready
set /a TRIES+=1
if %TRIES% lss 20 goto wait
echo [ERROR] gateway did not start; see gw.log
pause
exit /b 1

:ready
echo Gateway is up: http://127.0.0.1:7866/v1
if exist "api_key.txt" echo API key is in api_key.txt (also the panel login password).
start "" http://127.0.0.1:7866/panel
timeout /t 3 /nobreak >nul
exit /b 0