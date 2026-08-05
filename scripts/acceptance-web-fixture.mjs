import { createReadStream } from "node:fs";
import { readFile, stat } from "node:fs/promises";
import { createServer } from "node:http";
import { extname, isAbsolute, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = fileURLToPath(new URL("../internal/gui/web/", import.meta.url));
const requestedPort = Number.parseInt(process.env.ACCEPTANCE_PORT ?? "4173", 10);
const port = Number.isInteger(requestedPort) && requestedPort > 0
  ? requestedPort
  : 4173;
const groupKindOffsets = {
  exact: 0,
  image: 1000000,
  video: 2000000,
};
const groupsPerKind = 1000000;
const memberCountCycle = 250000;

function fixtureHttpError(statusCode, message, kind) {
  return Object.assign(new Error(message), { kind, statusCode });
}
const fixtureState = createFixtureState();

const agents = [
  {
    machine_id: "agent-online",
    addr: "10.0.0.11:9101",
    online: true,
  },
  {
    machine_id: "agent-offline",
    addr: "10.0.0.12:9101",
    online: false,
    last_err: "验收夹具：Agent 暂时离线",
  },
];

function json(response, status, value) {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    "Content-Length": Buffer.byteLength(body),
    "Content-Type": "application/json; charset=utf-8",
    "X-Acceptance-Fixture": "1",
  });
  response.end(body);
}

function noContent(response, status = 204) {
  response.writeHead(status, { "X-Acceptance-Fixture": "1" });
  response.end();
}

