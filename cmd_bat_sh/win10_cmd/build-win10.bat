@echo off
chcp 936 >nul
setlocal EnableExtensions EnableDelayedExpansion

REM ============================================================
REM build-win10.bat : bridge_qoder Win10 编译脚本
REM
REM 本工程是单进程 Go 网关（无前端、无 WebView），"单文件 exe"
REM 指 code\qoder-proxy.exe 本体；运行时还需同级目录的
REM   web\      单文件看板
REM   i18n\     语言包
REM   assets\   baseprompt + 双区模型快照
REM 这三个目录不含可执行代码，属于随包分发的静态资源。
REM
REM 用法:
REM   build-win10.bat            构建 release exe + 组装运行目录
REM   build-win10.bat debug      构建带符号的调试版
REM   build-win10.bat clean      清理产物目录（会先停服）
REM
REM 可选环境变量:
REM   OUT_DIR     产物目录      默认 <repo>\dist-win
REM   GO_BIN      go.exe 所在目录 默认从 PATH 查找
REM
REM 退出码: 0 成功 / 1 环境缺失 / 2 参数错误 / 3 编译失败 / 5 组装失败
REM ============================================================

set "SCRIPT_DIR=%~dp0"
for %%I in ("%SCRIPT_DIR%..\..") do set "REPO_ROOT=%%~fI"
set "CODE_DIR=%REPO_ROOT%\code"
if not defined OUT_DIR set "OUT_DIR=%REPO_ROOT%\dist-win"

set "MODE=%~1"
if "%MODE%"=="" set "MODE=all"

echo.
echo ============================================================
echo  bridge_qoder - Win10 构建
echo    repo   : %REPO_ROOT%
echo    mode   : %MODE%
echo    out    : %OUT_DIR%
echo ============================================================
echo.

if /i "%MODE%"=="clean" goto :do_clean
if /i "%MODE%"=="all" goto :check_env
if /i "%MODE%"=="debug" goto :check_env
echo [FAIL] 未知参数: %MODE%
echo        可用: all ^| debug ^| clean
exit /b 2

REM ------------------------------------------------------------
REM 1. 环境检查
REM ------------------------------------------------------------
:check_env
if not exist "%CODE_DIR%\go.mod" (
  echo [FAIL] 未找到 %CODE_DIR%\go.mod
  echo        请确认脚本位于 cmd_bat_sh\win10_cmd\ 下
  exit /b 1
)
if not exist "%CODE_DIR%\cmd\qoder-proxy\main.go" (
  echo [FAIL] 未找到 %CODE_DIR%\cmd\qoder-proxy\main.go
  exit /b 1
)

set "GO_EXE="
if defined GO_BIN if exist "%GO_BIN%\go.exe" set "GO_EXE=%GO_BIN%\go.exe"
if not defined GO_EXE if exist "C:\Program Files\Go\bin\go.exe" set "GO_EXE=C:\Program Files\Go\bin\go.exe"
if not defined GO_EXE for /f "delims=" %%G in ('where go.exe 2^>nul') do if not defined GO_EXE set "GO_EXE=%%G"
if not defined GO_EXE (
  echo [FAIL] 未找到 go.exe，请安装 Go 1.21+ 并加入 PATH
  exit /b 1
)
for %%D in ("%GO_EXE%") do set "PATH=%%~dpD;%PATH%"
echo [INFO] go      : %GO_EXE%
for /f "delims=" %%V in ('"%GO_EXE%" version 2^>nul') do echo [INFO] version : %%V

REM --- 关键: 关闭 CGO，产出真正单文件、无 VC++ 运行库依赖的 exe ---
REM 本工程零第三方依赖，纯标准库；CGO 关掉后 net 走 Go 自带解析器，
REM 目标机不必再装 VC++ 运行库。
set "CGO_ENABLED=0"
set "GOOS=windows"
set "GOARCH=amd64"
echo [INFO] target  : %GOOS%/%GOARCH% ^(CGO_ENABLED=0^)
echo.

echo [INFO] go mod download
pushd "%CODE_DIR%"
"%GO_EXE%" mod download 2>&1
set "RC=%ERRORLEVEL%"
popd
REM go 会把控制台代码页改成 UTF-8，之后中文 echo 必须复位
chcp 936 >nul
if not "%RC%"=="0" (
  echo [FAIL] go mod download 失败 exit=%RC%
  exit /b 3
)

echo.
echo [INFO] go vet ./...
pushd "%CODE_DIR%"
"%GO_EXE%" vet ./... 2>&1
set "RC=%ERRORLEVEL%"
popd
chcp 936 >nul
if not "%RC%"=="0" (
  echo [FAIL] go vet 失败 exit=%RC%
  exit /b 3
)
echo.

