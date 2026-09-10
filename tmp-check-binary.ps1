$f = 'C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.exe'
$hash = Get-FileHash $f -Algorithm SHA256
Write-Host "File: $f"
Write-Host "Size: $((Get-Item $f).Length)"
Write-Host "SHA256: $($hash.Hash)"
Write-Host "LastWrite: $((Get-Item $f).LastWriteTime)"
