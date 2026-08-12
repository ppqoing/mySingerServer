import { ApiError } from "./client";
import { createAppApi, GUIConfigValidationError, waitForManager } from "./appApi";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" }
  });
}

const groupPage = {
  kind: "image",
  page: 2,
  size: 100,
  total: 1,
  groups: [{
    id: 7,
    kind: "image",
    member_count: 3,
    rep_machine: "agent-a",
    rep_path: "D:\\media\\poster.jpg",
    machines: ["agent-a", "agent-b"],
    created_at: "2026-07-31T00:00:00Z",
    total_bytes: 3000,
    wasted_bytes: 2000
  }]
};

afterEach(() => {
  vi.unstubAllGlobals();
});

test("encodes complete group filters and converts snake_case fields", async () => {
  const fetchMock = vi.fn().mockResolvedValue(jsonResponse(groupPage));
  vi.stubGlobal("fetch", fetchMock);

  const result = await createAppApi().listGroups({
    kind: "image",
    page: 2,
    size: 100,
    q: "poster & cover",
    machine: "agent-a",
    minMembers: 3,
    sort: "reclaim_desc"
  });

  expect(fetchMock).toHaveBeenCalledWith(
    "/api/groups?kind=image&page=2&size=100&q=poster+%26+cover&machine=agent-a&min_members=3&sort=reclaim_desc",
    expect.objectContaining({ method: "GET", body: undefined })
  );
  expect(result.groups[0]).toMatchObject({
    memberCount: 3,
    repMachine: "agent-a",
    repPath: "D:\\media\\poster.jpg",
    createdAt: "2026-07-31T00:00:00Z",
    totalBytes: 3000,
    wastedBytes: 2000
  });
});

test("turns handled error responses and malformed success payloads into ApiError", async () => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(jsonResponse({ error: "central database unavailable" }, 503))
    .mockResolvedValueOnce(new Response("not json", { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const api = createAppApi();

  await expect(api.listAgents()).rejects.toMatchObject({
    name: "ApiError",
    status: 503,
    message: "central database unavailable",
    retryable: true
  });
  await expect(api.listAgents()).rejects.toMatchObject({
    name: "ApiError",
    status: 200,
    retryable: false
  });
});

test("preserves structured analysis status from its documented 503 response", async () => {
  const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
    running: false,
    last: {
      files_scanned: 10,
      exact_groups: 2,
      exact_members: 4,
      image_features: 3,
      image_pairs: 1,
      video_features: 2,
      video_pairs: 1,
      bad_rows: 0,
      skipped_pairs: 0,
      groups_written: 3,
      members_written: 6,
      stage_elapsed_ms: { exact: 12 },
      heap_alloc_bytes: 2048
    },
    last_err: "firstscreen analysis unavailable"
  }, 503));
  vi.stubGlobal("fetch", fetchMock);

  await expect(createAppApi().getAnalysisStatus()).resolves.toMatchObject({
    running: false,
    last: {
      filesScanned: 10,
      stageElapsedMs: { exact: 12 },
      heapAllocBytes: 2048
    },
    lastErr: "firstscreen analysis unavailable"
  });
  expect(fetchMock.mock.calls[0]?.[1]).not.toHaveProperty("decodeStatuses");
});

test("rejects 204 for JSON endpoints but accepts it only for analysis start", async () => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(new Response(null, { status: 204 }))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetchMock);
  const api = createAppApi();

  await expect(api.listTasks()).rejects.toBeInstanceOf(ApiError);
  await expect(api.runAnalysis()).resolves.toBeUndefined();
});

test("preserves native abort errors without wrapping them", async () => {
  const abort = new DOMException("The operation was aborted", "AbortError");
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(abort));

  await expect(createAppApi().listAgents()).rejects.toBe(abort);
});

test("decodes pending Agent identity state without inferring from errors", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse([{
    machine_id: "",
    addr: "192.168.1.10:9101",
    online: false,
    identity_state: "pending"
  }])));

  await expect(createAppApi().listAgents()).resolves.toEqual([{
    machineId: "",
    addr: "192.168.1.10:9101",
    online: false,
    identityState: "pending"
  }]);
});

test("decodes a Go nil task recent slice as empty", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse([{
      task_id: "task-1",
      machine_id: "agent-a",
      phase: 1,
      roots: ["D:\\media"],
      rescan: false,
      status: "sent",
      done: 0,
      total: -1,
      skipped: 0,
      failed: 0,
      scan_errors: 0,
      elapsed_ms: 0,
      speed: 0,
      recent: null,
      updated_at: "2026-07-31T00:00:00Z"
    }])));

  await expect(createAppApi().listTasks()).resolves.toMatchObject([{ recent: [] }]);
});

