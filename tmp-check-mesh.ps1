[System.Net.ServicePointManager]::ServerCertificateValidationCallback = { $true }
try {
    $r = Invoke-WebRequest -Uri https://localhost:39999/api/config -UseBasicParsing
    $json = $r.Content | ConvertFrom-Json
    Write-Host "Mesh config: $($json.mesh | ConvertTo-Json -Compress)"
} catch {
    Write-Host "Error: $($_.Exception.Message)"
}
