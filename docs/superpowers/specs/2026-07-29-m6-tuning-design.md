# M6 Performance Tuning and Benchmarking Design

## 1. Delivery Goal

M6 adds low-overhead runtime observability, bounded scan backpressure, and a
complete set of reproducible performance tools around the existing Agent,
worker pool, PostgreSQL synchronizer, and first-screen implementation.

This delivery has two acceptance layers:

1. **Automated short-run acceptance:** unit/integration tests plus a read-only,
   time-bounded benchmark over at most 10,000 media files from each of
   `I:\tmp` and `H:\pik\00000000000`, with total benchmark time capped at
   30 minutes.
2. **Manual long-run acceptance:** a generated script for the original
   million-file and 24-hour scenarios. It is not executed automatically.

The two source corpora are on separate physical SSDs. They may be enumerated
and read, but must never be renamed, deleted, have attributes changed, or be
used as generated-corpus destinations. No HDD corpus is available, so the
original HDD utilization threshold remains `NOT_RUN`.

## 2. Constraints and Compatibility

- Keep the current Go module and existing dependencies; do not add a metrics
  service, web UI, or monitoring database.
- Keep JSON as the Agent configuration format.
- Preserve protocol version 1 and the existing numeric values of
  `MsgStatsQuery` and `MsgStatsReport`. New msgpack fields are append-only and
  optional.
- Preserve directory-ordered pending work and existing HDD/SSD stream counts.
- Preserve a shared worker pool and the process-wide `read_chunk_kb` setting.
  The 1 MiB/4 MiB comparison is performed by separate benchmark runs.
- Preserve production sync semantics: five-minute or 50,000-row trigger, with
  no more than 5,000 upsert rows per transaction.
- PostgreSQL credentials are read from configuration, environment, or a
  prompt-owned in-memory value. They are never printed, placed on a process
  command line, or copied into benchmark evidence.
- No administrator elevation or tool installation is part of M6.
- The workspace has no Git repository. Verification records replace commit
  checkpoints.

## 3. Runtime Statistics

### 3.1 Collector

Add `internal/stats` with a concurrency-safe `Collector`. The collector
records cumulative counters and one-second snapshots in a fixed 300-entry
ring:

- process CPU utilization, RSS, Go heap, goroutine count, and Windows handle
  count where available;
- worker ready count, completed/failed files, decode calls, read/decode
  attempts and time, thumbnail counters, single-flight hits, and crashes;
- outstanding scan bytes and per-disk active work, completed bytes, completed
  files, read bytes/second, and logical busy fraction;
- worker respawn count and the latest observed respawn delay;
- read and decode latency percentiles.

Existing `worker.Pool.Metrics()` counters remain authoritative. The collector
samples them; it does not add a competing worker-accounting path.

Latency uses fixed logarithmic buckets, not retained samples or insertion
sorting. Snapshot generation is bounded by the number of buckets and disks,
not by corpus size.

### 3.2 Scan Hooks and Backpressure

`ScanManager` accepts a narrow optional observer:

```go
type ScanObserver interface {
    Begin(diskNo int64, bytes int64)
    End(
        diskNo int64,
        bytes int64,
        elapsed time.Duration,
        read time.Duration,
        decode time.Duration,
    )
}
```

Every media job and non-media hash operation brackets actual processing with
these calls. The same hook updates outstanding bytes and per-disk activity.

A shared weighted byte limiter caps concurrently active scan bytes. The
configured value is clamped to a safe positive range. A file larger than the
limit acquires the full limit so it can still progress. Cancellation releases
all acquired weight. This supplements, rather than replaces, the existing
per-disk stream count.

### 3.3 Configuration

Add this optional JSON section:

```json
{
  "tuning": {
    "stats_enabled": true,
    "stats_interval_s": 1,
    "stats_history_s": 300,
    "pending_bytes_mb": 1024,
    "stats_log_mb": 32,
    "pprof_addr": ""
  }
}
```

Defaults enable local statistics and JSONL output, use one-second sampling,
retain 300 seconds, cap active scan data at 1 GiB, and disable pprof.
`pprof_addr`, when non-empty, must be a loopback address. Invalid values fail
configuration loading before workers start.

The collector writes `data_dir/stats.log` as one JSON object per line using
the repository's existing rolling-file dependency. Log failures are reported
through the Agent logger and never stop media processing.

### 3.4 Protocol and pprof

Extend the existing messages:

```go
type StatsQuery struct {
    WindowSeconds int `msgpack:"window_seconds,omitempty"`
}

type StatsReport struct {
    Disks       []DiskStats `msgpack:"disks,omitempty"`
    CPU         float64     `msgpack:"cpu"`
    Workers     int         `msgpack:"workers"`
    WindowS     int         `msgpack:"window_s,omitempty"`
    RSSBytes    uint64      `msgpack:"rss_bytes,omitempty"`
    HeapBytes   uint64      `msgpack:"heap_bytes,omitempty"`
    Handles     uint64      `msgpack:"handles,omitempty"`
    PendingBytes int64      `msgpack:"pending_bytes,omitempty"`
    FilesDone   int64       `msgpack:"files_done,omitempty"`
    FilesFailed int64       `msgpack:"files_failed,omitempty"`
    Crashes     int64       `msgpack:"crashes,omitempty"`
    ReadP95MS   float64     `msgpack:"read_p95_ms,omitempty"`
    DecodeP95MS float64     `msgpack:"decode_p95_ms,omitempty"`
}
```

