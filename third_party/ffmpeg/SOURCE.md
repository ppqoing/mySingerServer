# FFmpeg SDK source evidence

**RELEASE BLOCKED**

## Locally proven facts

- `include/libavutil/ffversion.h` defines
  `N-125444-g6d72600a30-20260703`.
- The three local executables report that same version token and a matching
  configure line.
- The version token exposes only the abbreviated revision token
  `g6d72600a30`; it does not prove a full source commit.
- Component versions reported by the local executables are captured in
  `manifest.json`.
- The full configure argument sequence reported by the local executables is
  captured in `manifest.json`.

## Evidence not present in this SDK

No file in the supplied SDK identifies a distributor or upstream download URL
for this exact binary bundle. No corresponding source archive, full commit
identifier, source archive SHA-256, or authoritative source offer was supplied.

Accordingly, `provenance.source_url`, `provenance.commit`, and
`provenance.source_archive_sha256` are `null`; the manifest keeps
`redistributable` false. These values must not be filled from inference or a
similar-looking public build.
