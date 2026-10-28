@echo off
cd /d "%~dp0"
echo Filling this folder: src, toolchain, windivert
echo.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0pack-selfbuild.ps1"
if errorlevel 1 (
  echo.
  echo pack failed. scroll up for the reason.
  pause
  exit /b 1
)
echo.
echo done. copy this whole folder, then double-click 一键编译.cmd
pause
