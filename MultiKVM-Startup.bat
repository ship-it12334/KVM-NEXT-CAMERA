@echo off
chcp 65001 >nul
setlocal
title MultiKVM - автостарт при вход в Windows

echo ==============================================
echo   MultiKVM - добавяне при стартиране на Windows
echo ==============================================
echo.

rem --- 1. Автостарт (HKCU\...\Run) ---
reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "MultiKVM" /t REG_SZ /d "\"%~dp0MultiKVM.exe\"" /f
if errorlevel 1 (
  echo ГРЕШКА при запис в регистъра!
  pause
  exit /b 1
)
echo   -> MultiKVM ще се стартира автоматично при вход в Windows.
echo.

rem --- 2. Защитна стена (само ако сме администратори) ---
net session >nul 2>&1
if not errorlevel 1 (
  echo   -> отварям TCP 9876 в защитната стена...
  netsh advfirewall firewall delete rule name="MultiKVM" >nul 2>&1
  netsh advfirewall firewall add rule name="MultiKVM" dir=in action=allow protocol=TCP localport=9876 profile=any enable=yes
) else (
  echo   -> портът НЕ е отворен (стартирай този файл като Администратор).
)

echo.
echo Готово!
pause
endlocal
