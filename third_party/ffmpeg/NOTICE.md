# FFmpeg SDK redistribution notice

**RELEASE BLOCKED**

The checked-in SDK is pinned for local build and integrity verification only.
The repository does not currently possess authoritative redistribution
evidence for this exact binary build.

Known local evidence:

- SDK version token: `N-125444-g6d72600a30-20260703`
- `ffmpeg.exe`, `ffprobe.exe`, and `ffplay.exe` report matching build,
  configure, and component-version information.
- The embedded configure line contains `--enable-gpl` and
  `--enable-version3`.

Missing evidence:

- distributor or upstream source URL for this exact SDK;
- full exact source commit;
- SHA-256 of the corresponding source archive;
- distributor-provided license and third-party notice bundle;
- an authoritative corresponding-source and redistribution statement.

This verifier intentionally remains `RELEASE BLOCKED`: manifest fields cannot
self-authorize redistribution. Do not publish or redistribute this SDK or a
package containing its DLLs until a separately controlled authoritative review
gate is implemented, binds approval to the manifest and evidence-document
digests, and explicitly authorizes the exact SDK.
