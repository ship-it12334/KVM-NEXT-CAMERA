@echo off
cd /d "%~dp0"
set PATH=C:\Go\bin;C:\Program Files\Go\bin;%PATH%
echo Building MultiKVM.exe...
go build -ldflags "-H windowsgui -s -w" -o MultiKVM.exe .
if errorlevel 1 (
  echo BUILD FAILED!
  pause
  exit /b 1
)
echo OK: MultiKVM.exe
pause