`DiskStats` gains optional completed-file and pending-byte fields. A
`StatsProvider` interface is attached to `agent.Server`; requests clamp the
window to 1–300 seconds and return a synchronous snapshot. When no provider
is installed, the server returns its normal protocol error.

pprof is started only when `pprof_addr` is configured. It uses a private
`http.ServeMux`, binds only to loopback, follows Agent cancellation, and a
bind failure is logged without crashing the scan service.

## 4. M6 Tools

All command tools emit a versioned JSON result to stdout or an explicit
output path and return non-zero on invalid input or failed acceptance.

### 4.1 `cmd/benchio`

- Enumerates media extensions under one or more roots in stable path order.
- Supports `-max-files`, `-duration`, `-streams`, `-block-kb`, and `-out`.
- Reads source files sequentially with bounded buffers and computes total
  bytes, elapsed time, throughput, file count, errors, and latency percentiles.
- Opens files read-only and performs no write, rename, attribute, timestamp,
  or delete operation.
- Stops at either the file or duration bound and records which bound ended the
  run.

### 4.2 `cmd/benchsync`

- Uses a PostgreSQL DSN from `M6_PG_DSN`, never a command-line flag.
- Creates a run-unique schema and table, writes deterministic rows in batches,
  measures 1,000/5,000/10,000/50,000 batch sizes, verifies count and distinct
  keys, then drops only its own schema.
- Cleanup is attempted on cancellation and failure.
- Results are advisory; production `upsert_batch=5000` is never changed
  automatically.

### 4.3 `cmd/benchscreen`

- Calls an exported benchmark function in `internal/firstscreen` so it
  exercises the production band-index candidate path.
- Generates deterministic synthetic hashes, known near-duplicate clusters,
  and an expected group count.
- Reports build/query/total duration, peak heap delta, candidates, and
  correctness counts.
- Supports quick and million-scale sizes without PostgreSQL.

### 4.4 `cmd/corpusgen`

- Requires an explicit destination containing an M6 ownership marker.
- A new destination must be absent or empty; an existing destination must
  carry the matching marker.
- Refuses the approved read-only corpora, drive roots, workspace root, UNC
  roots, reparse points, and destinations outside the caller-provided root.
- Produces deterministic small files, duplicate groups, optional sparse
  oversized files, and a manifest with seed and expected totals.
- Cleanup removes only files listed in its own manifest below the marked root.

### 4.5 `cmd/soakrun`

- Starts and tracks only child PIDs it owns; it never searches for or kills
  processes by global executable name.
- Mutations and fault injection are limited to a corpusgen-owned root.
- Captures periodic process/statistics samples, child exits, recovery time,
  and final reconciliation.
- Supports a short smoke duration and a manually selected 24-hour duration.

### 4.6 `cmd/perfreport`

- Merges benchio, benchsync, benchscreen, soak, and log-audit JSON artifacts.
- Emits machine-readable JSON plus a compact Markdown report.
- Uses `PASS`, `FAIL`, and `NOT_RUN`; missing HDD or long-soak evidence can
  never become `PASS`.
- Redacts values matching DSN/password/token key names before output.

### 4.7 PowerShell Drivers

- `scripts/verify_m6.ps1` builds and tests M6, runs bounded quick tool checks,
  and can optionally invoke the two approved read-only SSD benchmarks.
- `scripts/run_m6_short_benchmark.ps1` enforces 10,000 files per source and a
  30-minute aggregate deadline.
- `scripts/run_m6_long_manual.ps1` is the human-invoked corpus, million-scale
  screen, sync, and soak driver. It requires an explicit generated-corpus root
  and confirmation flag.
- `scripts/audit_m6_logs.ps1` validates JSONL structure, monotonic cumulative
  counters, non-negative rates, crash/recovery pairing, and credential
  redaction.
- `scripts/disk_baseline.ps1` optionally invokes a caller-supplied DiskSpd or
  fio executable. It never downloads or installs one and fails clearly when
  the path is absent.

## 5. Verification and Evidence

Implementation follows test-driven development:

- histogram, ring aggregation, redaction, config validation, protocol
  compatibility, StatsQuery dispatch, scan limiter release, deterministic
  corpus generation, guarded cleanup, benchmark bounds, report status logic,
  and child-PID ownership receive focused tests;
- all Go tests and builds run after focused tests;
- PowerShell scripts receive parser checks and bounded smoke checks;
- real-corpus benchmarks are read-only and record source path, selected file
  count, bytes read, errors, throughput, duration, stream count, and block
  size without listing every media filename.

Evidence is written below `.superpowers/evidence/m6-<run-id>/`. The acceptance
report is written to `docs/acceptance/2026-07-29-m6.md`.

The M6 item in `docs/todolist.md` is checked only if the original mandatory
HDD and 24-hour gates are actually executed and pass. This delivery instead
records `M6_TOOLING_READY` when implementation, automated tests, short
double-SSD benchmark, report generation, and the manual driver pass while
long/HDD gates remain `NOT_RUN`.

## 6. Explicit Non-Goals

- No GUI performance dashboard.
- No background telemetry upload.
- No automatic operating-system tuning, power-plan changes, UAC, or package
  installation.
- No mutation of `I:\tmp` or `H:\pik\00000000000`.
- No automatic production configuration rewrite from benchmark results.
- No claim that the short SSD run proves HDD utilization or 24-hour
  stability.