REM ------------------------------------------------------------
REM 2. 编译
REM ------------------------------------------------------------
REM 版本号取 git 短哈希，便于线上排查到底是哪次构建。
set "GIT_DESC=unknown"
pushd "%REPO_ROOT%"
for /f "delims=" %%H in ('git rev-parse --short HEAD 2^>nul') do set "GIT_DESC=%%H"
popd
if "%GIT_DESC%"=="unknown" echo [WARN] 读不到 git 哈希，版本号记为 unknown

set "LDFLAGS=-s -w -X github.com/xzs/bridge_qoder/internal/server.Version=2.0.0-go+%GIT_DESC%"
if /i "%MODE%"=="debug" (
  set "LDFLAGS=-X github.com/xzs/bridge_qoder/internal/server.Version=2.0.0-go+%GIT_DESC%-debug"
)

if not exist "%OUT_DIR%" mkdir "%OUT_DIR%" >nul 2>&1
if not exist "%OUT_DIR%" (
  echo [FAIL] 无法创建产物目录 %OUT_DIR%
  exit /b 5
)

echo [INFO] go build -trimpath
pushd "%CODE_DIR%"
"%GO_EXE%" build -trimpath -buildvcs=false -ldflags "%LDFLAGS%" -o "%OUT_DIR%\qoder-proxy.exe" ./cmd/qoder-proxy 2>&1
set "RC=%ERRORLEVEL%"
popd
chcp 936 >nul

if not "%RC%"=="0" (
  echo.
  echo [FAIL] go build 失败 exit=%RC%
  echo        常见原因: 语法错误 / 依赖缺失 / 目标 exe 正被运行
  echo        若提示拒绝访问，请先执行 stop-win10.bat
  exit /b 3
)
if not exist "%OUT_DIR%\qoder-proxy.exe" (
  echo [FAIL] 编译结束但未找到 %OUT_DIR%\qoder-proxy.exe
  exit /b 3
)
echo [ OK ] 编译完成
echo.

REM ------------------------------------------------------------
REM 3. 组装运行目录（exe 同级需要 web / i18n / assets）
REM ------------------------------------------------------------
echo [INFO] 组装运行时资源
for %%R in (web i18n assets) do (
  if not exist "%CODE_DIR%\%%R" (
    echo [FAIL] 缺少源目录 %CODE_DIR%\%%R
    exit /b 5
  )
  if exist "%OUT_DIR%\%%R" rmdir /s /q "%OUT_DIR%\%%R"
  xcopy "%CODE_DIR%\%%R" "%OUT_DIR%\%%R\" /e /i /q /y >nul
  if errorlevel 1 (
    echo [FAIL] 复制 %%R 失败
    exit /b 5
  )
)

REM 首次运行会生成 accounts\ usage\ logs\；这里只建空目录，
REM 不含任何凭据，方便直接双击运行。
for %%R in (accounts usage logs) do (
  if not exist "%OUT_DIR%\%%R" mkdir "%OUT_DIR%\%%R" >nul 2>&1
)

echo [ OK ] 资源就绪
echo.

REM --- SHA256 校验和 ---
del /q "%OUT_DIR%\SHA256SUMS.txt" >nul 2>&1
for /f "usebackq tokens=1" %%H in ('certutil -hashfile "%OUT_DIR%\qoder-proxy.exe" SHA256 ^| findstr /v ":"') do echo %%H  qoder-proxy.exe>>"%OUT_DIR%\SHA256SUMS.txt"

echo ------------------------------------------------------------
echo  产物目录: %OUT_DIR%
for %%F in ("%OUT_DIR%\qoder-proxy.exe") do echo    %%~nxF  ^(%%~zF bytes^)
echo    web\ i18n\ assets\ accounts\ usage\ logs\
echo    SHA256SUMS.txt
echo ------------------------------------------------------------
echo.
echo 说明: exe 为静态编译，目标机无需 Go 与 VC++ 运行库。
echo       直接运行本目录下 start-win10.bat 即可启动。
echo.
exit /b 0

REM ------------------------------------------------------------
REM clean
REM ------------------------------------------------------------
:do_clean
echo [INFO] 先停止正在运行的实例
call "%SCRIPT_DIR%stop-win10.bat" quiet
echo [INFO] 清理产物目录 %OUT_DIR%
if exist "%OUT_DIR%" rmdir /s /q "%OUT_DIR%"
if exist "%OUT_DIR%" (
  echo [FAIL] 删除失败，可能仍有进程占用文件
  exit /b 1
)
echo [ OK ] 已清理
exit /b 0

