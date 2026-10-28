# 离线编译。只读本脚本旁边的相对路径，不访问上一级目录，也不上网。
#   src\         源码 + vendor
#   toolchain\   Go 压缩包
#   windivert\   WinDivert.dll / WinDivert64.sys
#   out\         编出来的程序
$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$Src = Join-Path $Root "src"
$OutDir = Join-Path $Root "out"
$GoZip = Get-ChildItem -Path (Join-Path $Root "toolchain") -Filter "go*.windows-amd64.zip" -ErrorAction SilentlyContinue |
	Select-Object -First 1
$GoRoot = Join-Path $Root "toolchain\go"
$GoExe = Join-Path $GoRoot "bin\go.exe"
$DriverDir = Join-Path $Root "windivert"

function Say([string]$msg) { Write-Host $msg }

if (-not [Environment]::Is64BitOperatingSystem) {
	throw "只支持 64 位 Windows。"
}
if (-not (Test-Path (Join-Path $Src "go.mod"))) {
	throw "缺少 src\go.mod。请先在有源码的电脑上双击「打包自编译.cmd」，再把整个文件夹拷过来。"
}
if (-not (Test-Path (Join-Path $Src "vendor\modules.txt"))) {
	throw "缺少 src\vendor。请重新双击「打包自编译.cmd」。"
}

$dll = Join-Path $DriverDir "WinDivert.dll"
$sys = Join-Path $DriverDir "WinDivert64.sys"
if (-not (Test-Path $dll) -or -not (Test-Path $sys)) {
	throw "缺少 windivert\WinDivert.dll 或 WinDivert64.sys。"
}

if (-not (Test-Path $GoExe)) {
	if (-not $GoZip) {
		throw "缺少 toolchain\go*.windows-amd64.zip。请重新打包。"
	}
	Say "正在解压 Go 工具链（只需第一次）…"
	Add-Type -AssemblyName System.IO.Compression.FileSystem
	[System.IO.Compression.ZipFile]::ExtractToDirectory($GoZip.FullName, (Join-Path $Root "toolchain"))
}
if (-not (Test-Path $GoExe)) {
	throw "解压后仍找不到 toolchain\go\bin\go.exe。"
}

$env:GOROOT = $GoRoot
$env:GOPATH = Join-Path $Root "toolchain\gopath"
$env:GOCACHE = Join-Path $Root "toolchain\gocache"
$env:GOPROXY = "off"
$env:GOSUMDB = "off"
$env:GOTOOLCHAIN = "local"
$env:CGO_ENABLED = "0"
$env:GOFLAGS = "-mod=vendor"
$env:PATH = (Join-Path $GoRoot "bin") + [IO.Path]::PathSeparator + $env:PATH
New-Item -ItemType Directory -Force -Path $env:GOPATH, $env:GOCACHE, $OutDir | Out-Null

Say "正在编译 src → out\nethub.exe …"
Push-Location $Src
try {
	& $GoExe build -mod=vendor -tags production -ldflags "-H=windowsgui -s -w" -o (Join-Path $OutDir "nethub.exe") .
	if ($LASTEXITCODE -ne 0) { throw "go build 失败（退出码 $LASTEXITCODE）" }
}
finally {
	Pop-Location
}

Copy-Item $dll (Join-Path $OutDir "WinDivert.dll") -Force
Copy-Item $sys (Join-Path $OutDir "WinDivert64.sys") -Force
$lic = Join-Path $Root "WinDivert-LICENSE.txt"
if (Test-Path $lic) { Copy-Item $lic (Join-Path $OutDir "WinDivert-LICENSE.txt") -Force }
$example = Join-Path $Src "config.yaml.example"
if (Test-Path $example) { Copy-Item $example (Join-Path $OutDir "config.yaml.example") -Force }

Say ""
Say "编译完成：out\nethub.exe"
Say "安全软件要放行 out\WinDivert64.sys，否则会报 1450。"
Say "双击 out\nethub.exe（会要一次管理员权限）。配置在界面里填，或把 config.yaml.example 复制成 config.yaml。"
