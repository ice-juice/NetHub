@echo off
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1" %*
if errorlevel 1 (
  echo.
  echo build failed. scroll up for the reason.
  pause
  exit /b 1
)
echo.
echo done. program is in the out folder: out\nethub.exe
pause
