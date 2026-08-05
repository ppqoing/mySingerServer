# Task 3 — PostgreSQL outage during a real Agent scan

> Archived M1 local acceptance evidence.

## Final status

`DONE`

## Isolation

- Machine id: `m1-pg-outage-local`
- Agent: `D:\code\mySingerServer\bin\agent.exe`, PID `9444`,
  `127.0.0.1:19101`
- GUI: `D:\code\mySingerServer\bin\gui.exe`, PID `22880`,
  `127.0.0.1:18080`
- Task root:
  `D:\code\mySingerServer\.tmp\m1-acceptance-pg-outage`
- Task id: `800b0252-029e-4638-bd9f-73ff37924696`
- PostgreSQL compose service: `deploy-postgres-1`, PostgreSQL 16
- Sync interval: 2 seconds
- Everything disabled so the newly created corpus is deterministically
  enumerated by the real Walker path.

The exact local SQLite observer source is preserved at
`D:\code\mySingerServer\docs\acceptance\evidence\m1-postgres-observer.go.txt`.
It imports the real `dedup/internal/store` package and calls
`PendingSyncCount`, `PendingSyncRows`, and `LoadFilesByIDs`.

## Setup and launch

Prior central state was removed only for the test machine:

```sql
DELETE FROM files WHERE machine_id='m1-pg-outage-local';
DELETE FROM scan_tasks WHERE machine_id='m1-pg-outage-local';
```

A task-owned 3 GiB file was created:

```powershell
fsutil file createnew `
  D:\code\mySingerServer\.tmp\m1-acceptance-pg-outage\corpus\outage-large.bin `
  3221225472
```

Real Agent and GUI processes were launched with their task-owned configs.
The GUI `/api/agents` response before submission was:

```json
[{"machine_id":"m1-pg-outage-local","addr":"127.0.0.1:19101","online":true}]
```

The scan request was:

```json
{
  "machine_id": "m1-pg-outage-local",
  "roots": [
    "D:\\code\\mySingerServer\\.tmp\\m1-acceptance-pg-outage\\corpus"
  ],
  "phase": 1,
  "rescan": true
}
```

Immediately after the API accepted the task:

```powershell
docker compose -f deploy\docker-compose.yml stop postgres
```

## Ordering proof

Docker event history contains:

```text
1785135544293949247|stop
1785135544297431728|die
1785135628041968047|start
1785135633077003216|health_status: healthy
```

The PostgreSQL `die` event converts to
`2026-07-27T06:59:04.297Z` (`14:59:04.297 +08:00`).

The Agent completed the scan later, while PostgreSQL remained stopped:

```json
{"time":"2026-07-27T14:59:06.990604+08:00","level":"INFO",
 "msg":"scan done","task_id":"800b0252-029e-4638-bd9f-73ff37924696",
 "stats":{"Total":1,"Done":1,"Skipped":0,"Failed":0,
          "ScanErrors":0,"ElapsedMS":3083}}
```

Thus PostgreSQL was already down approximately 2.69 seconds before the
scan reached `TaskDone`.

The GUI recorded failed task-status persistence during the outage:

```text
14:59:04.909 ERROR upsert scan_tasks: established connection was aborted
14:59:05.908 ERROR upsert scan_tasks: connection actively refused
14:59:06.908 ERROR upsert scan_tasks: connection actively refused
14:59:06.991 ERROR upsert scan_tasks: connection actively refused
```

Despite those remote failures, `/api/tasks` while
`docker compose ps --status running -q postgres` returned no container id:

```json
{
  "task_id": "800b0252-029e-4638-bd9f-73ff37924696",
  "machine_id": "m1-pg-outage-local",
  "status": "done",
  "done": 1,
  "total": 1,
  "skipped": 0,
  "failed": 0,
  "scan_errors": 0,
  "elapsed_ms": 3083,
  "recent": [{
    "Path": "D:\\code\\mySingerServer\\.tmp\\m1-acceptance-pg-outage\\corpus\\outage-large.bin",
    "SHA512": "bf554a61551d250585ecfe6c82b14a3e26594311f27d433280768d9d4bcd8daddbd180a661100b1dd2437d996696ee333308c19b8a519bbacc49e97b5f101648",
    "Size": 3221225472,
    "Status": "done"
  }]
}
```

## Local durability while PostgreSQL was stopped

The task observer returned:

```json
{
  "pending": 1,
  "queue": [{"RowPK":"1","Generation":1}],
  "files": [{
    "ID": 1,
    "MachineID": "m1-pg-outage-local",
    "DiskNo": 0,
    "Path": "D:\\code\\mySingerServer\\.tmp\\m1-acceptance-pg-outage\\corpus\\outage-large.bin",
    "Size": 3221225472,
    "SHA512": "bf554a61551d250585ecfe6c82b14a3e26594311f27d433280768d9d4bcd8daddbd180a661100b1dd2437d996696ee333308c19b8a519bbacc49e97b5f101648",
    "Phase1Done": true,
    "Status": "done",
    "MissingMask": 0
  }]
}
```

Every two seconds the real Agent syncer logged:

```text
sync: batch failed, retry next round
failed to connect ... dial tcp 127.0.0.1:5432 ... actively refused
rows=1
```

The queue remained at `pending=1`; the scan and local durable result were
not lost.

## Recovery and automatic synchronization

PostgreSQL was restored with:

```powershell
docker compose -f deploy\docker-compose.yml up -d --wait postgres
```

- Restart command timestamp: `2026-07-27T07:00:27.6962033Z`
- Container start event: `2026-07-27T07:00:28.042Z`
- Queue observed drained: `2026-07-27T07:00:35.0926819Z`
- Measured restart-command-to-drain time: `7396 ms`

Local observer after recovery:

```json
{"pending":0,"queue":null,"files":null}
```

Central PostgreSQL query:

```sql
SELECT machine_id,path,size,sha512,status,missing_mask
FROM files
WHERE machine_id='m1-pg-outage-local'
ORDER BY path;
```

Result:

```text
m1-pg-outage-local|
D:\code\mySingerServer\.tmp\m1-acceptance-pg-outage\corpus\outage-large.bin|
3221225472|
bf554a61551d250585ecfe6c82b14a3e26594311f27d433280768d9d4bcd8daddbd180a661100b1dd2437d996696ee333308c19b8a519bbacc49e97b5f101648|
done|0
```

The central path, size, status, missing mask, and SHA-512 exactly match the
locally durable row captured while PostgreSQL was down.

Final compose state before cleanup:

```text
deploy-postgres-1  postgres:16  Up (healthy)  0.0.0.0:5432->5432/tcp
```

## Assessment

The real Agent completed hashing after PostgreSQL had stopped, retained one
generation-aware unsynced SQLite row through repeated connection failures,
and automatically drained it to the central PostgreSQL row after recovery.
The scan path did not depend on central database availability after durable
task dispatch.

## Fix round 1 — isolated cleanup

Before stopping processes, each PID was resolved through `Win32_Process`.
Cleanup refused any executable outside the built M1 binaries or command line
without the task-owned configuration path.

Stopped:

```text
9444|agent.exe|"D:\code\mySingerServer\bin\agent.exe" -config D:\code\mySingerServer\.tmp\m1-acceptance-pg-outage\agent.json
22880|gui.exe|"D:\code\mySingerServer\bin\gui.exe" -config D:\code\mySingerServer\.tmp\m1-acceptance-pg-outage\gui.json
```

Both PIDs were confirmed absent after the stop.

Central cleanup was restricted to the exact test machine:

```sql
DELETE FROM files WHERE machine_id='m1-pg-outage-local';
DELETE FROM scan_tasks WHERE machine_id='m1-pg-outage-local';
SELECT
  (SELECT count(*) FROM files
   WHERE machine_id='m1-pg-outage-local') AS files_left,
  (SELECT count(*) FROM scan_tasks
   WHERE machine_id='m1-pg-outage-local') AS tasks_left;
```

Result:

```text
DELETE 1
DELETE 1
0|0
```

Before recursive removal, the resolved task root was required to equal
`D:\code\mySingerServer\.tmp\m1-acceptance-pg-outage` and to remain below
the resolved workspace root. Cleanup evidence:

```text
REMOVED_TASK_ROOT=D:\code\mySingerServer\.tmp\m1-acceptance-pg-outage
REMOVED_CHILD_COUNT=9
REMOVED_LARGE_FILE_BYTES=3221225472
TASK_ROOT_EXISTS=False
```

PostgreSQL was deliberately left running:

```text
deploy-postgres-1  postgres:16  Up (healthy)  0.0.0.0:5432->5432/tcp
```
