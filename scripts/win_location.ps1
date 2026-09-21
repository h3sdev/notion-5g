# win_location.ps1 — lee la posición del Windows Location API (GPS del portátil o Wi-Fi/IP).
# Requiere: Configuración > Privacidad > Ubicación activada para apps de escritorio.
# Salida: una línea JSON {"lat":..,"lon":..,"acc":..}
Add-Type -AssemblyName System.Device
$w = New-Object System.Device.Location.GeoCoordinateWatcher([System.Device.Location.GeoPositionAccuracy]::High)
$null = $w.TryStart($false, [TimeSpan]::FromSeconds(15))
$t0 = Get-Date
while (($w.Position.Location.IsUnknown) -and ((Get-Date) - $t0).TotalSeconds -lt 15) { Start-Sleep -Milliseconds 300 }
$l = $w.Position.Location
if ($l.IsUnknown) { Write-Output '{"lat":null,"lon":null,"acc":null}' }
else {
  $o = @{ lat = [math]::Round($l.Latitude, 6); lon = [math]::Round($l.Longitude, 6); acc = [math]::Round($l.HorizontalAccuracy, 1) }
  Write-Output ($o | ConvertTo-Json -Compress)
}
$w.Stop()
