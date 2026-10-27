Get-Process | Where-Object {$_.ProcessName -like '*phaethon*'} | Select-Object Id, ProcessName, StartTime, CPU
