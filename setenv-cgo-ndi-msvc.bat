@echo off
rem Configure environment for Go CGO build with NDI (MSVC toolchain)
rem Usage (from an MSVC x64 Native Tools Command Prompt):
rem   call setenv-cgo-ndi-msvc.bat
rem Then build:
rem   go run ./cmd/whep

setlocal

rem Adjust this if your NDI SDK is installed elsewhere
set "NDI_SDK_DIR=C:\Program Files\NDI\NDI 6 SDK"
set "NDI_INCLUDE=%NDI_SDK_DIR%\Include"
set "NDI_LIB64=%NDI_SDK_DIR%\Lib\x64"

rem Enable CGO and select MSVC compiler
set "CGO_ENABLED=1"
set "CC=cl"

rem CGO flags for NDI headers and libs (quote paths with spaces)
set "CGO_CFLAGS=-I""%NDI_INCLUDE%"""
set "CGO_LDFLAGS=-L""%NDI_LIB64%"" -lProcessing.NDI.Lib.x64"

rem Ensure the DLL can be found at runtime: add repo root (current dir) and NDI lib dir to PATH
set "PATH=%CD%;%NDI_LIB64%;%PATH%"

rem Work around VS environments that inject invalid flags like /Werror via CL
rem Clear CL so cgo does not inherit incompatible MSVC flags from the environment
set "CL="

rem Basic validations
if not exist "%NDI_INCLUDE%\Processing.NDI.Lib.h" (
  echo [WARN] NDI header not found at "%NDI_INCLUDE%\Processing.NDI.Lib.h"
)
if not exist "%NDI_LIB64%\Processing.NDI.Lib.x64.lib" (
  echo [WARN] NDI import library not found at "%NDI_LIB64%\Processing.NDI.Lib.x64.lib"
)

echo.
echo CGO_ENABLED=%CGO_ENABLED%
echo CC=%CC%
echo CGO_CFLAGS=%CGO_CFLAGS%
echo CGO_LDFLAGS=%CGO_LDFLAGS%
echo.
echo Environment configured for CGO + NDI (MSVC).
echo Next: go run ./cmd/whep

endlocal & (
  set "CGO_ENABLED=1"
  set "CC=cl"
  set "CGO_CFLAGS=-I""%NDI_INCLUDE%"""
  set "CGO_LDFLAGS=-L""%NDI_LIB64%"" -lProcessing.NDI.Lib.x64"
  set "CL="
  set "PATH=%CD%;%NDI_LIB64%;%PATH%"
)
