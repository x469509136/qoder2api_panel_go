<#
.SYNOPSIS
    bridge_qoder 一键提交并推送到 GitLab + GitHub。
.DESCRIPTION
    1. 检查工作区；如有修改先 git add -A 并提交（可指定 -Message）。
    2. 推送本地 develop 到 GitLab origin/develop。
    3. 从系统 Git 凭据管理器读取 github.com 凭据，推送 develop 到 GitHub main。
    4. token 只在进程内使用，不写 remote、不落盘、不打印。
    注意：绕过本机全局 github.com -> gh-proxy.com URL 重写；VPN 必须先连好。
.EXAMPLE
    .\push-all.ps1
    .\push-all.ps1 -Message "fix: 修复登录限速"
    .\push-all.ps1 -SkipCommit
#>
[CmdletBinding()]
param(
    [string] $Message = "",
    [switch] $SkipCommit,
    [string] $GitLabRemote = "origin",
    [string] $GitLabBranch = "develop",
    [string] $LocalBranch = "develop",
    [string] $GitHubBranch = "main",
    [string] $GitHubRepo = "https://github.com/x469509136/qoder2api_panel_go.git"
)
$ErrorActionPreference = "Stop"

function Write-Step([string]$Text) {
    Write-Host ""
    Write-Host "==> $Text" -ForegroundColor Cyan
}

function Get-GitRoot {
    $root = (& git rev-parse --show-toplevel 2>$null)
    if ($LASTEXITCODE -ne 0 -or -not $root) { throw "当前目录不是 Git 仓库" }
    return (($root -split "`r?`n")[0])
}

function Test-CleanWorktree {
    $status = & git status --porcelain=v1
    if ($LASTEXITCODE -ne 0) { throw "git status 失败" }
    if ($status) {
        Write-Host "工作区有未提交改动：" -ForegroundColor Yellow
        $status | Select-Object -First 30
        return $false
    }
    return $true
}

function Invoke-GitOrThrow {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$GitArgs)
    & git @GitArgs
    if ($LASTEXITCODE -ne 0) { throw ("git " + ($GitArgs -join " ") + " 失败，exit=" + $LASTEXITCODE) }
}

function Get-GitHubToken {
    $credText = "protocol=https`nhost=github.com`n`n" | & git credential fill 2>$null
    if ($LASTEXITCODE -ne 0) { throw "无法读取 github.com 系统凭据" }
    $line = $credText | Where-Object { $_ -like "password=*" } | Select-Object -First 1
    if (-not $line) { throw "系统凭据中没有 github.com token" }
    return ($line -replace "^password=", "")
}

try {
    $repoRoot = Get-GitRoot
    Set-Location -LiteralPath $repoRoot
    Write-Step ("仓库: " + $repoRoot)

    $current = (& git branch --show-current).Trim()
    if ($current -ne $LocalBranch) { throw ("当前分支是 $current，要求 $LocalBranch") }

    $clean = Test-CleanWorktree
    if (-not $SkipCommit) {
        if (-not $clean) {
            if (-not $Message) { $Message = "chore: auto commit " + (Get-Date -Format "yyyy-MM-dd HH:mm:ss") }
            Write-Step ("提交: " + $Message)
            Invoke-GitOrThrow add -A
            Invoke-GitOrThrow commit -m $Message
        } else {
            Write-Step "工作区干净，跳过提交"
        }
    } elseif (-not $clean) {
        throw "指定 -SkipCommit 但工作区不干净"
    }

    $head = (& git rev-parse HEAD).Trim()
    Write-Step ("本地 HEAD: " + $head)

    Write-Step ("推送 GitLab: " + $GitLabRemote + "/" + $GitLabBranch)
    Invoke-GitOrThrow push $GitLabRemote ("HEAD:refs/heads/" + $GitLabBranch)

    Write-Step ("推送 GitHub: " + $GitHubRepo + " (" + $GitHubBranch + ")")
    $token = Get-GitHubToken
    $authRepoUrl = $GitHubRepo.Replace("https://", "https://x469509136:" + $token + "@")
    $oldGlobal = $env:GIT_CONFIG_GLOBAL
    $oldSystem = $env:GIT_CONFIG_SYSTEM
    $oldPrompt = $env:GIT_TERMINAL_PROMPT
    try {
        $env:GIT_CONFIG_GLOBAL = "NUL"
        $env:GIT_CONFIG_SYSTEM = "NUL"
        $env:GIT_TERMINAL_PROMPT = "0"
        & git -c credential.helper= -c credential.interactive=never push $authRepoUrl ("HEAD:refs/heads/" + $GitHubBranch)
        if ($LASTEXITCODE -ne 0) { throw ("GitHub 推送失败，exit=" + $LASTEXITCODE) }
    } finally {
        $env:GIT_CONFIG_GLOBAL = $oldGlobal
        $env:GIT_CONFIG_SYSTEM = $oldSystem
        $env:GIT_TERMINAL_PROMPT = $oldPrompt
        Remove-Variable token -ErrorAction SilentlyContinue
    }

    Write-Step "完成"
    Write-Host ("GitLab : " + $GitLabRemote + "/" + $GitLabBranch)
    Write-Host ("GitHub : " + $GitHubRepo + " (" + $GitHubBranch + ")")
    Write-Host ("HEAD   : " + $head)
} catch {
    Write-Host ""
    Write-Host ("[ERROR] " + $_.Exception.Message) -ForegroundColor Red
    exit 1
}
