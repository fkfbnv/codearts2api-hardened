# make-config.ps1 - create config.json with a random api_key on first run.
# Called by start.cmd. Keeps config.example.json untouched and prints the key.
# ASCII-only: Windows PowerShell 5.1 reads BOM-less files with the ANSI codepage.
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

if (Test-Path "config.json") {
    Write-Host "config.json already exists, skipping."
    exit 0
}
if (-not (Test-Path "config.example.json")) {
    Write-Host "config.example.json not found."
    exit 1
}

$key = -join (1..48 | ForEach-Object { "{0:x}" -f (Get-Random -Maximum 16) })
$utf8 = New-Object System.Text.UTF8Encoding($false)
# Read as UTF-8 explicitly: Windows PowerShell 5.1 `Get-Content -Raw` uses the
# ANSI codepage and mangles the Chinese comments, producing invalid JSON.
$text = [System.IO.File]::ReadAllText((Join-Path $root "config.example.json"), $utf8)
$text = $text.Replace('"api_key": ""', '"api_key": "' + $key + '"')
# Local-only default: the panel page itself is unauthenticated, so bind loopback.
# Docker / reverse-proxy setups should change it back to ":7866".
$text = $text.Replace('"listen": ":7866"', '"listen": "127.0.0.1:7866"')

[System.IO.File]::WriteAllText((Join-Path $root "config.json"), $text, $utf8)
[System.IO.File]::WriteAllText((Join-Path $root "api_key.txt"), $key, [System.Text.Encoding]::ASCII)

Write-Host "config.json created (listen 127.0.0.1:7866)."
Write-Host "api_key: $key"