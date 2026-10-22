@echo off
chcp 65001 >nul
setlocal
title MultiKVM - инсталиране

echo ==============================================
echo   MultiKVM - инсталация
echo ==============================================
echo.
echo   1. Отваря TCP 9876 (и 9877) в защитната стена
echo   2. Добавя автостарт при вход в Windows
echo   3. Стартира MultiKVM
echo.

net session >nul 2>&1
if errorlevel 1 (
  echo ИЗИСКВА СЕ ПРАВО НА АДМИНИСТРАТОР.
  echo Стартирай този файл с десен клик ^> "Изпълни като администратор".
  echo.
  pause
  exit /b 1
)

echo [1/3] Изтриване на старото правило...
netsh advfirewall firewall delete rule name="MultiKVM" >nul 2>&1

echo [2/3] Отваряне на TCP 9876,9877 в защитната стена...
netsh advfirewall firewall add rule name="MultiKVM" dir=in action=allow protocol=TCP localport=9876,9877 profile=any enable=yes
if errorlevel 1 (
  echo ГРЕШКА при добавяне на правилото!
  pause
  exit /b 1
)
echo       -> правилото е добавено.

echo [3/3] Настройка на автостарт...
reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "MultiKVM" /t REG_SZ /d "\"%~dp0MultiKVM.exe\"" /f >nul
echo       -> готов е.

echo.
echo Стартиране на MultiKVM...
start "" "%~dp0MultiKVM.exe"
echo.
echo Готово! Може да затвориш този прозорец.
pause
endlocal
