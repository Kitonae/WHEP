# Building third-party dependencies (Windows, MSYS2/MinGW-w64)

This document describes how to build static libyuv and libvpx that are ABI-compatible with our MinGW-w64 build of WHEP, and how the build script consumes them.

Prerequisites
- MSYS2 installed (https://www.msys2.org/)
- MSYS2 packages:
  - Base tools: make, gawk, sed, diffutils
  - Toolchain: mingw-w64-x86_64-gcc, mingw-w64-x86_64-g++, mingw-w64-x86_64-pkg-config
  - Assemblers: nasm, yasm

From an elevated PowerShell or Windows Terminal, you can install prerequisites inside MSYS2:
- Open “MSYS2 MSYS” or run: C:\msys64\usr\bin\bash.exe -lc "pacman -S --needed make gawk sed diffutils nasm yasm"
- Ensure MinGW64 binaries are present: C:\msys64\mingw64\bin\gcc.exe

Directory layout
- 3pp/libyuv
  - src/ (libyuv source checkout)
  - lib/ (built libraries copied here: yuv.lib, libyuv.a)
  - include/ (optional, not required; we include headers from src/include)
- 3pp/libvpx
  - src/ (libvpx source checkout and build output libvpx.a)
  - lib/ (copy of libvpx.a)
  - include.bak/ (archived headers not used; we include headers from src to match ABI)

libyuv: build static library
1) Fetch sources (if not present):
   git clone --depth 1 https://chromium.googlesource.com/libyuv/libyuv 3pp/libyuv/src

2) Generate and build with MinGW (using CMake MinGW generator):
   - Configure (GCC/G++):
     cmake -S 3pp/libyuv/src -B 3pp/libyuv/build-mingw -G "MinGW Makefiles" -DBUILD_SHARED_LIBS=OFF -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_COMPILER=gcc -DCMAKE_CXX_COMPILER=g++
   - Build:
     cmake --build 3pp/libyuv/build-mingw --config Release -j 8

3) Place outputs for the WHEP build script to discover:
   - Static lib:
     copy 3pp\libyuv\build-mingw\libyuv.a 3pp\libyuv\lib\libyuv.a
   - Import lib (optional):
     copy 3pp\libyuv\build-mingw\yuv.lib 3pp\libyuv\lib\yuv.lib

libvpx: build static library
1) Fetch sources (if not present):
   git clone --depth 1 https://chromium.googlesource.com/webm/libvpx 3pp/libvpx/src

2) Build with MSYS2 MinGW64 environment (requires nasm/yasm and base tools):
   C:\msys64\usr\bin\env.exe MSYSTEM=MINGW64 PATH=/mingw64/bin:/usr/bin:%PATH% C:\msys64\usr\bin\bash.exe -lc "set -e; cd /c/src/WHEP/3pp/libvpx/src; ./configure --target=x86_64-win64-gcc --disable-examples --disable-tools --disable-docs --enable-vp8 --enable-vp9 --enable-static --disable-shared; make -j8"

3) Place output for the WHEP build script:
   copy 3pp\libvpx\src\libvpx.a 3pp\libvpx\lib\libvpx.a

Build script behavior (build-mingw-auto.bat)
- Prefers static libraries:
  - libvpx: links with -l:libvpx.a if present; otherwise falls back to -lvpx
  - libyuv: links with -lyuv (import lib) when present; libyuv.a is also supported via standard library search
- Header include paths:
  - libvpx: forced to 3pp/libvpx/src so headers match the exact version that produced libvpx.a (prevents vpx ABI mismatches)
  - libyuv: 3pp/libyuv/src/include
- The script sets CGO and LDFLAGS for MinGW-w64 and links NDI from the 64-bit SDK path.

Troubleshooting
- ABI version mismatch at runtime (vpx_codec_enc_init_ver failed):
  - Ensure we are linking statically against libvpx.a built on the same machine/toolchain.
  - Ensure include path is 3pp/libvpx/src (not include.bak or any prebuilt include tree).
- Linker cannot find -lyuv or -lvpx:
  - Verify libraries exist in 3pp/libyuv/lib and 3pp/libvpx/lib, respectively.
  - PATH should include these lib folders as set by the script, but static archives are found via -L and -l.
- Missing MSYS2 tools during libvpx build:
  - Install with pacman: make gawk sed diffutils nasm yasm; confirm /mingw64/bin/gcc exists.

Regenerating everything from scratch
1) libyuv:
   - Remove 3pp/libyuv/build-mingw and 3pp/libyuv/lib/*
   - Re-run the libyuv build steps above
2) libvpx:
   - make distclean in 3pp/libvpx/src (or reclone), rebuild as above
3) Run the project build:
   .\build-mingw-auto.bat