test("decodes a Go nil delete problems slice as empty", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({
      task_id: "11111111-1111-4111-8111-111111111111",
      mode: "soft",
      total: 0,
      ok: 0,
      failed: 0,
      uncertain: 0,
      pending: 0,
      complete: true,
      state_sync_failures: 0,
      by_machine: {},
      error_codes: {},
      problems: null
    })));

  await expect(createAppApi().getDeleteStatus("11111111-1111-4111-8111-111111111111"))
    .resolves.toMatchObject({ problems: [] });
});

test("continues rejecting null for required arrays", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({
    ...groupPage,
    groups: null
  })));

  await expect(createAppApi().listGroups({ kind: "image", page: 2, size: 100 }))
    .rejects.toBeInstanceOf(ApiError);
});

test("sorts and deduplicates member IDs before delete preparation", async () => {
  const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
    confirm_token: "confirm-1",
    expires_in_seconds: 60,
    summary: {
      total_files: 2,
      total_bytes: 123,
      by_machine: { "agent-a": 2 },
      samples: ["D:\\media\\a.jpg"]
    }
  }));
  vi.stubGlobal("fetch", fetchMock);

  await createAppApi().prepareDelete([9, 2, 9, 5]);

  expect(fetchMock).toHaveBeenCalledWith("/api/delete/prepare", expect.objectContaining({
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{\"member_ids\":[2,5,9]}"
  }));
  expect(() => createAppApi().prepareDelete([])).toThrow("at least one");
});

test("sends the exact confirmation token and mode when executing deletion", async () => {
  const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ task_id: "task-7" }, 202));
  vi.stubGlobal("fetch", fetchMock);

  await expect(createAppApi().executeDelete("confirm-7", "hard")).resolves.toEqual({ taskId: "task-7" });

  expect(fetchMock).toHaveBeenCalledWith("/api/delete/execute", expect.objectContaining({
    method: "POST",
    body: "{\"confirm_token\":\"confirm-7\",\"mode\":\"hard\"}"
  }));
});

const guiConfigResponse = {
  config: {
    listen_addr: "127.0.0.1:18080",
    pg_dsn: "postgres://user:pass@127.0.0.1:5432/dedup",
    agents: [
      { addr: "192.168.1.10:9101" },
      { addr: "192.168.1.11:9101" }
    ],
    heartbeat_s: 15,
    firstscreen: {
      hamming_max: 31,
      aspect_tolerance: 0.1,
      video_duration_window_ms: 2000,
      image_quality_min: 50,
      read_page_size: 50000,
      group_insert_batch: 1000,
      sha_resolve_chunk: 10000
    },
    phase2: {
      phash_pass_t2: 0.8,
      phash_part_threshold: 10,
      sobel_t3: 0.85,
      video_frames: 6,
      video_avg_t4: 0.8,
      video_min_passed: 4,
      video_min_valid: 4,
      video_file_timeout_s: 120,
      video_frame_command_timeout_s: 20,
      image_file_timeout_s: 30,
      task_shard_size: 5000,
      auto_dispatch: true
    }
  },
  restart_required: true
};

test("loads and decodes the complete GUI configuration", async () => {
  const fetchMock = vi.fn().mockResolvedValue(jsonResponse(guiConfigResponse));
  vi.stubGlobal("fetch", fetchMock);

  await expect(createAppApi().loadGUIConfig()).resolves.toEqual({
    config: {
      listenAddr: "127.0.0.1:18080",
      pgDsn: "postgres://user:pass@127.0.0.1:5432/dedup",
      agents: [
        { addr: "192.168.1.10:9101" },
        { addr: "192.168.1.11:9101" }
      ],
      heartbeatS: 15,
      firstScreen: {
        hammingMax: 31,
        aspectTolerance: 0.1,
        videoDurationWindowMs: 2000,
        imageQualityMin: 50,
        readPageSize: 50000,
        groupInsertBatch: 1000,
        shaResolveChunk: 10000
      },
      phase2: {
        phashPassT2: 0.8,
        phashPartThreshold: 10,
        sobelT3: 0.85,
        videoFrames: 6,
        videoAvgT4: 0.8,
        videoMinPassed: 4,
        videoMinValid: 4,
        videoFileTimeoutS: 120,
        videoFrameCommandTimeoutS: 20,
        imageFileTimeoutS: 30,
        taskShardSize: 5000,
        autoDispatch: true
      }
    },
    restartRequired: true
  });
  expect(fetchMock).toHaveBeenCalledWith("/api/config", expect.objectContaining({ method: "GET" }));
});

