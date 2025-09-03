@echo off
setlocal

echo.
echo ========================================
echo   WHEP Build Script - MinGW-w64 (Auto)
echo ========================================
echo.

rem Check for MinGW-w64/GCC
where gcc >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [ERROR] GCC not found in PATH
    echo.
    echo Please install MinGW-w64:
    echo 1. Via Chocolatey: choco install mingw
    echo 2. Via MSYS2: pacman -S mingw-w64-x86_64-gcc
    echo 3. Download from: https://www.mingw-w64.org/downloads/
    echo.
    echo Make sure gcc.exe is in your PATH after installation.
    exit /b 1
)

rem Check for Go
go version >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Go not found in PATH
    echo Please install Go from https://golang.org/
    exit /b 1
)

rem Set up paths
set "NDI_SDK_DIR=C:\Program Files\NDI\NDI 6 SDK"
set "NDI_INCLUDE=%NDI_SDK_DIR%\Include"
set "NDI_LIB64=%NDI_SDK_DIR%\Lib\x64"
set "LIBVPX_PATH=%CD%\3pp\libvpx"
set "LIBYUV_PATH=%CD%\3pp\libyuv"

rem Dependency checks
echo Checking dependencies...

rem Check NDI SDK
if not exist "%NDI_INCLUDE%\Processing.NDI.Lib.h" (
    echo [WARN] NDI headers not found at "%NDI_INCLUDE%\Processing.NDI.Lib.h"
    echo       Please install NDI SDK from https://ndi.video/
    set "NDI_AVAILABLE=0"
) else (
    echo [✓] NDI headers found
    set "NDI_AVAILABLE=1"
)

if not exist "%NDI_LIB64%\Processing.NDI.Lib.x64.lib" (
    echo [WARN] NDI library not found at "%NDI_LIB64%\Processing.NDI.Lib.x64.lib"
    echo       Please install NDI SDK from https://ndi.video/
    set "NDI_AVAILABLE=0"
) else (
    echo [✓] NDI library found
)

rem Check libvpx
if not exist "%LIBVPX_PATH%\include\vpx\vpx_encoder.h" (
    echo [WARN] libvpx headers not found at "%LIBVPX_PATH%\include\vpx\vpx_encoder.h"
    echo       VP8 encoding will not be available
    set "VPX_AVAILABLE=0"
) else (
    echo [✓] libvpx headers found
    set "VPX_AVAILABLE=1"
)

set "VPX_LIB_OK=0"
if exist "%LIBVPX_PATH%\lib\libvpx.a" (
    echo [✓] libvpx static library found: libvpx.a
    set "VPX_LIB_OK=1"
)
if exist "%LIBVPX_PATH%\lib\vpx.lib" (
    echo [✓] libvpx import library found: vpx.lib
    set "VPX_LIB_OK=1"
)
if "%VPX_LIB_OK%"=="0" (
    echo [WARN] libvpx library not found at "%LIBVPX_PATH%\lib\vpx.lib" or "%LIBVPX_PATH%\lib\libvpx.a"
    echo       VP8 encoding will not be available
    set "VPX_AVAILABLE=0"
)

rem Check libyuv
if not exist "%LIBYUV_PATH%\src" (
    echo [WARN] libyuv sources not found at "%LIBYUV_PATH%\src"
    echo       You can fetch with: git clone https://chromium.googlesource.com/libyuv/libyuv "%LIBYUV_PATH%\src"
    set "YUV_AVAILABLE=0"
) else (
    if exist "%LIBYUV_PATH%\src\include\libyuv.h" (
        echo [✓] libyuv headers found
        set "YUV_AVAILABLE=1"
    ) else (
        echo [WARN] libyuv headers not found under src\include
        set "YUV_AVAILABLE=0"
    )
)

set "YUV_LIB_OK=0"
if exist "%LIBYUV_PATH%\lib\libyuv.a" (
    echo [✓] libyuv static library found: libyuv.a
    set "YUV_LIB_OK=1"
)
if exist "%LIBYUV_PATH%\lib\yuv.lib" (
    echo [✓] libyuv import library found: yuv.lib
    set "YUV_LIB_OK=1"
)
if "%YUV_LIB_OK%"=="0" (
    echo [WARN] libyuv library not found at "%LIBYUV_PATH%\lib\yuv.lib" or "%LIBYUV_PATH%\lib\libyuv.a"
    set "YUV_AVAILABLE=0"
)

rem Check GCC version
echo [✓] GCC found:
gcc --version | findstr "gcc"

rem Configure CGO environment for MinGW-w64
echo.
echo Configuring CGO environment for MinGW-w64...

set "CGO_ENABLED=1"
set "CC=gcc"
set "CXX=g++"

rem Use MSYS2-style paths if available, otherwise short paths
echo Attempting to resolve path issues...

rem Try to convert to 8.3 format to avoid spaces
for %%i in ("%NDI_INCLUDE%") do set "NDI_INCLUDE_SAFE=%%~si"
for %%i in ("%NDI_LIB64%") do set "NDI_LIB64_SAFE=%%~si"
for %%i in ("%LIBVPX_PATH%") do set "LIBVPX_PATH_SAFE=%%~si"
for %%i in ("%LIBYUV_PATH%") do set "LIBYUV_PATH_SAFE=%%~si"
for %%i in ("%LIBYUV_PATH%\src\include") do set "LIBYUV_INCLUDE_SAFE=%%~si"
for %%i in ("%LIBYUV_PATH%\lib") do set "LIBYUV_LIB_SAFE=%%~si"

