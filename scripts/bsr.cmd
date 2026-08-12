@echo off
rem bsr - BSRouter gateway process manager (Windows shim).
rem Forwards to bsr.ps1 in the same directory.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0bsr.ps1" %*
exit /b %ERRORLEVEL%
