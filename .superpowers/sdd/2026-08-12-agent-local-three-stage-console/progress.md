# SDD ledger — plan: docs/superpowers/plans/2026-08-12-agent-local-three-stage-console.md

Implementation base: cc8ef01522fff94a47444d1bd998798f379561f2
Workspace: D:/code/mySingerServer/.worktrees/portable-dual-package
Branch: codex/portable-dual-package
Pre-flight conflict scan: clean

Baseline: focused `internal/proto` PASS at implementation base.
Baseline: whole-source package run PARTIAL due pre-existing environment boundaries: protected `artifacts/releases`, missing `bin/tools` ffmpeg/ffprobe and videocore stage/DLL, Windows PowerShell 5 execution policy, restricted-token fixture, active Agent pipe listener, and temp-path ACL behavior. No Task 1 package failure.

Task 1: downstream requirement: Task 6 must replace Agent's per-item legacy validation with stage-aware task validation and cover Stage 2/3 image/video masks plus Stage 0 compatibility.
Task 1: complete (commits cc8ef01..766497d, review clean; fresh `go test -count=1 ./internal/proto` PASS)
Task 1: fix round 1/5 (1 addressed, 0 open — restored legacy Stage 0 validation errors; commit 879ca7e)
Task 1: complete after fix (commits cc8ef01..766497d plus 879ca7e, scoped re-review PASS)

Task 2: fix round 1/5 (2 addressed, 0 open — existing-token object/DACL validation and uniform response sanitization; commit 6aeeb1b)
Task 2: verification boundary: real symlink creation skipped for missing privilege; production validation boundary test covers reparse/directory/non-disk fail-closed branches.
Task 2: complete (commits 766497d..670d51c plus 6aeeb1b, review clean; fresh `go test -count=1 ./internal/localcontrol ./internal/agent` PASS)

Task 3: fix round 1/5 (4 addressed, 0 open — production Agent config Socket gateway, protected Windows atomic config write, staged endpoint promotion, shared controller lifecycle; commit 12f6ffc)
Task 3: complete (commits 6aeeb1b..1d6a897 plus 12f6ffc, scoped re-review APPROVED; focused NodeTray, race, static no-Agent-pipe and diff gates PASS)

Task 4: fix round 1/5 (3 addressed, 0 open — monotonic generation publish, composite machine FKs, complete migration/schema proof; commit cd04cb2)
Task 4: complete (commits 12f6ffc..eeddad3 plus cd04cb2, scoped re-review APPROVED; fresh controller Store PASS and race PASS with explicit Machine M5_CC)

Task 5: fix round 1/5 (4 Important + 2 Minor addressed, 0 open — legacy/new contact separation, strict PDQ, unified stage-one state, partial contact publish semantics, production mask source; commit 5709a55)
Task 5: complete (commits cd04cb2..652e442 plus 5709a55, scoped re-review APPROVED; fresh controller agent/worker/store/cmd PASS; wproc initially blocked by system temp ACL then PASS with task-local TEMP/GOTMPDIR)

Task 6: fix round 1/5 (5 addressed, 4 open — native cached Hash did not truly rehash; contact cache committed before final guard; Agent scan logs still exposed paths; split SavePhase2 never reached done; commit ba8558a)
Task 6: fix round 2/5 (3 addressed, 1 open — independent rehash, pre-commit contact guard, split completion fixed; robust mixed-separator and Unicode-safe log redaction still open; commit bc938ce)
Task 6: fix round 3/5 (1 addressed, 0 open — mixed-separator and Unicode-safe Agent path redaction; commit 0a5e6f9)
Task 6: complete (commits 5709a55..9dccf3e plus ba8558a, bc938ce, 0a5e6f9; scoped re-review APPROVED; implementer five-package and agent/worker/store race gates PASS)
Task 6: controller verification PASS (agent/worker/store/cmd fresh; wproc first exited 0xc0000135 because native stage was absent from PATH, then PASS with artifacts/portable-dual-stage-20260812 dependency closure; agent/worker/store race PASS with explicit WinLibs CC)

Task 7: fix round 1/5 (3 addressed, 0 open — PG-equivalent loader eligibility, bounded SQLite feature chunks, effective history/current/machine/rollback/PG characterization tests; commit dfa3028)
Task 7: complete (commits 0a5e6f9..3a785ed plus dfa3028; scoped re-review APPROVED; implementer firstscreen/store/localanalysis full and race gates PASS)
Task 7: controller verification PASS (fresh firstscreen/store/localanalysis full and race gates with explicit WinLibs CC)

Task 8: minor (deferred): legacy video wrapper computes Stage3 for all present frames before merging, rather than preserving old per-frame pHash-pass Sobel short-circuit; final score/frame output remains compatible.
Task 8: fix round 1/5 (3 addressed, 1 open — stage persistence order, full Worker identity and textual verdict fixed; Engine exact video coverage still rejects legitimate partial-frame results; commit a99f346)
Task 8: fix round 2/5 (1 addressed, 0 open — Engine accepts and strictly validates explicit partial-video frame results per real Pool contract; commit 8c5dfe1)
Task 8: complete (commits dfa3028..7310fef plus a99f346, 8c5dfe1; scoped re-review APPROVED; one deferred Minor; implementer phase2/localanalysis/firstscreen/store full and race gates PASS)
Task 8: controller verification PASS (fresh phase2/localanalysis/firstscreen/store full and phase2/localanalysis/store race gates with explicit WinLibs CC)

Task 9: fix round 1/5 (7 addressed in main paths, 4 open — admission-wait cancellation, Cancel/Retry task serialization, complete/published stage2 recovery convergence, invalid-envelope Retry atomicity; commit 14ba57d)
Task 9: fix round 2/5 (4 addressed, 0 open — context-aware admission, per-task cancellable gate, complete/published recovery convergence, pre-transition envelope validation; commit 3c85d24)
Task 9: complete (commits 8c5dfe1..602657e plus 14ba57d, 3c85d24; scoped re-review APPROVED; six-package full, five-package race and repeated concurrency gates PASS)
Task 9: controller verification PASS (fresh proto/store/localtask/localanalysis/agent/cmd full and store/localtask/localanalysis/agent/cmd race gates with explicit WinLibs CC)

Task 10: downstream requirement: Task 14 release builds must pass `-tags nodynamic` so the in-memory WebP implementation always uses its bundled CGo-free fallback and never probes a host `libwebp` DLL.
Task 10: implementation complete pending scoped commit (local current/history query, atomic review+outbox, strict NodeTray socket DTO, Worker-only in-memory JPEG/WebP preview; Go directive 1.23; fixed MIT `github.com/gen2brain/webp v0.6.4`).
Task 10: verification boundary: Windows default/nodynamic full, race and nodynamic Agent/Worker build PASS; Linux/CGO=0 localreview+proto PASS, localpreview remains PARTIAL because the pre-existing Worker pool/supervisor implementation is Windows-only.
Task 10: fix round 1/5 (5 Important + 1 Minor addressed, 0 open — preview crash/metrics isolation, synthesized uncertain-pair groups with transactional review materialization, ImageMemBytes pre-decode budget, preview-only strict decoder, official Worker nodynamic build; README Go 1.23 text deferred to Task 14).
Task 10: fix round 2/5 (1 Important addressed, 0 open — animated WebP content rejected before codec with zero-allocation RIFF inspection; nodynamic source/decode/resize/encode live-set budget includes metadata, WASM and encoded copies; ordinary small images use actual decoded dimensions; README Go 1.23 text remains deferred to Task 14).
