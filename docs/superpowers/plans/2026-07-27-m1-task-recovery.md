# M1 Task Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an M1 scan recover automatically after a TCP reconnect or GUI restart, including the case where the Agent completed while disconnected.

**Architecture:** `TaskAck` gains optional final statistics as a backward-compatible msgpack map field. The GUI persists full resumable scan envelopes in `scan_tasks.target`, restores unfinished tasks at startup, and re-sends them whenever the matching Agent completes Hello. An Agent retaining a completed task returns its final statistics in the idempotent ACK so the restored GUI state can converge to `done`.

**Tech Stack:** Go 1.22+, `vmihailenco/msgpack/v5`, `pgx/v5`, PostgreSQL 16, Go `net` integration tests.

## Global Constraints

- Architecture plan v1.2 is authoritative; protocol evolution may only append named msgpack map fields.
- M1 accepts only phase 1 scan tasks.
- A reconnect must reuse the original `task_id`.
- PostgreSQL `scan_tasks.status` remains one of `sent`, `acked`, `running`, `done`, or `failed`.
- This workspace has no Git repository metadata; each task ends with a test checkpoint instead of a commit.

---

### Task 1: Completed-task ACK carries final statistics

**Files:**
- Modify: `internal/proto/message.go`
- Modify: `internal/agent/scan.go`
- Test: `internal/agent/scan_test.go`
- Test: `internal/proto/conn_test.go`

**Interfaces:**
- Produces: `TaskAck.Stats *TaskStats`
- Consumes: retained `ScanState.Stats`

- [ ] **Step 1: Write the failing Agent test**

After an accepted scan reaches `TaskDone`, send the same `ScanTask` again and assert:

```go
ack := manager.Handle(task, sender)
if ack.Reason != "already_done" || ack.Stats == nil || ack.Stats.Done != 1 {
    t.Fatalf("completed ACK = %#v", ack)
}
```

- [ ] **Step 2: Run the focused test and verify it fails because `TaskAck.Stats` is absent**

Run:

```powershell
go test -count=1 -run TestCompletedScanAckCarriesFinalStats -v ./internal/agent
```

- [ ] **Step 3: Append the protocol field and populate it**

Add the backward-compatible field:

```go
Stats *TaskStats `msgpack:"stats,omitempty"`
```

For `reason="already_done"`, copy `ScanState.Stats` and attach its address to the ACK.

- [ ] **Step 4: Run Agent and protocol tests**

Run:

```powershell
go test -count=1 ./internal/agent ./internal/proto
```

Expected: PASS.

### Task 2: Persist and restore resumable task envelopes

**Files:**
- Modify: `internal/gui/tasks.go`
- Modify: `internal/gui/httpapi.go`
- Test: `internal/gui/tasks_test.go`
- Test: `internal/gui/postgres_integration_test.go`

**Interfaces:**
- Produces: `(*TaskRegistry).Restore(context.Context) error`
- Produces: `(*TaskRegistry).PendingScans(machineID string) []proto.ScanTask`
- Consumes: `TaskInfo.Rescan`, `TaskInfo.Roots`, `TaskInfo.Phase`

- [ ] **Step 1: Write failing registry tests**

Assert that an `already_done` ACK with final stats changes the task to `done` and copies `Total`, `Done`, `Skipped`, `Failed`, and `ElapsedMS`. Assert that `PendingScans("machine-a")` returns only `sent`, `acked`, and `running` tasks and preserves `rescan`.

- [ ] **Step 2: Run the focused tests and verify missing methods/fields fail**

Run:

```powershell
go test -count=1 -run 'TestTaskRegistry(CompletesFromAlreadyDoneAck|ReturnsPendingScanEnvelopes)' -v ./internal/gui
```

- [ ] **Step 3: Implement in-memory state transitions**

Add these JSON-visible fields to `TaskInfo`:

```go
Rescan    bool  `json:"rescan"`
Skipped  int64 `json:"skipped"`
Failed   int64 `json:"failed"`
ElapsedMS int64 `json:"elapsed_ms"`
```

Implement `PendingScans` by cloning only unfinished tasks. Handle an accepted `already_done` ACK with non-nil stats as terminal completion.

- [ ] **Step 4: Write and run the PostgreSQL restore test**

Insert a `running` row whose `target` is:

```json
{"roots":["D:\\media"],"rescan":true}
```

Call `Restore`, then assert the matching `ScanTask` is returned. Run with `DEDUP_TEST_PG_DSN`.

- [ ] **Step 5: Implement durable envelope persistence and restore**

Persist `roots` and `rescan` in `target`, persist ACK/progress state changes, and restore only `sent`, `acked`, and `running` rows. Reject malformed restored targets with a returned error.

- [ ] **Step 6: Run GUI unit and PostgreSQL integration tests**

Run:

```powershell
go test -count=1 ./internal/gui
```

Expected: PASS.

### Task 3: Re-send restored/active tasks after Hello

**Files:**
- Modify: `internal/gui/pool.go`
- Modify: `cmd/gui/main.go`
- Test: `internal/gui/pool_test.go`

**Interfaces:**
- Produces: `(*Pool).SetOnConnect(func(machineID string))`
- Consumes: `TaskRegistry.PendingScans(machineID)`

- [ ] **Step 1: Write a failing real-connection test**

Use the existing TCP listener fixture. After valid Hello, assert the configured connect callback fires once with `machine-a`.

- [ ] **Step 2: Run the focused test and verify the callback API is absent**

Run:

```powershell
go test -count=1 -run TestAgentConnNotifiesPoolAfterValidHello -v ./internal/gui
```

- [ ] **Step 3: Implement the callback at the successful Hello boundary**

Store the callback on `Pool`, invoke it only after `setOnline`, and never invoke it for identity/version failures.

- [ ] **Step 4: Restore before starting the pool and wire automatic resend**

In `cmd/gui/main.go`:

```go
if err := tasks.Restore(ctx); err != nil {
    logger.Error("restore tasks", "err", err)
}
pool.SetOnConnect(func(machineID string) {
    for _, task := range tasks.PendingScans(machineID) {
        if err := pool.Send(machineID, proto.MsgScanTask, &task); err != nil {
            logger.Warn("resume scan", "task_id", task.TaskID, "err", err)
        }
    }
})
```

- [ ] **Step 5: Run GUI tests**

Run:

```powershell
go test -count=1 ./internal/gui ./cmd/gui
```

Expected: PASS.

### Task 4: Recovery verification

**Files:**
- Modify if needed: `internal/gui/web/index.html`
- Verify: all Go packages and Windows binaries

**Interfaces:**
- Consumes: all interfaces from Tasks 1–3.

- [ ] **Step 1: Show failure counters in the task table**

Render `failed` and `skipped` next to progress so a structurally completed task with per-file failures is not presented as error-free.

- [ ] **Step 2: Run complete verification**

Run formatting, `go test -count=1 ./...` with PostgreSQL and Everything integration variables, `go vet ./...`, `go test -race -count=1 ./...`, and `scripts/build.ps1`.

- [ ] **Step 3: Execute local two-Agent black-box recovery**

Start two Agents and one GUI against PostgreSQL, interrupt the GUI during an active scan, restart it with the same database, and verify the restored task is re-sent and converges to `done`.

- [ ] **Step 4: Record the physical acceptance boundary**

Do not mark M1 complete until two distinct Windows machines pass AC-1 through AC-10 in `docs/details/M1-skeleton.md`.