async function readJson(request) {
  const chunks = [];
  for await (const chunk of request) {
    chunks.push(chunk);
  }
  if (chunks.length === 0) return {};
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

function groupKindForId(id) {
  if (id > groupKindOffsets.video && id <= groupKindOffsets.video + 1000000) {
    return "video";
  }
  if (id > groupKindOffsets.image && id <= groupKindOffsets.image + 1000000) {
    return "image";
  }
  if (id > 0 && id <= 1000000) return "exact";
  return undefined;
}

function groupOrdinal(id, kind) {
  return id - groupKindOffsets[kind];
}

function memberCountForGroup(id, kind) {
  const ordinal = groupOrdinal(id, kind);
  return groupsPerKind - ((ordinal - 1) % memberCountCycle);
}

function normalizedMinMembers(value) {
  const parsed = Number.parseInt(String(value ?? 2), 10);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : 2;
}

function representativeMachine(id, kind) {
  return machinesForGroup(id, kind)[0];
}

function machinesForGroup(id, kind) {
  switch (groupOrdinal(id, kind) % 4) {
    case 0:
      return ["agent-offline"];
    case 1:
      return ["agent-online", "agent-offline"];
    default:
      return ["agent-online"];
  }
}

function memberPath(kind, id, fileId, memberIndex) {
  return `D:\\百万文件验收\\${kind}\\needle\\组-${id}\\层级-${String(memberIndex).padStart(6, "0")}\\媒体文件-${fileId}-超长路径用于验证省略与完整信息查看.mp4`;
}

function representativePath(kind, id) {
  return memberPath(kind, id, id * 1000000 + 1, 0);
}

function groupSummaries(kind, page, size = 100, filters = {}) {
  return filteredGroupPage(kind, page, size, filters).groups;
}

function orderedOrdinal(index, sort) {
  if (sort !== "members_desc") return index + 1;
  const cycles = groupsPerKind / memberCountCycle;
  const residue = Math.floor(index / cycles) + 1;
  const cycle = index % cycles;
  return residue + cycle * memberCountCycle;
}

function pathMatcher(kind, query) {
  const normalized = String(query ?? "").trim().toLocaleLowerCase();
  if (normalized === "") return () => true;
  const commonPath = `d:\\百万文件验收\\${kind}\\needle\\`;
  if (commonPath.includes(normalized) || ".mp4".includes(normalized)) {
    return () => true;
  }
  const exactGroup = /组-(\d+)/u.exec(normalized);
  if (exactGroup) {
    const requestedId = Number(exactGroup[1]);
    return id => id === requestedId;
  }
  return id => representativePath(kind, id).toLocaleLowerCase()
    .includes(normalized);
}

function buildGroupSummary(kind, ordinal) {
  const id = groupKindOffsets[kind] + ordinal;
  return {
    id,
    kind,
    member_count: memberCountForGroup(id, kind),
    rep_machine: representativeMachine(id, kind),
    rep_path: representativePath(kind, id),
    machines: machinesForGroup(id, kind),
    created_at: new Date(Date.UTC(2026, 6, 31, 8, 0, 0) - ordinal * 1000)
      .toISOString(),
    total_bytes: 8589934592 + (1000000 - ordinal),
    wasted_bytes: 6442450944 + (1000000 - ordinal),
  };
}

function filteredGroupPage(kind, page, size = 100, filters = {}) {
  const offset = (page - 1) * size;
  const minMembers = normalizedMinMembers(filters.minMembers);
  const requestedMachine = String(filters.machine ?? "").trim();
  const matchesPath = pathMatcher(kind, filters.q);
  const sort = ["members_desc", "newest", "reclaim_desc"].includes(filters.sort)
    ? filters.sort
    : "members_desc";
  const groups = [];
  let total = 0;
  for (let index = 0; index < groupsPerKind; index += 1) {
    const ordinal = orderedOrdinal(index, sort);
    const id = groupKindOffsets[kind] + ordinal;
    if (memberCountForGroup(id, kind) < minMembers) continue;
    const machines = machinesForGroup(id, kind);
    if (requestedMachine !== "" && !machines.includes(requestedMachine)) {
      continue;
    }
    if (!matchesPath(id)) continue;
    if (total >= offset && groups.length < size) {
      groups.push(buildGroupSummary(kind, ordinal));
    }
    total += 1;
  }
  return { groups, total };
}

function score(kind, index) {
  if (kind === "image") {
    return {
      combined: 0.991 - index / 100000,
      edge: 0.982,
      phash: 0.996,
    };
  }
  if (kind === "video") {
    return {
      duration_delta_seconds: 0.12,
      frame_similarity: [0.99, 0.98, 0.97],
    };
  }
  return { sha512_equal: true };
}

function groupMembers(kind, id, memberPage, memberSize = 100) {
  const offset = (memberPage - 1) * memberSize;
  const memberTotal = memberCountForGroup(id, kind);
  const count = Math.max(0, Math.min(memberSize, memberTotal - offset));
  return Array.from({ length: count }, (_, index) => {
    const fileId = id * 1000000 + offset + index + 1;
    const memberIndex = offset + index;
    const groupMachines = machinesForGroup(id, kind);
    const machineId = groupMachines.length === 1
      ? groupMachines[0]
      : memberIndex === 0 || memberIndex % 10 !== 0
        ? "agent-online"
        : "agent-offline";
    return {
      file_id: fileId,
      machine_id: machineId,
      path: memberPath(kind, id, fileId, memberIndex),
      size: 1048576 + index * 4096,
      mtime: 1722384000 + index,
      score_json: score(kind, index),
    };
  });
}

const analysisStats = {
  files_scanned: 1000000,
  exact_groups: 12840,
  exact_members: 39102,
  image_features: 450000,
  image_pairs: 18200,
  video_features: 210000,
  video_pairs: 8400,
  bad_rows: 7,
  skipped_pairs: 152,
  groups_written: 21880,
  members_written: 67420,
  stage_elapsed_ms: {
    exact: 830,
    image_candidates: 2140,
    video_candidates: 1760,
  },
  heap_alloc_bytes: 536870912,
};

const initialScan = {
  task_id: "acceptance-scan-1",
  machine_id: "agent-online",
  phase: 1,
  roots: ["D:\\百万文件验收"],
  rescan: false,
  status: "done",
  done: 1000000,
  total: 1000000,
  skipped: 8,
  failed: 1,
  scan_errors: 1,
  elapsed_ms: 92000,
  speed: 10869,
  recent: [],
  last_seq: 9999,
  updated_at: "2026-07-31T08:30:00Z",
};

function createFixtureState(clock = () => Date.now()) {
  let analysisStartedAt;
  let deleteCounter = 0;
  let scanCounter = 1;
  let tokenCounter = 0;
  const deleteTasks = new Map();
  const preparations = new Map();
  const scans = [];

  function listTasks() {
    const dynamic = scans.map(scan => {
      const elapsed = Math.max(0, clock() - scan.startedAt);
      const complete = elapsed >= 3000;
      return {
        task_id: scan.taskId,
        machine_id: scan.machineId,
        phase: scan.phase,
        roots: [...scan.roots],
        rescan: scan.rescan,
        status: complete ? "done" : "running",
        done: complete ? 1000000 : Math.min(999999, Math.floor(elapsed * 250)),
        total: 1000000,
        skipped: complete ? 8 : 0,
        failed: complete ? 1 : 0,
        scan_errors: complete ? 1 : 0,
        elapsed_ms: elapsed,
        speed: complete ? 10869 : 250000,
        recent: [],
        last_seq: complete ? 9999 : Math.floor(elapsed / 100),
        updated_at: new Date(clock()).toISOString(),
      };
    });
    return [...dynamic, initialScan];
  }

  function startScan(input) {
    const machineId = String(input.machine_id ?? "").trim();
    const roots = Array.isArray(input.roots)
      ? input.roots.map(root => String(root).trim()).filter(Boolean)
      : [];
    if (!machineId || roots.length === 0) {
      throw new Error("scan requires machine_id and roots");
    }
    const taskId = `acceptance-scan-${++scanCounter}`;
    scans.unshift({
      machineId,
      phase: Number.isInteger(input.phase) ? input.phase : 1,
      rescan: Boolean(input.rescan),
      roots,
      startedAt: clock(),
      taskId,
    });
    return { task_id: taskId };
  }

  function startAnalysis() {
    if (
      analysisStartedAt !== undefined
      && clock() - analysisStartedAt < 3000
    ) {
      throw new Error("analysis is already running");
    }
    analysisStartedAt = clock();
  }

  function getAnalysisStatus() {
    if (analysisStartedAt === undefined) {
      return { running: false, last: analysisStats, last_err: "" };
    }
    const running = clock() - analysisStartedAt < 3000;
    return {
      running,
      last: running ? null : analysisStats,
      last_err: "",
    };
  }

  function prepareDelete(memberIds) {
    const normalized = [...new Set(memberIds.map(Number))]
      .filter(Number.isSafeInteger)
      .filter(id => id > 0)
      .sort((left, right) => left - right);
    if (normalized.length === 0) {
      throw new Error("delete preparation requires member IDs");
    }
    const confirmToken = `acceptance-confirm-token-${++tokenCounter}`;
    preparations.set(confirmToken, {
      createdAt: clock(),
      memberIds: normalized,
      used: false,
    });
    return {
      confirm_token: confirmToken,
      expires_in_seconds: 60,
      summary: {
        total_files: normalized.length,
        total_bytes: normalized.length * 1048576,
        by_machine: { "agent-online": normalized.length },
        samples: normalized.slice(0, 3).map(
          id => `D:\\百万文件验收\\待删除\\文件-${id}.mp4`,
        ),
      },
    };
  }

  function executeDelete(confirmToken, mode) {
    const preparation = preparations.get(confirmToken);
    if (!preparation) {
      throw fixtureHttpError(400, "invalid confirmation", "invalid");
    }
    if (preparation.used) {
      throw fixtureHttpError(409, "confirmation already used", "consumed");
    }
    if (clock() - preparation.createdAt >= 60000) {
      throw fixtureHttpError(400, "invalid confirmation", "expired");
    }
    if (mode !== "soft" && mode !== "hard") {
      throw fixtureHttpError(400, "invalid confirmation", "mode");
    }
    preparation.used = true;
    const suffix = String(++deleteCounter).padStart(12, "0");
    const taskId = `11111111-2222-4333-8444-${suffix}`;
    deleteTasks.set(taskId, {
      memberIds: [...preparation.memberIds],
      mode,
      startedAt: clock(),
    });
    return { task_id: taskId };
  }

  function getDeleteStatus(taskId) {
    const task = deleteTasks.get(taskId);
    if (!task) throw new Error("unknown deletion task");
    return deleteStatus(
      taskId,
      clock() - task.startedAt >= 2500,
      task.mode,
      task.memberIds,
    );
  }

  function listGroups(query) {
    const kind = ["exact", "image", "video"].includes(query.kind)
      ? query.kind
      : "exact";
    const page = Math.max(1, Number.parseInt(String(query.page ?? 1), 10));
    const size = Math.max(
      1,
      Math.min(100, Number.parseInt(String(query.size ?? 100), 10)),
    );
    const minMembers = normalizedMinMembers(query.min_members);
    const result = filteredGroupPage(kind, page, size, {
      machine: query.machine,
      minMembers,
      q: query.q,
      sort: query.sort,
    });
    return {
      kind,
      page,
      size,
      total: result.total,
      groups: result.groups,
    };
  }

  function getGroup(id, memberPage, memberSize) {
    const kind = groupKindForId(id);
    if (!kind) throw new Error("unknown group");
    const size = Math.max(1, Math.min(100, Number(memberSize)));
    const page = Math.max(1, Number(memberPage));
    const memberTotal = memberCountForGroup(id, kind);
    return {
      id,
      kind,
      representative_file_id: id * 1000000 + 1,
      member_total: memberTotal,
      member_page: page,
      member_size: size,
      members: groupMembers(kind, id, page, size),
    };
  }

  return {
    executeDelete,
    getAnalysisStatus,
    getDeleteStatus,
    getGroup,
    listGroups,
    listTasks,
    prepareDelete,
    startAnalysis,
    startScan,
  };
}

function deleteStatus(taskId, complete, mode = "soft", memberIds = [1, 2]) {
  const total = memberIds.length;
  const failed = complete && total > 1 ? 1 : 0;
  const ok = complete ? total - failed : 0;
  const pending = complete ? 0 : total;
  const problemMemberId = memberIds.at(-1);
  return {
    task_id: taskId,
    mode,
    total,
    ok,
    failed,
    uncertain: 0,
    pending,
    complete,
    state_sync_failures: 0,
    by_machine: {
      "agent-online": {
        machine_id: "agent-online",
        total,
        ok,
        failed,
        uncertain: 0,
        pending,
        complete,
        state_sync_failures: 0,
        sequences: {
          "0": {
            sequence: 0,
            last_seq: 0,
            received: complete,
            total,
            ok,
            failed,
            uncertain: 0,
          },
        },
      },
    },
    error_codes: failed > 0 ? { E_IN_USE: failed } : {},
    problems: failed > 0
      ? [{
          machine_id: "agent-online",
          sequence: 0,
          path: `D:\\百万文件验收\\无法删除\\文件-${problemMemberId}-这是一个没有空格且很长很长很长很长很长很长很长很长很长很长的媒体文件.mp4`,
          error_code: "E_IN_USE",
          error_message: "验收夹具：文件正在使用",
          uncertain: false,
        }]
      : [],
  };
}

async function handleApi(request, response, url) {
  if (request.method === "GET" && url.pathname === "/api/agents") {
    json(response, 200, agents);
    return true;
  }

  if (request.method === "GET" && url.pathname === "/api/tasks") {
    json(response, 200, fixtureState.listTasks());
    return true;
  }

  if (request.method === "POST" && url.pathname === "/api/scan") {
    try {
      json(response, 202, fixtureState.startScan(await readJson(request)));
    } catch (error) {
      json(response, 400, {
        error: error instanceof Error ? error.message : String(error),
      });
    }
    return true;
  }

  if (
    request.method === "GET"
    && url.pathname === "/api/analysis/firstscreen/status"
  ) {
    json(response, 200, fixtureState.getAnalysisStatus());
    return true;
  }

  if (
    request.method === "POST"
    && url.pathname === "/api/analysis/firstscreen/run"
  ) {
    await readJson(request);
    try {
      fixtureState.startAnalysis();
      json(response, 202, { status: "started" });
    } catch (error) {
      json(response, 409, {
        error: error instanceof Error ? error.message : String(error),
      });
    }
    return true;
  }

  if (request.method === "GET" && url.pathname === "/api/groups") {
    json(response, 200, fixtureState.listGroups({
      kind: url.searchParams.get("kind"),
      machine: url.searchParams.get("machine"),
      min_members: url.searchParams.get("min_members"),
      page: url.searchParams.get("page"),
      q: url.searchParams.get("q"),
      size: url.searchParams.get("size"),
      sort: url.searchParams.get("sort"),
    }));
    return true;
  }

  const groupMatch = url.pathname.match(/^\/api\/groups\/(\d+)$/);
  if (request.method === "GET" && groupMatch) {
    const id = Number.parseInt(groupMatch[1], 10);
    const memberPage = Math.max(
      1,
      Number.parseInt(url.searchParams.get("member_page") ?? "1", 10),
    );
    const memberSize = Math.max(
      1,
      Number.parseInt(url.searchParams.get("member_size") ?? "100", 10),
    );
    try {
      json(response, 200, fixtureState.getGroup(id, memberPage, memberSize));
    } catch (error) {
      json(response, 404, {
        error: error instanceof Error ? error.message : String(error),
      });
    }
    return true;
  }

  if (request.method === "POST" && url.pathname === "/api/delete/prepare") {
    try {
      const body = await readJson(request);
      json(
        response,
        200,
        fixtureState.prepareDelete(
          Array.isArray(body.member_ids) ? body.member_ids : [],
        ),
      );
    } catch (error) {
      json(response, 400, {
        error: error instanceof Error ? error.message : String(error),
      });
    }
    return true;
  }

  if (request.method === "POST" && url.pathname === "/api/delete/execute") {
    try {
      const body = await readJson(request);
      json(
        response,
        202,
        fixtureState.executeDelete(body.confirm_token, body.mode),
      );
    } catch (error) {
      json(response, error?.statusCode ?? 409, {
        error: error instanceof Error ? error.message : String(error),
      });
    }
    return true;
  }

  const deleteMatch = url.pathname.match(/^\/api\/delete\/tasks\/([^/]+)$/);
  if (request.method === "GET" && deleteMatch) {
    const taskId = decodeURIComponent(deleteMatch[1]);
    try {
      json(response, 200, fixtureState.getDeleteStatus(taskId));
    } catch (error) {
      json(response, 404, {
        error: error instanceof Error ? error.message : String(error),
      });
    }
    return true;
  }

  return false;
}

const contentTypes = {
  ".css": "text/css; charset=utf-8",
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".png": "image/png",
  ".svg": "image/svg+xml",
  ".webp": "image/webp",
};

async function serveStatic(response, pathname) {
  let filePath;
  try {
    filePath = resolveStaticPath(pathname);
  } catch {
    json(response, 403, { error: "forbidden" });
    return;
  }

  let fileStat;
  try {
    fileStat = await stat(filePath);
  } catch {
    json(response, 404, { error: "not found" });
    return;
  }
  if (!fileStat.isFile()) {
    json(response, 404, { error: "not found" });
    return;
  }

  response.writeHead(200, {
    "Content-Length": fileStat.size,
    "Content-Type": contentTypes[extname(filePath).toLowerCase()]
      ?? "application/octet-stream",
    "X-Acceptance-Fixture": "1",
  });
  createReadStream(filePath).pipe(response);
}

function resolveStaticPath(pathname) {
  const entries = {
    "/": "index.html",
    "/groups": "groups.html",
  };
  const relativePath = entries[pathname] ?? pathname.replace(/^\/+/, "");
  if (
    relativePath.includes("\0")
    || relativePath.includes("\\")
    || isAbsolute(relativePath)
    || /^[a-z]:/i.test(relativePath)
  ) {
    throw new Error("absolute or forbidden static path");
  }
  const filePath = resolve(webRoot, relativePath);
  const fromRoot = relative(webRoot, filePath);
  if (
    fromRoot === ".."
    || fromRoot.startsWith(`..${sep}`)
    || isAbsolute(fromRoot)
  ) {
    throw new Error("static path resolves outside web root");
  }
  return filePath;
}

function staticReferences(html) {
  const references = [];
  const tagPattern = /<(script|link)\b[^>]*>/giu;
  const attributePattern = /\b([a-z][a-z0-9:_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>]+))/giu;
  for (const tag of html.matchAll(tagPattern)) {
    const attributes = {};
    for (const attribute of tag[0].matchAll(attributePattern)) {
      attributes[attribute[1].toLowerCase()] = attribute[2]
        ?? attribute[3]
        ?? attribute[4];
    }
    if (tag[1].toLowerCase() === "script" && attributes.src) {
      references.push({ kind: "script", url: attributes.src });
      continue;
    }
    const rel = String(attributes.rel ?? "").toLowerCase().split(/\s+/u);
    if (rel.includes("stylesheet") && attributes.href) {
      references.push({ kind: "stylesheet", url: attributes.href });
    }
  }
  return references;
}

