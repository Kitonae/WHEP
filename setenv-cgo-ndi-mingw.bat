@echo off
rem Configure environment for Go CGO build with NDI (MinGW-w64 GCC toolchain)
rem Usage (from a Command Prompt with gcc in PATH):
rem   call setenv-cgo-ndi-mingw.bat
rem Then build:
rem   go run ./cmd/whep

setlocal

rem Adjust this if your NDI SDK is installed elsewhere
set "NDI_SDK_DIR=C:\Program Files\NDI\NDI 5 SDK"
set "NDI_INCLUDE=%NDI_SDK_DIR%\Include"
set "NDI_LIB64=%NDI_SDK_DIR%\Lib\x64"

rem Enable CGO and select MinGW GCC
set "CGO_ENABLED=1"
set "CC=gcc"

rem CGO flags for NDI headers and libs (quote paths with spaces)
set "CGO_CFLAGS=-I""%NDI_INCLUDE%"""
set "CGO_LDFLAGS=-L""%NDI_LIB64%"" -lProcessing.NDI.Lib.x64"

rem Ensure the DLL can be found at runtime: add repo root (current dir) and NDI lib dir to PATH
set "PATH=%CD%;%NDI_LIB64%;%PATH%"

rem Basic validations
if not exist "%NDI_INCLUDE%\Processing.NDI.Lib.h" (
  echo [WARN] NDI header not found at "%NDI_INCLUDE%\Processing.NDI.Lib.h"
)
if not exist "%NDI_LIB64%\Processing.NDI.Lib.x64.lib" (
  echo [WARN] NDI import library not found at "%NDI_LIB64%\Processing.NDI.Lib.x64.lib"
  echo        If linking fails with GCC, prefer using the MSVC script instead.
)

echo.
echo CGO_ENABLED=%CGO_ENABLED%
echo CC=%CC%
echo CGO_CFLAGS=%CGO_CFLAGS%
echo CGO_LDFLAGS=%CGO_LDFLAGS%
echo.
echo Environment configured for CGO + NDI (MinGW).
echo Next: go run ./cmd/whep

endlocal & (
  set "CGO_ENABLED=1"
  set "CC=gcc"
  set "CGO_CFLAGS=-I""%NDI_INCLUDE%"""
  set "CGO_LDFLAGS=-L""%NDI_LIB64%"" -lProcessing.NDI.Lib.x64"
  set "PATH=%CD%;%NDI_LIB64%;%PATH%"
)
