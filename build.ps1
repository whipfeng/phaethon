# Build script for phaethon - all platforms
# Usage: .\build.ps1 [target]
#   target: linux, windows, windows7, darwin-amd64, darwin-arm64, all (default)

param([string]$Target = "all")

$Binary = "phaethon"
$Src = "."
$BuildDir = "build"

function Build-Linux {
    Write-Host "Building Linux amd64..."
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    go build -o "$BuildDir/linux/$Binary" $Src
    Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
}

function Build-Windows {
    Write-Host "Building Windows amd64..."
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    go build -o "$BuildDir/windows/$Binary.exe" $Src
    Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
}

function Build-Windows7 {
    $go = $env:GO_LEGACY_WIN7
    if (-not $go) {
        Write-Error "GO_LEGACY_WIN7 is not set; point it to the go-legacy-win7 compiler, e.g. C:\go-legacy-win7\bin\go.exe"
        exit 1
    }
    Write-Host "Building Windows 7 amd64 (legacy compiler)..."
    & $go build -o "$BuildDir/windows7/$Binary.exe" $Src
}

function Build-DarwinAmd64 {
    Write-Host "Building macOS amd64..."
    $env:GOOS = "darwin"
    $env:GOARCH = "amd64"
    go build -o "$BuildDir/darwin/$Binary-amd64" $Src
    Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
}

function Build-DarwinArm64 {
    Write-Host "Building macOS arm64..."
    $env:GOOS = "darwin"
    $env:GOARCH = "arm64"
    go build -o "$BuildDir/darwin/$Binary-arm64" $Src
    Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
}

switch ($Target.ToLower()) {
    "linux"        { Build-Linux }
    "windows"      { Build-Windows }
    "windows7"     { Build-Windows7 }
    "darwin-amd64" { Build-DarwinAmd64 }
    "darwin-arm64" { Build-DarwinArm64 }
    "all" {
        Build-Linux
        Build-Windows
        Build-Windows7
        Build-DarwinAmd64
        Build-DarwinArm64
        Write-Host "All builds completed!" -ForegroundColor Green
    }
    default {
        Write-Host "Unknown target: $Target" -ForegroundColor Red
        Write-Host "Usage: .\build.ps1 [linux|windows|windows7|darwin-amd64|darwin-arm64|all]"
    }
}
