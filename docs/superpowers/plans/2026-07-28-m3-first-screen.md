# M3 First-Screen Analysis Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a repeatable GUI-process first-screen analyzer that reads M2 features from PostgreSQL, generates exact/image/video candidates without quadratic image comparison, transactionally rewrites M3 result kinds, exposes run/status HTTP endpoints, and proves the million-row acceptance gates.

**Architecture:** Add a pure-Go `internal/firstscreen` package containing deterministic in-memory screening algorithms and a PostgreSQL adapter. The existing GUI owns one analyzer and prevents concurrent runs; M3 does not change Agent protocol, Worker, cgo, or `mediacore.dll`. Results are replaced in one repeatable-read transaction for only `exact`, `image_candidate`, and `video_candidate`.

**Tech Stack:** Go 1.26.5 (minimum 1.22), `pgx/v5`, PostgreSQL 16, `net/http`, `math/bits`, structured `slog`, Docker PostgreSQL integration tests.

## Global Constraints

- Use the current root module `dedup`; all new production code lives under `internal/firstscreen`, not the obsolete example path `gui/internal/firstscreen`.
- The current central schema stores `files.sha512`, `image_features.sha512`, and `video_features.sha512` as canonical 128-character lowercase hex `TEXT`; decode to `[64]byte` only at the Go boundary. Do not change these columns to `BYTEA`.
- `pdq256` and `thumb_pdq256` are exactly 32 raw bytes decoded as four big-endian `uint64` words.
- Default thresholds are Hamming `31`, aspect tolerance `0.10`, video duration window `2000ms`, and image quality minimum `50`.
- Image candidate generation must use four 64-bit exact-band inverted indexes; do not introduce O(n²) full image comparison.
- M3 may write only `exact`, `image_candidate`, and `video_candidate`; it must not delete or rewrite M4 `image`/`video` results.
- Candidate identity is `(kind, sha_a, sha_b)` with lexicographically ordered SHA values; `dup_groups.id` is not stable across runs.
- No GUI↔Agent protocol, Agent, Worker, cgo, or native DLL changes are in scope.
- Every PostgreSQL integration test uses run-unique machine/path/SHA data, restores or deletes only its own rows, and proves cleanup.
- This workspace is not a Git repository; replace commit steps with an SDD report, exact changed-file list, fresh commands, and independent review.

---

### Task 1: Configuration, Core Types, and Hash Primitives

**Files:**
- Create: `internal/firstscreen/config.go`
- Create: `internal/firstscreen/hamming.go`
- Create: `internal/firstscreen/pairs.go`
- Create: `internal/firstscreen/core_test.go`
- Modify: `internal/config/gui.go`
- Modify: `internal/config/config_test.go`
- Modify: `deploy/gui.example.json`

**Interfaces:**
- Produce `firstscreen.Config`, `DefaultConfig() Config`, `Validate() error`.
- Produce `hamming256([4]uint64,[4]uint64) int`, `pdqFromBytes([]byte) ([4]uint64,bool)`, and `shaFromText(string) ([64]byte,bool)`.
- Produce `CandidatePair`, `newCandidatePair`, `scoreJSON`, `M3Kinds`, and the three M3 kind constants.
- Add `FirstScreen firstscreen.Config`-equivalent JSON fields to `config.GUIConfig` without importing an internal child package into `internal/config`; use a local `FirstScreenConfig` DTO and a conversion in GUI composition.

- [x] **Step 1: Write failing primitive and configuration tests**

```go
func TestPDQFromBytesUsesBigEndianAndRejectsWrongLength(t *testing.T) {
    raw := make([]byte, 32)
    binary.BigEndian.PutUint64(raw[0:8], 0x0102030405060708)
    got, ok := pdqFromBytes(raw)
    if !ok || got[0] != 0x0102030405060708 { t.Fatalf("got=%x ok=%t", got[0], ok) }
    if _, ok := pdqFromBytes(raw[:31]); ok { t.Fatal("accepted 31-byte PDQ") }
}

func TestSHAFromTextRequiresCanonicalSHA512(t *testing.T) {
    valid := strings.Repeat("ab", 64)
    if _, ok := shaFromText(valid); !ok { t.Fatal("rejected canonical SHA") }
    for _, bad := range []string{strings.ToUpper(valid), valid[:127], valid + "0", strings.Repeat("gg", 64)} {
        if _, ok := shaFromText(bad); ok { t.Fatalf("accepted %q", bad) }
    }
}
```

