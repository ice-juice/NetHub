# 在仓库里双击「打包自编译.cmd」：把编译要用的源码、依赖、Go 收进本目录（third_party）。
# 本目录就是整包。拷走整个 third_party 后，双击「一键编译.cmd」。
# 编译脚本只读本目录下的相对路径：src\、toolchain\、windivert\、out\。
#
# WinDivert：优先用 windivert\ 里已放好的官方 dll/sys。
# 没有就下载 WinDivert 2.2.2 官方 zip。Defender 经常把这个 zip 隔离，
# 那时把已经在跑的那两份文件拷进 windivert\ 再打包。
$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Kit = $PSScriptRoot
$Repo = Split-Path -Parent $Kit
$Src = Join-Path $Kit "src"
$Toolchain = Join-Path $Kit "toolchain"

if (-not (Test-Path (Join-Path $Repo "go.mod"))) {
	throw "上一级目录没有 go.mod。请在仓库的 third_party 里双击「打包自编译.cmd」，不要把脚本单独拷走再打包。"
}

# WinDivert 2.2.2 官方包（divert-go 接受 2.0/2.1/2.2）。
$WinDivertZipName = "WinDivert-2.2.2-A.zip"
$WinDivertUrl = "https://github.com/basil00/WinDivert/releases/download/v2.2.2/$WinDivertZipName"
$WinDivertZipSha = "63cb41763bb4b20f600b6de04e991a9c2be73279e317d4d82f237b150c5f3f15"
$WinDivertSysSha = "8da085332782708d8767bcace5327a6ec7283c17cfb85e40b03cd2323a90ddc2"
$WinDivertThumb = "043589F75FCE2795E7F2CC3E526D46784D5DDAB3"

function Say([string]$msg) { Write-Host $msg }

function Get-GoVersion {
	$line = & go version
	if ($line -match "go(\d+\.\d+(?:\.\d+)?)") { return $Matches[1] }
	throw "认不出本机 Go 版本：$line"
}

function Save-Url([string]$url, [string]$dest) {
	Say "下载 $url"
	Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing
}

function Get-Sha256([string]$path) {
	return (Get-FileHash -Algorithm SHA256 -Path $path).Hash.ToLowerInvariant()
}

function Assert-OfficialSys([string]$path) {
	$hash = Get-Sha256 $path
	if ($hash -ne $WinDivertSysSha) {
		throw "WinDivert64.sys 不是官方 2.2.2（摘要 $hash）。请换官方 x64 文件。"
	}
	$sig = Get-AuthenticodeSignature -FilePath $path
	$thumb = ""
	if ($sig.SignerCertificate) { $thumb = $sig.SignerCertificate.Thumbprint }
	if ($sig.Status -ne "Valid" -or $thumb -ne $WinDivertThumb) {
		throw "WinDivert64.sys 签名无效（status=$($sig.Status) thumb=$thumb）。不要用来路不明的驱动。"
	}
}

function Copy-Source {
	if (Test-Path $Src) { Remove-Item -Recurse -Force $Src }
	New-Item -ItemType Directory -Force -Path $Src | Out-Null
	# 源码放进 third_party\src。不把 third_party 自己再套一层，也不带凭据和运行目录。
	$prev = $ErrorActionPreference
	$ErrorActionPreference = "Continue"
	& robocopy $Repo $Src /E /NFL /NDL /NJH /NJS /nc /ns /np `
		/XD .git third_party run local dist build update logs pcap backups vendor out `
		/XF config.yaml secrets.dat WinDivert.dll WinDivert64.sys *.exe | Out-Null
	$code = $LASTEXITCODE
	$ErrorActionPreference = $prev
	if ($code -ge 8) { throw "复制源码失败（robocopy $code）" }
	if (-not (Test-Path (Join-Path $Src "go.mod"))) { throw "src\go.mod 没有复制出来" }
	if (Test-Path (Join-Path $Src "config.yaml")) { throw "src 里不该有 config.yaml" }
	if (Test-Path (Join-Path $Src "secrets.dat")) { throw "src 里不该有 secrets.dat" }
}

function Add-Vendor {
	Say "整理离线依赖到 src\vendor …"
	Push-Location $Src
	try {
		$env:GOFLAGS = ""
		$env:GOTOOLCHAIN = "local"
		& go mod vendor
		if ($LASTEXITCODE -ne 0) { throw "go mod vendor 失败" }
	}
	finally { Pop-Location }
	if (-not (Test-Path (Join-Path $Src "vendor\modules.txt"))) {
		throw "src\vendor 没生成。本机需要能访问 Go 模块代理。"
	}
}