test("encodes the complete GUI configuration with snake_case fields", async () => {
  const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
    saved: true, restart_required: true, restarting: false, recovery_url: ""
  }));
  vi.stubGlobal("fetch", fetchMock);
  const guiConfig = {
    listenAddr: "127.0.0.1:18080",
    pgDsn: "postgres://user:pass@127.0.0.1:5432/dedup",
    agents: [
      { addr: "192.168.1.10:9101" },
      { addr: "192.168.1.11:9101" }
    ],
    heartbeatS: 15,
    firstScreen: {
      hammingMax: 31,
      aspectTolerance: 0.1,
      videoDurationWindowMs: 2000,
      imageQualityMin: 50,
      readPageSize: 50000,
      groupInsertBatch: 1000,
      shaResolveChunk: 10000
    },
    phase2: {
      phashPassT2: 0.8,
      phashPartThreshold: 10,
      sobelT3: 0.85,
      videoFrames: 6,
      videoAvgT4: 0.8,
      videoMinPassed: 4,
      videoMinValid: 4,
      videoFileTimeoutS: 120,
      videoFrameCommandTimeoutS: 20,
      imageFileTimeoutS: 30,
      taskShardSize: 5000,
      autoDispatch: true
    }
  };

  await expect(createAppApi().saveGUIConfig(guiConfig)).resolves.toEqual({
    saved: true,
    restartRequired: true,
    restarting: false,
    recoveryURL: ""
  });
  expect(fetchMock).toHaveBeenCalledWith("/api/config", expect.objectContaining({
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(guiConfigResponse.config)
  }));
});

test("decodes automatic restart recovery details from a saved GUI configuration", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({
    saved: true,
    restart_required: true,
    restarting: true,
    recovery_url: "http://127.0.0.1:28081/api/restart/health"
  })));

  await expect(createAppApi().saveGUIConfig({
    listenAddr: "127.0.0.1:18080",
    pgDsn: "postgres://user:pass@127.0.0.1:5432/dedup",
    agents: [{ addr: "192.168.1.10:9101" }],
    heartbeatS: 15,
    firstScreen: {
      hammingMax: 31, aspectTolerance: 0.1, videoDurationWindowMs: 2000,
      imageQualityMin: 50, readPageSize: 50000, groupInsertBatch: 1000, shaResolveChunk: 10000
    },
    phase2: {
      phashPassT2: 0.8, phashPartThreshold: 10, sobelT3: 0.85, videoFrames: 6,
      videoAvgT4: 0.8, videoMinPassed: 4, videoMinValid: 4, videoFileTimeoutS: 120,
      videoFrameCommandTimeoutS: 20, imageFileTimeoutS: 30, taskShardSize: 5000, autoDispatch: true
    }
  })).resolves.toMatchObject({
    saved: true,
    restartRequired: true,
    restarting: true,
    recoveryURL: "http://127.0.0.1:28081/api/restart/health"
  });
});

test.each([
  ["connecting", "", [{ machine_id: "", addr: "10.0.0.1:9101", online: false, identity_state: "pending" }]],
  ["connected", "", [{ machine_id: "node-a", addr: "10.0.0.1:9101", online: true, identity_state: "claimed" }]],
  ["error", "database_unavailable", [{ machine_id: "", addr: "10.0.0.1:9101", online: false, identity_state: "pending" }]]
])("decodes runtime status %s", async (databaseState, databaseErrorCode, agents) => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({
    database_state: databaseState,
    database_error_code: databaseErrorCode,
    agents,
    restarting: false,
    recovery_url: ""
  })));

  await expect(createAppApi().getRuntimeStatus()).resolves.toMatchObject({
    databaseState,
    databaseErrorCode,
    restarting: false,
    recoveryURL: ""
  });
});

test("waits past the old Manager health response until restarting is false", async () => {
  vi.useFakeTimers();
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(jsonResponse({ ok: true, restarting: true }))
    .mockResolvedValueOnce(jsonResponse({ ok: true, restarting: false }));
  vi.stubGlobal("fetch", fetchMock);
  const waiting = waitForManager("http://127.0.0.1:28081/api/restart/health");

  await vi.advanceTimersByTimeAsync(250);
  await expect(waiting).resolves.toBeUndefined();
  expect(fetchMock).toHaveBeenCalledTimes(2);
  vi.useRealTimers();
});