Add table tests for all defaults, invalid negative/zero page and batch sizes, Hamming outside `0..256`, aspect tolerance outside `0..1`, deterministic SHA ordering, and exact image/video `score_json` fields.

- [x] **Step 2: Run RED**

```powershell
$env:CGO_ENABLED='0'
& $Go test -count=1 ./internal/firstscreen ./internal/config
```

Expected: package/types and GUI first-screen config fields do not exist.

- [x] **Step 3: Implement the exact interfaces**

Use the defaults and JSON keys:

```json
{
  "firstscreen": {
    "hamming_max": 31,
    "aspect_tolerance": 0.10,
    "video_duration_window_ms": 2000,
    "image_quality_min": 50,
    "read_page_size": 50000,
    "group_insert_batch": 1000,
    "sha_resolve_chunk": 10000
  }
}
```

Encode `peer_sha512` as lowercase 128-character hex. Build score JSON with `encoding/json`, not string concatenation.

- [x] **Step 4: Run GREEN and formatting**

```powershell
& $Go test -count=20 ./internal/firstscreen ./internal/config
& $Go fmt ./internal/firstscreen ./internal/config
```

- [x] **Step 5: Write `.superpowers/sdd/2026-07-28-m3-first-screen/task-1-report.md` and request independent review**

---

### Task 2: Band Index and Image Screening

**Files:**
- Create: `internal/firstscreen/bandindex.go`
- Create: `internal/firstscreen/image_screen.go`
- Create: `internal/firstscreen/image_screen_test.go`

**Interfaces:**
- Consume Task 1 hash primitives and `CandidatePair`.
- Produce `bandIndex`, `ImageFeature`, `aspectClose`, and `screenImages`.

- [x] **Step 1: Write failing correctness, recall, and determinism tests**

Cover:

```go
func TestBandIndexRecallWithinThreeBits(t *testing.T) {
    // Deterministically generate 10,000 base hashes and mutations at distance 0..3.
    // Every mutation must return the base index and query must contain no duplicate index.
}

func TestScreenImagesFiltersQualityAspectAndHamming(t *testing.T) {
    // Two quality>=50 same-aspect hashes at distance 31 pair.
    // Distance 32, quality 49, and >10% aspect variants do not pair.
}
```

Also compare randomized output with a naive oracle restricted to pairs sharing at least one exact band, test missing dimensions pass aspect pruning, verify no duplicate pairs when two hashes share multiple bands, and repeat deterministic output 20 times.

- [x] **Step 2: Run RED**

```powershell
& $Go test -count=1 ./internal/firstscreen -run 'Test(BandIndex|ScreenImages|AspectClose)'
```

- [x] **Step 3: Implement the timestamp-deduplicated four-band index and image pipeline**

Use `map[bandKey][]uint32`, a reusable `stamp []uint32`, overflow reset, quality filtering before query/add, aspect pruning before Hamming, and final `(ShaA,ShaB)` sort.

- [x] **Step 4: Run repeated GREEN and race**

```powershell
& $Go test -count=50 ./internal/firstscreen -run 'Test(BandIndex|ScreenImages|AspectClose)'
& $Go test -race -count=1 ./internal/firstscreen
```

- [x] **Step 5: Report and independent review**

---

### Task 3: Video Screening and Exact Duplicate Collection

**Files:**
- Create: `internal/firstscreen/video_screen.go`
- Create: `internal/firstscreen/exact.go`
- Create: `internal/firstscreen/video_exact_test.go`

**Interfaces:**
- Produce `VideoFeature`, `screenVideos`, `FileRef`, `ExactGroup`, and `exactCollector`.

- [x] **Step 1: Write failing boundary tests**

Assert video duration differences `2000ms` pass and `2001ms` fail; Hamming `31` passes and `32` fails; input ties are SHA-stable; quality is recorded but not filtered. Assert exact grouping combines cross-machine/disk/path copies, excludes a singleton, orders members by file ID, and flushes the final group.

- [x] **Step 2: Run RED**

```powershell
& $Go test -count=1 ./internal/firstscreen -run 'Test(ScreenVideos|ExactCollector)'
```

- [x] **Step 3: Implement duration-sorted screening and streaming exact collection**

