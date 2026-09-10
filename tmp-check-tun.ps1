try {
    $r = Invoke-WebRequest -Uri http://localhost:39999/api/tun -UseBasicParsing
    $json = $r.Content | ConvertFrom-Json
    Write-Host "TUN running: $($json.running)"
    Write-Host "TUN enabled: $($json.enabled)"
    Write-Host "TUN available: $($json.available)"
    Write-Host "Device: $($json.deviceName)"
} catch {
    Write-Host "Error: $($_.Exception.Message)"
}
