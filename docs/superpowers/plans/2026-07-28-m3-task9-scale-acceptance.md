# M3 Task 9 Million-Scale Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax
> for tracking. Root explicitly owns independent review; do not dispatch a
> reviewer from this task.

**Goal:** Build a deterministic, bounded-memory, two-process PostgreSQL 16
million-scale acceptance gate for M3, measure before optimizing, and produce
two fresh complete evidence sets.

**Architecture:** A build-tagged Go integration test owns only an explicitly
named `m3_scale_<run>` schema. Streaming `pgx.CopyFromSource` generators seed
the exact 1.35M/1M/200k dataset without materializing row matrices. The normal
verifier remains fast by default; `-Scale` runs seed and reuse processes in one
evidence run, validates both strict JSON markers, and invokes a prefix-checked
test-only cleanup mode on failure.

**Tech Stack:** Go 1.22+, pgx/v5, PostgreSQL 16, PowerShell 7, Docker-hosted
PostgreSQL, JSON evidence.

## Global Constraints

- Do not modify the Task 9 checkbox or progress ledger.
- Do not run an independent second-Windows acceptance; record
  `USER_WAIVED`.
- Use deterministic seed 1 and canonical lowercase TEXT SHA-512 identities.
- Never retain one million `[][]any` feature rows or 1.35 million file rows.
- Only optimize a bottleneck demonstrated by the first scale RED.
- Preserve default Task 8 acceptance, full-repository unit, race, and vet gates.
- Never write the PostgreSQL DSN or password to logs, reports, or evidence.

---

### Task 1: Deterministic bounded dataset generators

**Files:**
- Create: `internal/firstscreen/scale_acceptance_test.go`

**Interfaces:**
- Produces: `scaleCopySource`, implementing `pgx.CopyFromSource`.
- Produces: deterministic image, video, and file generators with exact physical
  row counts.
- Produces: test-only schema validation and cleanup helpers.

- [ ] **Step 1: Write generator arithmetic and uniqueness tests**

Add build-tagged tests that exhaust only generator metadata/counters and assert
literal totals:

```go
if got := scaleExactMemberTotal(); got != 150_000 {
    t.Fatalf("exact members=%d, want 150000", got)
}
if scaleImageRows != 1_000_000 || scaleVideoRows != 200_000 ||
    scaleFileRows != 1_350_000 {
    t.Fatal("scale physical totals changed")
}
```

The exact generator uses `2+i%3` members and adds one member only to group
49,999, correcting the documented 149,999 sum to 150,000.

- [ ] **Step 2: Run generator RED**

```powershell
go test -tags m3scale -run '^TestScaleGenerator' ./internal/firstscreen
```

Expected: compile failure because the streaming generators do not yet exist.

- [ ] **Step 3: Implement bounded streaming sources**

Implement sources whose `Values()` returns one reusable `[]any` row. Image
bands are namespace-separated and unique outside each four-member cluster.
Video rows are generated in duration order; every new non-cluster PDQ is
checked against the bounded 2-second active window, while each cluster's four
members deliberately form exactly `C(4,2)`. Canonical SHAs encode
domain+ordinal directly into 64 bytes, avoiding cross-domain duplicates.

- [ ] **Step 4: Run generator GREEN**

```powershell
go test -tags m3scale -run '^TestScaleGenerator' ./internal/firstscreen
```

Expected: all generator arithmetic, bounded-window, and exact-cluster tests
pass.

### Task 2: Real PostgreSQL scale acceptance and first measurement

**Files:**
- Modify: `internal/firstscreen/scale_acceptance_test.go`

**Interfaces:**
- Consumes: `FS_PG_DSN`, `FS_M3_SEED`, `FS_M3_SCHEMA`,
  `M3_VERIFY_RUN_ID`.
- Produces: one `M3_SCALE_ACCEPTANCE {json}` marker per process.

- [ ] **Step 1: Add real acceptance assertions before optimization**

The seed process snapshots the complete public catalog, creates the validated
run schema, applies `central.sql` twice, streams fixed 50,000-row CopyFrom
chunks, analyzes tables, records PostgreSQL version and
`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`, and runs Analyzer twice.

Both processes assert these literals:

```text
files=1350000 images=1000000 videos=200000 image_loaded=990400
image_pairs=60000 video_pairs=15000 exact_groups=50000
groups=125000 members=300000 skipped=0 bad_rows=0
```

Each Analyzer run samples HeapInuse immediately and every 50ms and enforces
image screen ≤5s, video screen ≤3s, total ≤90s, and peak ≤4GiB.

- [ ] **Step 2: Run the first real scale RED**

```powershell
$env:FS_M3_SEED='1'
go test -v -tags m3scale -run '^TestAcceptanceM3$' -timeout 30m ./internal/firstscreen
```

Record the first breached assertion and all measured stages. Do not optimize
before this output exists.

- [ ] **Step 3: Add a narrow regression only if production changes**

If a measured production bottleneck requires a code change, first add a small
unit benchmark-style regression with a literal semantic oracle, run it RED,
then make one minimal production change and run it GREEN.

### Task 3: Strict optional verifier scale gate

**Files:**
- Modify: `scripts/verify_m3.ps1`
- Create: `scripts/verify_m3_scale_marker.ps1`
- Create: `scripts/test_verify_m3_scale_marker.ps1`