Normalize pair SHA order independently of duration order. Keep the exact collector streaming: only the current SHA group plus completed groups may be retained.

- [x] **Step 4: Run GREEN and randomized oracle comparison**

```powershell
& $Go test -count=50 ./internal/firstscreen -run 'Test(ScreenVideos|ExactCollector)'
```

- [x] **Step 5: Report and independent review**

---

### Task 4: PostgreSQL Keyset Readers and M3 Indexes

**Files:**
- Create: `internal/firstscreen/store.go`
- Create: `internal/firstscreen/store_integration_test.go`
- Modify: `deploy/central.sql`

**Interfaces:**
- Produce `NewStore(*pgx.Conn, Config) *Store`, `LoadImageFeatures`, `LoadVideoFeatures`, `StreamFilesBySHA`, and `BadRows`.
- Consume central `TEXT` SHA keys and return decoded `[64]byte` keys.

- [x] **Step 1: Write failing PostgreSQL integration tests with page size 3**

Tests must run only when `FS_PG_DSN` is present, use `t.Parallel` only with run-unique rows, run `deploy/central.sql` twice, seed values spanning at least four pages, and assert:

- no duplicate/lost rows;
- canonical text SHA ordering;
- image quality and NULL feature filtering in SQL;
- video duration/PDQ NULL filtering;
- malformed 31-byte PDQ increments `BadRows` and is skipped;
- source row cleanup restores zero test rows.

- [x] **Step 2: Run RED against PostgreSQL 16**

```powershell
$env:FS_PG_DSN='postgres://dedup:dedup@127.0.0.1:5432/dedup?sslmode=disable'
& $Go test -v -count=1 ./internal/firstscreen -run 'TestPGKeyset'
```

- [x] **Step 3: Add idempotent indexes and keyset readers**

Add:

```sql
CREATE INDEX IF NOT EXISTS idx_files_sha512_id
    ON files (sha512, id) WHERE sha512 IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dup_groups_kind ON dup_groups (kind);
CREATE INDEX IF NOT EXISTS idx_dup_members_file ON dup_members (file_id);
```

Use `TEXT` keyset predicates (`sha512 > $1::text`) and `(sha512,id) > ($1::text,$2)`. Validate text keys with `shaFromText`; malformed SHA is a hard data error for `files` and a counted/skipped bad row for feature tables.

- [x] **Step 4: Run GREEN, repeat schema, and inspect query plans**

```powershell
& $Go test -v -count=10 ./internal/firstscreen -run 'TestPGKeyset'
```

Record `EXPLAIN (ANALYZE, BUFFERS)` showing the files scan can use `idx_files_sha512_id`.

- [x] **Step 5: Report and independent review**

---

### Task 5: Transactional Result Replacement

**Files:**
- Modify: `internal/firstscreen/store.go`
- Create: `internal/firstscreen/replace_integration_test.go`

**Interfaces:**
- Produce `ReplaceResults(ctx, exact, pairs) (groupsWritten,membersWritten,skipped int,err error)`.
- Preserve all non-M3 kinds and use one repeatable-read transaction.

- [x] **Step 1: Write failing transactional integration tests**

Seed one row for each M3 kind plus M4 `image`/`video` sentinels. Assert exact representative = minimum file ID; candidate representative = ShaA minimum file ID; all file copies on both SHA sides become members; score JSON is side-correct; missing file side increments `skipped`; rerun does not double counts; definite failures before commit and `pgx.ErrTxCommitRollback` restore old results; an ambiguous non-rollback `Commit` error returns an explicit unknown-outcome error preserving its cause and converges after an idempotent retry; M4 sentinels remain byte-for-byte unchanged.

- [x] **Step 2: Run RED**

```powershell
& $Go test -v -count=1 ./internal/firstscreen -run 'TestPGReplaceResults'
```

- [x] **Step 3: Implement exact-generation-independent, whole-class replacement**

Delete members before groups for only `M3Kinds`; resolve SHA sets in `SHAResolveChunk` chunks; insert groups with `pgx.Batch` and `RETURNING id`; insert members with `CopyFrom`; commit only after every batch succeeds. Use a cancellation-independent bounded rollback context.

- [x] **Step 4: Run GREEN and failure-boundary repetitions**

```powershell
& $Go test -v -count=20 ./internal/firstscreen -run 'TestPGReplaceResults'
```