test("aborts a hung recovery health fetch at the global deadline", async () => {
  vi.useFakeTimers();
  let healthSignal: AbortSignal | undefined;
  vi.stubGlobal("fetch", vi.fn().mockImplementation((_url, options: RequestInit) => new Promise((_resolve, reject) => {
    healthSignal = options.signal ?? undefined;
    healthSignal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
  })));
  const waiting = waitForManager("http://127.0.0.1:28081/api/restart/health");
  const rejected = expect(waiting).rejects.toThrow("Manager restart timed out");

  await Promise.resolve();
  await vi.advanceTimersByTimeAsync(30_000);
  expect(healthSignal?.aborted).toBe(true);
  await rejected;
  vi.useRealTimers();
});

test("rejects a GUI save response missing recovery fields", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({ saved: true, restart_required: true })));

  await expect(createAppApi().saveGUIConfig({
    listenAddr: "127.0.0.1:18080", pgDsn: "postgres://user:pass@127.0.0.1:5432/dedup",
    agents: [{ addr: "192.168.1.10:9101" }], heartbeatS: 15,
    firstScreen: { hammingMax: 31, aspectTolerance: 0.1, videoDurationWindowMs: 2000, imageQualityMin: 50, readPageSize: 50000, groupInsertBatch: 1000, shaResolveChunk: 10000 },
    phase2: { phashPassT2: 0.8, phashPartThreshold: 10, sobelT3: 0.85, videoFrames: 6, videoAvgT4: 0.8, videoMinPassed: 4, videoMinValid: 4, videoFileTimeoutS: 120, videoFrameCommandTimeoutS: 20, imageFileTimeoutS: 30, taskShardSize: 5000, autoDispatch: true }
  })).rejects.toBeInstanceOf(ApiError);
});

test("preserves a saved configuration when automatic restart launch fails", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({
    error: "restart_launch_failed", saved: true, restart_required: true, restarting: false, recovery_url: ""
  }, 500)));

  await expect(createAppApi().saveGUIConfig({
    listenAddr: "127.0.0.1:18080", pgDsn: "postgres://user:pass@127.0.0.1:5432/dedup", agents: [{ addr: "192.168.1.10:9101" }], heartbeatS: 15,
    firstScreen: { hammingMax: 31, aspectTolerance: 0.1, videoDurationWindowMs: 2000, imageQualityMin: 50, readPageSize: 50000, groupInsertBatch: 1000, shaResolveChunk: 10000 },
    phase2: { phashPassT2: 0.8, phashPartThreshold: 10, sobelT3: 0.85, videoFrames: 6, videoAvgT4: 0.8, videoMinPassed: 4, videoMinValid: 4, videoFileTimeoutS: 120, videoFrameCommandTimeoutS: 20, imageFileTimeoutS: 30, taskShardSize: 5000, autoDispatch: true }
  })).rejects.toMatchObject({ name: "GUIConfigRestartError", saved: true, restartRequired: true });
});

test("preserves structured GUI configuration field errors", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({
    error: "config_invalid",
    fields: [{
      field: "agents[1].addr",
      code: "duplicate",
      message: "Agent 地址不能重复"
    }]
  }, 400)));

  await expect(createAppApi().saveGUIConfig({
    listenAddr: "127.0.0.1:18080",
    pgDsn: "postgres://user:pass@127.0.0.1:5432/dedup",
    agents: [{ addr: "192.168.1.10:9101" }],
    heartbeatS: 15,
    firstScreen: {
      hammingMax: 31,
      aspectTolerance: 0.1,
      videoDurationWindowMs: 2000,
      imageQualityMin: 50,
      readPageSize: 50000,
      groupInsertBatch: 1000,
      shaResolveChunk: 10000
    },
    phase2: {
      phashPassT2: 0.8,
      phashPartThreshold: 10,
      sobelT3: 0.85,
      videoFrames: 6,
      videoAvgT4: 0.8,
      videoMinPassed: 4,
      videoMinValid: 4,
      videoFileTimeoutS: 120,
      videoFrameCommandTimeoutS: 20,
      imageFileTimeoutS: 30,
      taskShardSize: 5000,
      autoDispatch: true
    }
  })).rejects.toMatchObject({
    name: "GUIConfigValidationError",
    status: 400,
    fields: [{ field: "agents[1].addr", code: "duplicate", message: "Agent 地址不能重复" }]
  });
  expect(GUIConfigValidationError.prototype).toBeInstanceOf(ApiError);
});

test("rejects malformed GUI configuration responses", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({
    ...guiConfigResponse,
    config: { ...guiConfigResponse.config, agents: null }
  })));

  await expect(createAppApi().loadGUIConfig()).rejects.toBeInstanceOf(ApiError);
});