**Interfaces:**
- `verify_m3.ps1 -Scale` runs `scale_seed`, then `scale_reuse`.
- Default invocation does not compile or execute the tagged scale test.

- [ ] **Step 1: Write strict marker negative matrix**

The independent script mutates a valid seed/reuse marker and requires rejection
for missing/null/string integers, non-native booleans, wrong counts, stage
sets, performance bounds, plan fields, schema/run mismatch, reuse reseeding,
and missing final cleanup.

- [ ] **Step 2: Run verifier validator RED**

```powershell
.\scripts\test_verify_m3_scale_marker.ps1
```

Expected: failure because `Assert-M3ScaleMarker` is absent.

- [ ] **Step 3: Implement strict validator and `-Scale` orchestration**

Use the Task 8 native-type helpers. The verifier creates one exact
`m3_scale_<sanitized-run-id>` schema name, passes it to both fresh Go
processes, requires the seed marker to preserve it, requires reuse
`seeded=false`, requires both markers to name the same schema/run, and requires
reuse cleanup residual 0. On failure, cleanup mode receives only the already
validated exact schema name. Scale logs are pre-created and included in final
gate auditing.

- [ ] **Step 4: Run validator GREEN and first full scale verifier**

```powershell
.\scripts\test_verify_m3_scale_marker.ps1
.\scripts\verify_m3.ps1 -Go <go> -PGDSN <dsn> -GCC <gcc> -Scale
```

### Task 4: Measurement-driven optimization and final evidence

**Files:**
- Modify only measured bottleneck files under `internal/firstscreen/`.
- Modify matching narrow unit/oracle tests.

- [ ] **Step 1: Diagnose the first breached gate**

Read the named stage, heap samples, PostgreSQL plans, and scale log. State one
root-cause hypothesis and test one variable. No speculative batching or
algorithm change.

- [ ] **Step 2: Implement the minimal proven fix**

Keep candidate semantics and deterministic exact counts unchanged. Preserve
all existing naive-oracle tests and add a narrow regression for every
production behavior change.

- [ ] **Step 3: Run all narrow and default gates**

```powershell
go test ./internal/firstscreen
.\scripts\test_verify_m3_marker.ps1
.\scripts\test_verify_m3_scale_marker.ps1
.\scripts\verify_m3.ps1 -Go <go> -PGDSN <dsn> -GCC <gcc>
```

- [ ] **Step 4: Run two fresh complete scale GREEN evidence runs**

```powershell
.\scripts\verify_m3.ps1 -Go <go> -PGDSN <dsn> -GCC <gcc> -Scale
.\scripts\verify_m3.ps1 -Go <go> -PGDSN <dsn> -GCC <gcc> -Scale
```

Each run contains seed+reuse, cleanup residual 0, all default gates, and unique
evidence paths.

### Task 5: Documentation and completion audit

**Files:**
- Modify: `docs/details/M3-first-screen.md` (§6.3)
- Create: `docs/acceptance/2026-07-28-m3.md`
- Create: `.superpowers/sdd/2026-07-28-m3-first-screen/task-9-report.md`

- [ ] **Step 1: Correct and document exact-member arithmetic**

Record that `sum(2+i%3, i=0..49999)=149999`; group 49,999 receives one extra
member, preserving the required 50,000 groups / 150,000 exact members.

- [ ] **Step 2: Record RED, measured bottleneck, fixes, and evidence**

Mark all failed/intermediate evidence historical. Record stage/total/heap,
physical and semantic counts, seed time/chunks, PostgreSQL version/plans,
schema cleanup, public snapshot, two authoritative run IDs, and
`second_windows=USER_WAIVED`.

- [ ] **Step 3: Run final fail-closed and secret audits**

Verify default mode omits scale, reuse cannot seed, schema mismatch and malformed
markers fail, failure cleanup cannot target public/other schemas, every log
exists in its evidence directory, environment/cwd restore exactly, and secret
scan returns zero.

- [ ] **Step 4: Hand off to root for independent review**

Do not start a reviewer. Send root the implementation summary, RED/GREEN
measurements, authoritative evidence paths, and residual risks.

## Fix Round 1 Addendum

The broad review added three mandatory acceptance refinements:

1. Public catalog proof spans each whole process. Seed baseline is before
   CREATE and final is after both Analyzers/DB assertions. Reuse baseline is at
   process entry and final is only after DROP succeeds with residual 0.
   Intermediate DDL snapshots never set the marker true.
2. Scale run total/peak are native positive integers; actual EXPLAIN execution
   is a native positive number. Authority-marker mutations cover zero, all
   upper boundaries, exact count, run, schema, and seed/reuse state.
3. Cleanup-only emits exactly one `M3_SCALE_CLEANUP` JSON marker. The verifier
   parses named PASS, exact run/schema, and native residual 0; nonzero residual
   fails, and combined primary/cleanup failures retain both reasons.

Fix Round 1 authority is produced only after the tagged public/cleanup tests,
23-case scale marker matrix, 7-case cleanup marker matrix, and two fresh full
`verify_m3.ps1 -Scale` runs pass. The pre-fix authority is retained but marked
superseded in acceptance and Task 9 reports.
