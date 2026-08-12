@echo off
rem BSRouter bsr installer (Windows shim).
rem Forwards to install.ps1 in the same directory.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
exit /b %ERRORLEVEL%