function cssAssetReferences(css) {
  const references = [];
  const pattern = /url\(\s*(?:"([^"]*)"|'([^']*)'|([^'")]*))\s*\)/giu;
  for (const match of css.matchAll(pattern)) {
    references.push(String(match[1] ?? match[2] ?? match[3]).trim());
  }
  return references;
}

async function validateStylesheetAssets(rootPath, assetsRoot, cssPath) {
  const css = await readFile(cssPath, "utf8");
  if (/@import\b/iu.test(css)) {
    throw new Error("CSS @import dependencies are forbidden");
  }
  for (const reference of cssAssetReferences(css)) {
    if (reference.startsWith("data:") || reference.startsWith("#")) continue;
    if (
      !reference.startsWith("/assets/")
      || reference.includes("\\")
      || reference.includes("://")
      || reference.startsWith("//")
    ) {
      throw new Error("CSS contains a remote or invalid asset URL");
    }
    const assetPath = resolve(rootPath, reference.replace(/^\/+/, ""));
    const fromAssets = relative(assetsRoot, assetPath);
    if (
      fromAssets === ".."
      || fromAssets.startsWith(`..${sep}`)
      || isAbsolute(fromAssets)
    ) {
      throw new Error("CSS asset resolves outside the assets root");
    }
    let assetStat;
    try {
      assetStat = await stat(assetPath);
    } catch {
      throw new Error("CSS references a missing asset");
    }
    if (!assetStat.isFile() || assetStat.size === 0) {
      throw new Error("CSS references an invalid asset");
    }
  }
}

