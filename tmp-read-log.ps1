$f = 'C:\Users\Docker\Desktop\Workspace\phaethon\mesh-debug.log'
if (Test-Path $f) {
    $content = [IO.File]::ReadAllText($f)
    Write-Host "=== mesh-debug.log ($($content.Length) chars) ==="
    Write-Host $content
} else {
    Write-Host "File not found"
}
