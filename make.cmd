@echo off
rem zMail Windows build entry: forwards args to make.ps1 with execution policy bypass.
rem Usage: make.cmd build ^| make.cmd build-linux ^| make.cmd check ^| make.cmd run ...
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0make.ps1" %*
exit /b %ERRORLEVEL%
