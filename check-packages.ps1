Get-ChildItem 'C:\Users\Docker\Desktop\Workspace\phaethon\data\packages\*.json' | ForEach-Object {
    $json = Get-Content $_.FullName | ConvertFrom-Json
    Write-Host "$($_.Name): $($json.meta.version) ($($json.meta.platform)/$($json.meta.arch))"
}