rem Fallback to original if 8.3 conversion fails
if "%NDI_INCLUDE_SAFE%"=="" set "NDI_INCLUDE_SAFE=%NDI_INCLUDE%"
if "%NDI_LIB64_SAFE%"=="" set "NDI_LIB64_SAFE=%NDI_LIB64%"
if "%LIBVPX_PATH_SAFE%"=="" set "LIBVPX_PATH_SAFE=%LIBVPX_PATH%"
if "%LIBYUV_PATH_SAFE%"=="" set "LIBYUV_PATH_SAFE=%LIBYUV_PATH%"
if "%LIBYUV_INCLUDE_SAFE%"=="" set "LIBYUV_INCLUDE_SAFE=%LIBYUV_PATH%\src\include"
if "%LIBYUV_LIB_SAFE%"=="" set "LIBYUV_LIB_SAFE=%LIBYUV_PATH%\lib"

rem CGO flags using safe paths and proper Windows library linking
rem Add flags for Windows mingw runtime and libvpx
set "CGO_CFLAGS=-I%NDI_INCLUDE_SAFE% -I%LIBVPX_PATH_SAFE%\include -I%LIBYUV_INCLUDE_SAFE%"
set "CGO_LDFLAGS=-L%LIBVPX_PATH_SAFE%\lib -L%LIBYUV_LIB_SAFE% -L%NDI_LIB64_SAFE% -lProcessing.NDI.Lib.x64 -L%LIBVPX_PATH_SAFE%\lib -lvpx -L%LIBYUV_LIB_SAFE% -lyuv -lmingwex -lmingw32 -lwinmm -lmsvcrt -luser32 -luuid"

rem Allow all CGO flags (MinGW is more permissive than MSVC)
set "CGO_CFLAGS_ALLOW=.*"
set "CGO_LDFLAGS_ALLOW=.*"
set "CGO_CXXFLAGS_ALLOW=.*"

rem Add runtime library paths
set "PATH=%CD%;%NDI_LIB64%;%LIBVPX_PATH%\lib;%LIBYUV_PATH%\lib;%PATH%"

rem Display configuration
echo.
echo CGO Configuration:
echo   CGO_ENABLED=%CGO_ENABLED%
echo   CC=%CC%
echo   CXX=%CXX%
echo   NDI_INCLUDE_SAFE=%NDI_INCLUDE_SAFE%
echo   NDI_LIB64_SAFE=%NDI_LIB64_SAFE%
echo   LIBVPX_PATH_SAFE=%LIBVPX_PATH_SAFE%
echo   CGO_CFLAGS=%CGO_CFLAGS%
  echo   CGO_LDFLAGS=%CGO_LDFLAGS%
echo   LIBYUV_PATH_SAFE=%LIBYUV_PATH_SAFE%
  echo   LIBYUV_INCLUDE_SAFE=%LIBYUV_INCLUDE_SAFE%
  echo   LIBYUV_LIB_SAFE=%LIBYUV_LIB_SAFE%
echo.

echo Building executable with NDI + VP8 support...
if "%VPX_AVAILABLE%"=="0" (
    echo [WARN] libvpx not available, building NDI-only version instead
    if "%YUV_AVAILABLE%"=="1" (
        echo [INFO] libyuv present, enabling yuv tag
        go build -v -tags yuv -o whep.exe ./cmd/whep
    ) else (
        go build -v -o whep.exe ./cmd/whep
    )
) else (
    if "%YUV_AVAILABLE%"=="1" (
        go build -v -tags "vpx yuv" -o whep.exe ./cmd/whep
    ) else (
        go build -v -tags vpx -o whep.exe ./cmd/whep
    )
)

if %ERRORLEVEL% equ 0 (
    echo.
    echo ✓ Build successful: whep.exe
    if "%VPX_AVAILABLE%"=="1" (
        if "%YUV_AVAILABLE%"=="1" (
            echo ✓ Features: NDI streaming + VP8 encoding + libyuv conversions
        ) else (
            echo ✓ Features: NDI streaming + VP8 encoding
        )
    ) else (
        if "%YUV_AVAILABLE%"=="1" (
            echo ✓ Features: NDI streaming + libyuv conversions
        ) else (
            echo ✓ Features: NDI streaming only
        )
    )
    echo ✓ Compiler: MinGW-w64 GCC
) else (
    echo.
    echo ✗ Build failed with error level %ERRORLEVEL%
    if "%VPX_AVAILABLE%"=="1" (
        echo Attempting NDI-only build as fallback...
        go build -v -o whep.exe ./cmd/whep
        if %ERRORLEVEL% equ 0 (
            echo ✓ Fallback build successful: whep.exe
            echo ✓ Features: NDI streaming only
            echo ✓ Compiler: MinGW-w64 GCC
        ) else (
            echo ✗ Fallback build also failed with error level %ERRORLEVEL%
            echo Try building with -x flag for verbose output
        )
    ) else (
        echo Try building with -x flag for verbose output
    )
)

endlocal