- [x] **Step 5: Report and independent review**

---

### Task 6: Analyzer Orchestration and Metrics

**Files:**
- Create: `internal/firstscreen/analyzer.go`
- Create: `internal/firstscreen/analyzer_test.go`

**Interfaces:**
- Produce `Analyzer`, `NewAnalyzer`, `Run`, and JSON-serializable `RunStats`.
- Store operations are injected behind a narrow internal interface so unit tests can force every stage failure.

- [x] **Step 1: Write failing orchestration tests**

Assert exact stage order:

```text
exact_group -> image_load -> image_screen -> video_load -> video_screen -> db_write
```

Assert all six `StageElapsedMs` keys exist, partial stats return with the stage-qualified error, write is not called after earlier failure, `BadRows` and `SkippedPairs` propagate, and logger output includes row/pair/write counts.

- [x] **Step 2: Run RED**

```powershell
& $Go test -count=1 ./internal/firstscreen -run 'TestAnalyzer'
```

- [x] **Step 3: Implement the minimal analyzer**

Use monotonic `time.Since`, deterministic pair concatenation, one Store per run, `runtime.ReadMemStats` after `runtime.GC`, and no goroutines inside the algorithm.

- [x] **Step 4: Run repeated GREEN and race**

```powershell
& $Go test -count=20 ./internal/firstscreen -run 'TestAnalyzer'
& $Go test -race -count=1 ./internal/firstscreen
```

- [x] **Step 5: Report and independent review**

---

### Task 7: GUI HTTP Trigger and Composition

**Files:**
- Create: `internal/gui/analysis.go`
- Create: `internal/gui/analysis_test.go`
- Modify: `internal/gui/httpapi.go`
- Modify: `cmd/gui/main.go`
- Modify: `cmd/gui/main_test.go`

**Interfaces:**
- Extend `NewAPI` with an `AnalysisRunner` dependency without breaking nil/test construction.
- Register `POST /api/analysis/firstscreen/run` and `GET /api/analysis/firstscreen/status`.
- A second POST during a run returns `409`; accepted run returns `202`; status exposes `running`, `last`, and `last_err`.

- [x] **Step 1: Write failing HTTP/lifecycle tests**

Use a channel-controlled fake runner. Assert first POST returns 202, concurrent POST 409, request cancellation does not cancel the accepted run, status changes running→finished, error text is retained, and concurrent status requests pass `-race`.

- [x] **Step 2: Run RED**

```powershell
& $Go test -count=1 ./internal/gui ./cmd/gui -run 'Test.*FirstScreen'
```

- [x] **Step 3: Implement handler and main wiring**

Acquire a dedicated PostgreSQL connection for each analysis run from the existing `pgxpool.Pool`, construct `firstscreen.Store` and `Analyzer`, release the connection after run, and bind the runner to the process shutdown context while decoupling it from the HTTP request context.

- [x] **Step 4: Run GREEN and race**

```powershell
& $Go test -count=20 ./internal/gui ./cmd/gui -run 'Test.*FirstScreen'
& $Go test -race -count=1 ./internal/gui ./cmd/gui
```

- [x] **Step 5: Report and independent review**

---

### Task 8: Small End-to-End PostgreSQL Acceptance

**Files:**
- Create: `internal/firstscreen/small_acceptance_test.go`
- Create: `scripts/verify_m3.ps1`

**Interfaces:**
- `scripts/verify_m3.ps1` accepts explicit Go and PG DSN, applies `deploy/central.sql`, runs unit/race/vet and PostgreSQL gates, and fails closed.

- [x] **Step 1: Write the failing 20-row acceptance**

Seed run-unique equivalents of:

- images A1/A2 distance 3 and accepted; A3 quality 30 rejected; A4 aspect rejected; A5 far;
- videos V1/V2 accepted at 1500ms; V2/V3 accepted at 1100ms; V1/V3 rejected at 2600ms; V4 far;
- exact A2×2 and E×3 groups.

Assert exactly 1 image candidate, 2 video candidates, 2 exact groups, 5 groups total, all member counts/representatives/score JSON, page size 3, and identical rerun counts without affecting M4 sentinel kinds.

- [x] **Step 2: Run RED through the verifier**

```powershell
& .\scripts\verify_m3.ps1 -Go $Go -PGDSN $env:FS_PG_DSN
```

