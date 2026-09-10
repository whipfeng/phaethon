$file = 'C:\Users\Docker\Desktop\Workspace\phaethon\config.yaml'
$content = Get-Content $file -Raw
$old = "    - name: SOCKS5_7890`n      type: socks5`n      server: 10.11.61.40`n      port: 7890"
$new = "    - name: SOCKS5_7890`n      type: socks5`n      server: 10.11.61.40`n      port: 7890`n      p2p: true"
$content = $content.Replace($old, $new)
Set-Content $file -Value $content -NoNewline
Write-Host "Done"
