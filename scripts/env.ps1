# zMail 本地开发环境变量（Windows / PowerShell）
#
# 用途：
#   1) 把 Go 加入 PATH（Go 未安装到系统 PATH 时可用）；
#   2) 需要联网拉取模块时，可从 ZMAIL_PROXY 或已有的 HTTPS_PROXY 继承代理配置。
#
# 用法：. .\scripts\env.ps1
# 覆盖：设置 ZMAIL_GO_BIN / ZMAIL_PROXY 环境变量即可。

# --- Go ---
if (-not $env:ZMAIL_GO_BIN) {
  $candidates = @(
    (Join-Path $env:LOCALAPPDATA 'Programs\go\bin'),
    'C:\Program Files\Go\bin',
    'C:\Go\bin'
  )
  foreach ($dir in $candidates) {
    if (Test-Path (Join-Path $dir 'go.exe')) { $env:ZMAIL_GO_BIN = $dir; break }
  }
}
if ($env:ZMAIL_GO_BIN -and (Test-Path (Join-Path $env:ZMAIL_GO_BIN 'go.exe'))) {
  if (($env:Path -split ';') -notcontains $env:ZMAIL_GO_BIN) {
    $env:Path = $env:ZMAIL_GO_BIN + ';' + $env:Path
  }
}

# --- 代理（仅在显式设置 ZMAIL_PROXY 时生效） ---
if ($env:ZMAIL_PROXY -and -not $env:HTTPS_PROXY) {
  $env:HTTP_PROXY = $env:ZMAIL_PROXY
  $env:HTTPS_PROXY = $env:ZMAIL_PROXY
  if (-not $env:NO_PROXY) { $env:NO_PROXY = '127.0.0.1,localhost' }
}