- [x] **Step 3: Fix only acceptance-exposed defects**

Every product change requires a narrower failing regression first. The verifier must reject missing Docker PostgreSQL, skipped integration tests, missing stage metrics, and cleanup residuals.

- [x] **Step 4: Run GREEN twice**

```powershell
& .\scripts\verify_m3.ps1 -Go $Go -PGDSN $env:FS_PG_DSN
& .\scripts\verify_m3.ps1 -Go $Go -PGDSN $env:FS_PG_DSN
```

- [x] **Step 5: Report and independent review**

---

### Task 9: Million-Scale Performance Acceptance

**Files:**
- Create: `internal/firstscreen/scale_acceptance_test.go`
- Modify: `scripts/verify_m3.ps1`
- Create: `docs/acceptance/2026-07-28-m3.md`

**Interfaces:**
- Tagged test `TestAcceptanceM3` seeds deterministically when `FS_M3_SEED=1` and otherwise verifies the existing run-scoped dataset.
- Machine-readable evidence records exact counts, per-stage time, total time, peak heap samples, PostgreSQL version, query plans, cleanup, and second-run idempotency.

- [x] **Step 1: Write the deterministic scale test before optimization**

Use seed 1 and exact sizes:

```text
image_features: 1,000,000
video_features:   200,000
files:          1,350,000
image_pairs:       60,000
video_pairs:       15,000
exact_groups:      50,000
groups_written:   125,000
members_written:  300,000
```

Generate/copy rows in bounded chunks; do not retain all PostgreSQL seed rows simultaneously. Sample `runtime.MemStats.HeapInuse` every 50ms.

- [x] **Step 2: Run RED and record the first breached gate**

```powershell
$env:FS_M3_SEED='1'
& $Go test -v -tags m3scale -run '^TestAcceptanceM3$' -timeout 30m ./internal/firstscreen
```

Required gates: exact counts, image screen ≤5s, video screen ≤3s, end-to-end ≤90s, peak heap ≤4GiB, and idempotent second run.

- [x] **Step 3: Optimize only measured bottlenecks**

Keep algorithm semantics unchanged. Allowed changes include preallocation, streaming/chunking, scratch reuse, and PostgreSQL batch sizing. Any algorithmic change must retain naive-oracle unit coverage and deterministic counts.

- [x] **Step 4: Run fresh scale GREEN twice and write evidence**

The second run must not require reseeding and must leave `dup_groups` totals unchanged. Clean only the scale test’s run-unique rows after evidence capture.

- [x] **Step 5: Report and broad independent performance/correctness review**

---

### Task 10: Final M3 Regression, Documentation, and Gate

**Files:**
- Modify: `docs/details/M3-first-screen.md`
- Modify: `docs/todolist.md`
- Modify: `scripts/verify_m3.ps1`
- Create: `.superpowers/sdd/2026-07-28-m3-first-screen/task-10-report.md`

**Interfaces:**
- One command proves M3: `scripts/verify_m3.ps1`.

- [x] **Step 1: Add final fail-closed verifier assertions**

The script must run formatting check, `go vet ./...`, pure-Go full tests, race for `internal/firstscreen` and GUI, PostgreSQL integration with explicit PASS/no SKIP, small acceptance, scale acceptance, schema/index inspection, and cleanup audit. It must print one concise PASS/FAIL line per M3 gate and return nonzero on any missing evidence.

- [x] **Step 2: Run full verifier and observe any remaining failure**

```powershell
& .\scripts\verify_m3.ps1 -Go $Go -PGDSN $env:FS_PG_DSN -RunScale
```

- [x] **Step 3: Fix final defects with narrow RED→GREEN tests**

Do not weaken thresholds or silently skip the local PostgreSQL 16 dependency.

- [x] **Step 4: Dispatch broad independent review**

Review schema compatibility with `TEXT` SHA, candidate determinism, exact-band recall contract, transaction rollback/idempotency, M4 kind preservation, HTTP lifecycle, scale counts, memory/time evidence, and cleanup scope. Fix every Critical/Important finding and re-review.

- [x] **Step 5: Run controller fresh verification and update completion records**

Only after the independent review passes, rerun the complete verifier, mark the M3 detailed checklist and `docs/todolist.md`, then append the SDD ledger with exact commands and evidence paths.