function Add-GoToolchain([string]$version) {
	New-Item -ItemType Directory -Force -Path $Toolchain | Out-Null
	$name = "go$version.windows-amd64.zip"
	$cached = Join-Path $Toolchain $name
	Say "核对 Go $version 的官方摘要…"
	$rels = Invoke-RestMethod -Uri "https://go.dev/dl/?mode=json&include=all"
	$rel = $rels | Where-Object { $_.version -eq "go$version" } | Select-Object -First 1
	if (-not $rel) { throw "go.dev 上没有 go$version" }
	$file = $rel.files | Where-Object { $_.filename -eq $name } | Select-Object -First 1
	if (-not $file) { throw "没有 $name" }
	$want = $file.sha256.ToLowerInvariant()
	$old = Join-Path $Repo "build\cache\$name"
	if (-not (Test-Path $cached) -and (Test-Path $old)) {
		Copy-Item $old $cached
	}
	if (-not (Test-Path $cached) -or (Get-Sha256 $cached) -ne $want) {
		Save-Url "https://go.dev/dl/$name" $cached
	}
	$got = Get-Sha256 $cached
	if ($got -ne $want) { throw "Go 压缩包摘要不符（期望 $want，实际 $got）" }
}

function Add-WinDivert {
	$dest = Join-Path $Kit "windivert"
	New-Item -ItemType Directory -Force -Path $dest | Out-Null
	$localDll = Join-Path $dest "WinDivert.dll"
	$localSys = Join-Path $dest "WinDivert64.sys"
	if ((Test-Path $localDll) -and (Test-Path $localSys)) {
		Say "使用 windivert\ 里已有的官方驱动文件。"
		Assert-OfficialSys $localSys
		return
	}

	$zip = Join-Path $Toolchain $WinDivertZipName
	try {
		if (-not (Test-Path $zip)) { Save-Url $WinDivertUrl $zip }
		$got = Get-Sha256 $zip
		if ($got -ne $WinDivertZipSha) { throw "WinDivert 压缩包摘要不符（期望 $WinDivertZipSha，实际 $got）" }
	}
	catch {
		Remove-Item -LiteralPath $zip -Force -ErrorAction SilentlyContinue
		throw @"
拿不到官方 WinDivert（$($_.Exception.Message)）。
源码和 Go 已经放在本目录。Defender 常把官方 zip 隔离，本脚本不会去关防护。
请把 WinDivert.dll 和 WinDivert64.sys 放到：
  $dest
（WinDivert64.sys 必须是官方 2.2.2 签名）然后重新双击「打包自编译.cmd」。
"@
	}

	Add-Type -AssemblyName System.IO.Compression.FileSystem
	$archive = [System.IO.Compression.ZipFile]::OpenRead($zip)
	try {
		foreach ($name in @("WinDivert.dll", "WinDivert64.sys")) {
			$entry = $archive.Entries | Where-Object { $_.FullName -replace "\\", "/" -eq "WinDivert-2.2.2-A/x64/$name" } | Select-Object -First 1
			if (-not $entry) { throw "压缩包里没有 x64\$name" }
			$out = Join-Path $dest $name
			$stream = $entry.Open()
			try {
				$fs = [System.IO.File]::Create($out)
				try { $stream.CopyTo($fs) } finally { $fs.Dispose() }
			}
			finally { $stream.Dispose() }
		}
	}
	finally { $archive.Dispose() }
	Assert-OfficialSys $localSys
}

Say "收集源码到 src\ …"
Copy-Source
Add-Vendor

$goVer = Get-GoVersion
Say "把 Go $goVer 放到 toolchain\ …"
Add-GoToolchain $goVer

$info = @"
把整个 third_party 文件夹拷到目标电脑（不要只拷脚本）。
然后双击本目录的「一键编译.cmd」。

编译只读相对路径：
  src\          源码和 vendor
  toolchain\    Go，不用安装、编译时不用上网
  windivert\    WinDivert.dll 与 WinDivert64.sys
  out\          编出来的 nethub.exe

打包时间：$(Get-Date -Format "yyyy-MM-dd HH:mm:ss")
Go：$goVer
"@
[System.IO.File]::WriteAllText((Join-Path $Kit "怎么编译.txt"), $info, [System.Text.UTF8Encoding]::new($true))

Say "检查 windivert\ …"
Add-WinDivert

Say ""
Say "已经收进本目录："
Say "  src\        源码"
Say "  toolchain\  Go $goVer"
Say "  windivert\  驱动"
Say "把整个 third_party 文件夹拷走，在那边双击「一键编译.cmd」。"