async function validateWebBuildRoot(root) {
  const rootPath = resolve(root);
  const assetsRoot = resolve(rootPath, "assets");
  for (const name of [
    "index.html",
    "groups.html",
    "legacy.html",
    "legacy-groups.html",
  ]) {
    let fileStat;
    try {
      fileStat = await stat(resolve(rootPath, name));
    } catch {
      throw new Error(`embedded web build is missing ${name}`);
    }
    if (!fileStat.isFile() || fileStat.size === 0) {
      throw new Error(`embedded web build has an invalid ${name}`);
    }
  }

  let assetsStat;
  try {
    assetsStat = await stat(assetsRoot);
  } catch {
    throw new Error("embedded web build is missing assets");
  }
  if (!assetsStat.isDirectory()) {
    throw new Error("embedded web assets path is not a directory");
  }

  for (const name of ["index.html", "groups.html"]) {
    const html = await readFile(resolve(rootPath, name), "utf8");
    if (!/\bid\s*=\s*["']root["']/iu.test(html)) {
      throw new Error(`${name} is not a React entry`);
    }
    const references = staticReferences(html);
    if (
      !references.some(reference => reference.kind === "script")
      || !references.some(reference => reference.kind === "stylesheet")
    ) {
      throw new Error(`${name} is missing its React script or stylesheet`);
    }
    for (const reference of references) {
      if (
        !reference.url.startsWith("/assets/")
        || reference.url.includes("\\")
        || reference.url.includes("://")
      ) {
        throw new Error(`${name} contains an invalid asset URL`);
      }
      const assetPath = resolve(rootPath, reference.url.replace(/^\/+/, ""));
      const fromAssets = relative(assetsRoot, assetPath);
      if (
        fromAssets === ".."
        || fromAssets.startsWith(`..${sep}`)
        || isAbsolute(fromAssets)
      ) {
        throw new Error(`${name} asset resolves outside the assets root`);
      }
      if (
        reference.kind === "script"
          ? extname(assetPath).toLowerCase() !== ".js"
          : extname(assetPath).toLowerCase() !== ".css"
      ) {
        throw new Error(`${name} contains an invalid asset extension`);
      }
      let assetStat;
      try {
        assetStat = await stat(assetPath);
      } catch {
        throw new Error(`${name} references a missing asset`);
      }
      if (!assetStat.isFile() || assetStat.size === 0) {
        throw new Error(`${name} references an invalid asset`);
      }
      if (reference.kind === "stylesheet") {
        await validateStylesheetAssets(rootPath, assetsRoot, assetPath);
      }
    }
  }
}

const server = createServer(async (request, response) => {
  try {
    const url = new URL(request.url ?? "/", "http://127.0.0.1");
    if (await handleApi(request, response, url)) return;
    if (request.method !== "GET" && request.method !== "HEAD") {
      json(response, 405, { error: "method not allowed" });
      return;
    }
    await serveStatic(response, decodeURIComponent(url.pathname));
  } catch (error) {
    json(response, 500, {
      error: error instanceof Error ? error.message : String(error),
    });
  }
});

if (
  process.argv[1]
  && resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    await validateWebBuildRoot(webRoot);
    server.listen(port, "127.0.0.1", () => {
      console.log(
        `Acceptance fixture ready at http://127.0.0.1:${port} (serving ${webRoot})`,
      );
    });
  } catch (error) {
    console.error(
      error instanceof Error ? error.message : String(error),
    );
    process.exitCode = 1;
  }
}

export {
  createFixtureState,
  deleteStatus,
  groupSummaries,
  resolveStaticPath,
  validateWebBuildRoot,
};
