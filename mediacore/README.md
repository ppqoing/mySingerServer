# mediacore

`mediacore.dll` is the Windows x64 native feature-computation library for
mySingerServer. Its public interface is the stable C ABI in
`include/mediacore/mediacore.h`.

Task 3 implements streaming SHA-512 with Windows CNG. Task 4 adds magic-byte
JPEG/PNG/WebP decoding, stb fallback for GIF/BMP/TGA/PNM, an owned integer
BT.601 grayscale plane, and PDQ-256.

Build and test from the repository root:

```powershell
pwsh -File scripts/build.ps1 -MediacoreOnly `
  -CMake C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe `
  -VcpkgRoot C:\vcpkg
```

The Release DLL is copied to `bin/mediacore.dll`.

Run the complete native M2 regression gate from the repository root:

```powershell
pwsh -File scripts/verify_m2_native.ps1
```

The gate rebuilds the DLL, runs CTest, independently compares all 72 Level A
luma vectors against the unwrapped upstream PDQ implementation, runs 20 local
Level B images plus all 49 supported images from the pinned upstream
`pdq/data` corpus, and isolates corrupt-input checks in timeout-bounded
subprocesses. Level B golden pixels are decoded through Windows Imaging
Component, independently of the DLL decoder backends.

## Pinned image hashing sources

PDQ is copied byte-for-byte from `facebook/ThreatExchange` commit
`baefb4ed67b6cdc1d4c82dbaef858d50866ac424`. Only
`pdq/cpp/common`, `pdq/cpp/downscaling`, `pdq/cpp/hashing`, and the root
`LICENSE` are included under `src/pdq_upstream`. The source archive used for
this workspace has SHA-256:

```text
b167b1b76b178a9face8442f5ef88396ea364b5bd117fb109a87dd157331074e
```

The stb fallback uses `stb_image.h` and `LICENSE` from stb commit
`f75e8d1cad7d90d72ef7a4661f1b994ef78b4e31`. The source archive SHA-256 is:

```text
893e084a0635b5186ef9da30f105cf1af437ddbbd504a8b86b83e9535dedd638
```

## Pinned local vcpkg snapshot

This host uses the exact source snapshot installed at `C:\vcpkg`. The snapshot
does not contain Git metadata, so the manifest intentionally has no
`builtin-baseline`; dependency names and the `x64-windows-static` triplet remain
unchanged.

```text
vcpkg package management program version 2026-04-08-e0612b42ce44e55a0e630f2ee9d3c533a63d8bc1

libjpeg-turbo/vcpkg.json
  sha256 82dc5be3acdf6f3bb1709233272188fffc46288f3285271631ae14851406d866
libjpeg-turbo/portfile.cmake
  sha256 fbad28a83aca81fad41181bec0d3f9b4ab9fabe697a51be1425ccecc6d996803
libpng/vcpkg.json
  sha256 3749b649ea8ce7bbe96346a7c0289ec5ca56418f4d9ef9881e30d90b7ec0b8ff
libpng/portfile.cmake
  sha256 5677ed31f47aa657854332f181f5497f22a7ce431c48e9036e0ce04639953af4
libwebp/vcpkg.json
  sha256 eb8262ead7b0a5f82f6e2335cae1d42e99fc9eda21bb8ab52689f0d62fbbc058
libwebp/portfile.cmake
  sha256 889bbac3ca6b249d9286f083666dc66ec7a353fd67d65b22c108772e7f6b6b1b
```
