@echo off
cd /d "%~dp0"
if not exist "bin\bot.exe" (
  echo bot.exe not found. Building...
  go build -o bin\bot.exe .\cmd\bot
  if errorlevel 1 (
    echo Build failed.
    pause
    exit /b 1
  )
)
echo Starting bot...
bin\bot.exe
echo.
echo Bot stopped.
pause
