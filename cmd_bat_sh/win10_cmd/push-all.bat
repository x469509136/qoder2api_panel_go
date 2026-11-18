@echo off
chcp 936 >nul
setlocal EnableExtensions

REM push-all.bat : 一键提交并推送 GitLab develop + GitHub qoder2api_panel_go/main
REM 用法：
REM   push-all.bat                      自动提交（无改动则只推送）
REM   push-all.bat "fix: 修复登录限速" 指定提交信息

set "SCRIPT_DIR=%~dp0"
set "PS=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
if not exist "%PS%" set "PS=powershell.exe"

if "%~1"=="" (
    "%PS%" -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT_DIR%push-all.ps1"
) else (
    "%PS%" -NoProfile -ExecutionPolicy Bypass -File "%SCRIPT_DIR%push-all.ps1" -Message "%~1"
)
exit /b %ERRORLEVEL%
