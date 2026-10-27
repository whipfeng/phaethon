@echo off
echo [%date% %time%] Starting phaethon.exe > C:\temp\debug-startup.log
echo PHAETHON_WORKER=%PHAETHON_WORKER% >> C:\temp\debug-startup.log
echo Current directory: %CD% >> C:\temp\debug-startup.log
echo. >> C:\temp\debug-startup.log
C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.exe >> C:\temp\debug-startup.log 2>&1
