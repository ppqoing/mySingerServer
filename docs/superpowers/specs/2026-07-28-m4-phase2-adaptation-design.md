# M4 Phase-2 Repository Adaptation Design

## Status

Approved by the project owner through the standing instruction to execute
`docs/details/M4-phase2.md` and continue M2 through M6. This document does not
change that specification; it maps it onto the repository that exists after
M3.

## Scope

M4 keeps the detailed document's boundaries:

- compute partition pHash and Sobel from one decoded grayscale surface;
- compute six video frames at `(1,3,5,7,9,11)/12` of duration;
- dispatch only missing phase-2 fields to the owning Agent;
- rescreen image/video candidate pairs, persist `pair_scores`, and rebuild
  only confirmed `image`/`video` groups;
- expose exact/image/video groups and score detail through the existing GUI
  HTTP service;
- do not change M3 phase-1 features, implement deletion, or add preview
  streaming.

The second independent Windows host remains outside verification scope by
explicit owner waiver. Local Windows, PostgreSQL 16, the existing Agent/GUI
loopback topology, and deterministic fixtures remain required.

## Repository Mapping

The detailed document uses conceptual multi-module paths. The implemented
repository is one Go module, so the authoritative mapping is:

| Detailed-document concept | Repository path |
|---|---|
| `shared/proto` | `internal/proto` |
| `shared/features` | `internal/features` |
| Agent mediacore binding | `internal/wproc/mediacore` |
| Agent Worker phase-2 pipeline | `internal/wproc` + `internal/worker` |
| Agent task admission/persistence | `internal/agent` + `internal/store` |
| GUI phase-2 domain | `internal/phase2` |
| GUI HTTP/API and embedded page | `internal/gui` |
| GUI composition | `cmd/gui` |

Existing `deploy/central.sql` already contains nullable phase-2 feature
columns, `video_frames`, and `pair_scores`. M4 may make idempotent constraint
or index corrections but must not introduce a second migration system.

## Architecture

Implementation is contract-first and vertically integrated:

1. `internal/features` owns portable versioned BLOB codecs and comparison
   primitives. `internal/proto` owns map-encoded wire structs only.
2. `mediacore.dll` owns grayscale resize, partition DCT hashes, and Sobel
   histogram calculation. The Go binding returns Go-native arrays and never
   exposes a C allocation.
3. `internal/wproc` processes a phase-2 job with the same file identity/stale
   checks and watchdog boundary used by phase 1. Images decode once. Videos
   run six exact output-side seeks and process each frame independently.
4. `internal/worker` persists successful fields atomically enough to preserve
   partial results, updates `missing_mask/phase2_done`, emits one field error
   per failed field, and returns the phase-2 payload to Agent task orchestration.
5. `internal/agent` accepts `Phase2Task` beside `ScanTask`, shards work through
   the existing pool, and returns normal Ack/Progress/FeatureResult/Done
   messages.
6. `internal/phase2` reads M3 candidate content keys, chooses one live copy per
   SHA, computes field/frame masks, dispatches shards of at most 5000 items,
   judges pairs when both endpoints are ready, persists normalized scores, and
   transactionally rebuilds confirmed groups.
7. `internal/gui` wires automatic dispatch after a successful M3 run and
   exposes read-only group list/detail APIs plus the embedded three-tab page.

## Data and Compatibility Contracts

- SHA-512 remains canonical lowercase 128-character `TEXT` outside native
  algorithm boundaries.
- Protocol structs remain msgpack maps. Optional result payloads are appended
  with `omitempty`; required `Phase2Item` identity/routing fields retain
  explicit zero values on the wire. Existing message type numbers are
  unchanged.
- `phash_parts` is exactly 76 bytes: header `1,3,3,0`, then nine little-endian
  `uint64`.
- `sobel_hist` is exactly 516 bytes: header `1,4,8,0`, then 128 finite
  little-endian `float32` values.
- Six-frame rows use `frame_idx` 0 through 5. A phase-2 result can be partial;
  success bits and field errors must agree.
- Candidate identity is `(kind, min(shaA,shaB), max(shaA,shaB))`; no M4 code
  persists an M3 `dup_groups.id`.
- M4 replacement deletes/rebuilds only confirmed `kind=image/video`; exact
  and M3 candidate kinds are preserved.

## Error and Lifecycle Design

- Stale size/mtime/file identity rejects all derived fields and requests a
  phase-1 rescan; stale output is never stored.
- Image jobs use the existing 30-second watchdog; video jobs use 120 seconds.
  A single ffmpeg frame command has a 20-second context.
- Fewer than four valid video frames yields `inconclusive`, not `false`.
- Offline Agent dispatch remains pending and retries on the existing
  reconnect callback. Duplicate task/result delivery is idempotent.
- Database writes use bounded contexts and transactions. A failed group
  rebuild leaves the previous confirmed groups intact.
- GUI shutdown stops new automatic dispatch, cancels active work, and waits
  for accepted orchestration before closing shared resources.

## Testing and Acceptance

Each layer has a narrow RED-to-GREEN test:

- native deterministic/boundary/similar/dissimilar tests and export audit;
- BLOB/protocol round trips and malformed input rejection;
- image decode-once, stale detection, video timestamp/timeout/partial-frame
  tests;
- SQLite phase-2 persistence and Agent message lifecycle;
- dispatcher dedupe/mask/shard/offline retry tests;
- image/video threshold boundaries, union-find determinism, restart recovery,
  and transactional group rebuild tests;
- PostgreSQL 16 end-to-end E1 through E4 with two local Agent identities;
- API and embedded-page tests;
- one fail-closed `scripts/verify_m4.ps1` with machine-readable evidence,
  cleanup audit, secret scan, and `SECOND_WINDOWS_STATUS=USER_WAIVED`.

No unchecked placeholder, threshold weakening, test skip, or production-only
dependency is accepted as completion evidence.
