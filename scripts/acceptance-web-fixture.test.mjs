import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { test } from "node:test";
import {
  createFixtureState,
  deleteStatus,
  groupSummaries,
  resolveStaticPath,
  validateWebBuildRoot,
} from "./acceptance-web-fixture.mjs";

test("rejects drive-qualified and UNC static paths", () => {
  assert.throws(
    () => resolveStaticPath("/C:/Windows/win.ini"),
    /outside|absolute|forbidden/i,
  );
  assert.throws(
    () => resolveStaticPath("/\\\\server\\share\\fixture.html"),
    /outside|absolute|forbidden/i,
  );
});

test("uses disjoint group IDs so list and detail kinds cannot collide", () => {
  const exact = groupSummaries("exact", 1);
  const image = groupSummaries("image", 1);
  const video = groupSummaries("video", 1);

  assert.equal(exact.length, 100);
  assert.equal(image.length, 100);
  assert.equal(video.length, 100);
  assert.ok(exact[0].id < image[0].id);
  assert.ok(image[0].id < video[0].id);
});

test("reports the mode accepted for an isolated deletion task", () => {
  assert.equal(deleteStatus("task-hard", true, "hard").mode, "hard");
  assert.equal(deleteStatus("task-soft", true, "soft").mode, "soft");
});

test("binds one-use confirmation tokens to isolated deletion tasks", () => {
  let now = 1000;
  const state = createFixtureState(() => now);
  const first = state.prepareDelete([4, 3, 2, 2]);
  const second = state.prepareDelete([7]);

  assert.notEqual(first.confirm_token, second.confirm_token);
  assert.throws(
    () => state.executeDelete("unknown-token", "soft"),
    /invalid confirmation/i,
  );

  const accepted = state.executeDelete(first.confirm_token, "hard");
  const pending = state.getDeleteStatus(accepted.task_id);
  assert.equal(pending.mode, "hard");
  assert.equal(pending.total, 3);
  assert.equal(pending.pending, 3);
  assert.equal(pending.by_machine["agent-online"].total, 3);
  assert.throws(
    () => state.executeDelete(first.confirm_token, "soft"),
    /token|used/i,
  );
  assert.throws(
    () => state.getDeleteStatus("unknown-task"),
    /task/i,
  );

  now += 3000;
  const complete = state.getDeleteStatus(accepted.task_id);
  assert.equal(complete.complete, true);
  assert.equal(complete.ok + complete.failed + complete.uncertain, 3);
});

test("classifies invalid, expired, and consumed confirmation tokens like production", () => {
  let now = 1000;
  const state = createFixtureState(() => now);

  assert.throws(
    () => state.executeDelete("unknown-token", "soft"),
    error => error?.statusCode === 400
      && error?.kind === "invalid"
      && error.message === "invalid confirmation",
  );

  const expired = state.prepareDelete([1]);
  now += 60000;
  assert.throws(
    () => state.executeDelete(expired.confirm_token, "soft"),
    error => error?.statusCode === 400
      && error?.kind === "expired"
      && error.message === "invalid confirmation",
  );

  const consumed = state.prepareDelete([2]);
  state.executeDelete(consumed.confirm_token, "soft");
  assert.throws(
    () => state.executeDelete(consumed.confirm_token, "soft"),
    error => error?.statusCode === 409
      && error?.kind === "consumed"
      && error.message === "confirmation already used",
  );
});

test("moves scan and analysis fixtures from running to terminal by elapsed time", () => {
  let now = 1000;
  const state = createFixtureState(() => now);
  state.startScan({
    machine_id: "agent-online",
    roots: ["D:\\媒体"],
    rescan: false,
  });
  state.startAnalysis();
  assert.throws(() => state.startAnalysis(), /running/i);

  assert.equal(state.listTasks()[0].status, "running");
  assert.equal(state.getAnalysisStatus().running, true);

  now += 4000;
  assert.equal(state.listTasks()[0].status, "done");
  assert.equal(state.getAnalysisStatus().running, false);
  assert.equal(state.getAnalysisStatus().last.files_scanned, 1000000);
});

test("uses member_size for member-page offsets", () => {
  const state = createFixtureState(() => 1000);
  const summary = state.listGroups({
    kind: "exact",
    page: 1,
    size: 100,
  }).groups[0];
  const detail = state.getGroup(summary.id, 2, 25);

  assert.equal(detail.member_size, 25);
  assert.equal(
    detail.members[0].file_id,
    detail.representative_file_id + 25,
  );
});

test("keeps group list and detail metadata coherent for every kind", () => {
  const state = createFixtureState(() => 1000);
  for (const kind of ["exact", "image", "video"]) {
    const page = state.listGroups({
      kind,
      page: 1,
      size: 100,
      q: "needle",
      machine: "agent-online",
      min_members: 2,
      sort: "reclaim_desc",
    });
    const summary = page.groups[0];
    const detail = state.getGroup(summary.id, 1, 100);

    assert.equal(page.groups.length, 100);
    assert.equal(summary.kind, kind);
    assert.equal(detail.kind, kind);
    assert.equal(detail.member_total, summary.member_count);
    assert.match(summary.rep_path, /needle/);
  }
});

test("applies high min_members thresholds to both totals and returned groups", () => {
  const state = createFixtureState(() => 1000);
  const first = state.listGroups({
    kind: "exact",
    page: 1,
    size: 100,
    min_members: 1000000,
  });
  const second = state.listGroups({
    kind: "exact",
    page: 2,
    size: 100,
    min_members: 1000000,
  });

  assert.equal(first.total, 4);
  assert.equal(first.groups.length, 4);
  assert.ok(first.groups.every(group => group.member_count >= 1000000));
  assert.equal(second.groups.length, 0);
});

test("keeps machine-filtered list metadata coherent with group details", () => {
  const state = createFixtureState(() => 1000);
  const page = state.listGroups({
    kind: "image",
    page: 1,
    size: 100,
    machine: "agent-offline",
  });
  const summary = page.groups[0];
  const detail = state.getGroup(summary.id, 1, 100);

  assert.ok(summary.machines.includes("agent-offline"));
  assert.ok(detail.members.some(member => member.machine_id === "agent-offline"));
  assert.equal(summary.rep_machine, detail.members[0].machine_id);
  assert.equal(summary.rep_path, detail.members[0].path);
});

test("applies selective path and machine filters instead of all-or-nothing switches", () => {
  const state = createFixtureState(() => 1000);
  const exactId = 42;
  const pathFiltered = state.listGroups({
    kind: "exact",
    page: 1,
    size: 100,
    q: `组-${exactId}`,
  });
  assert.equal(pathFiltered.total, 1);
  assert.deepEqual(pathFiltered.groups.map(group => group.id), [exactId]);

  const online = state.listGroups({
    kind: "exact",
    page: 1,
    size: 100,
    machine: "agent-online",
  });
  const offline = state.listGroups({
    kind: "exact",
    page: 1,
    size: 100,
    machine: "agent-offline",
  });
  assert.notEqual(online.total, offline.total);
  assert.ok(online.groups.every(group => group.machines.includes("agent-online")));
  assert.ok(offline.groups.every(group => group.machines.includes("agent-offline")));
});

test("returns visibly different stable orders for newest and member-count sorts", () => {
  const state = createFixtureState(() => 1000);
  const newest = state.listGroups({
    kind: "exact",
    page: 1,
    size: 4,
    sort: "newest",
  });
  const members = state.listGroups({
    kind: "exact",
    page: 1,
    size: 4,
    sort: "members_desc",
  });

  assert.deepEqual(newest.groups.map(group => group.id), [1, 2, 3, 4]);
  assert.deepEqual(
    members.groups.map(group => group.id),
    [1, 250001, 500001, 750001],
  );
});

test("refuses to start against a legacy or incomplete embedded web directory", async t => {
  const root = await mkdtemp(join(tmpdir(), "mysinger-web-fixture-"));
  t.after(() => rm(root, { force: true, recursive: true }));

  await assert.rejects(
    validateWebBuildRoot(root),
    /missing|React|assets/i,
  );

  await mkdir(join(root, "assets"));
  await writeFile(
    join(root, "index.html"),
    '<div id="root"></div><script type="module" src="/assets/app.js"></script><link rel="stylesheet" href="/assets/app.css">',
  );
  await writeFile(
    join(root, "groups.html"),
    '<div id="root"></div><script type="module" src="/assets/app.js"></script><link rel="stylesheet" href="/assets/app.css">',
  );
  await writeFile(join(root, "legacy.html"), "<!doctype html><title>legacy</title>");
  await writeFile(join(root, "legacy-groups.html"), "<!doctype html><title>legacy groups</title>");
  await writeFile(join(root, "assets", "app.js"), "document.querySelector('#root');");
  await writeFile(join(root, "assets", "app.css"), ":root { color-scheme: light; }");

  await assert.doesNotReject(validateWebBuildRoot(root));

  await writeFile(
    join(root, "assets", "app.css"),
    "body { background: url('/assets/missing.png'); }",
  );
  await assert.rejects(
    validateWebBuildRoot(root),
    /missing|CSS|asset/i,
  );
  await writeFile(join(root, "assets", "missing.png"), "synthetic image");
  await assert.doesNotReject(validateWebBuildRoot(root));

  await writeFile(
    join(root, "assets", "app.css"),
    "body { background: url('https://example.invalid/remote.png'); }",
  );
  await assert.rejects(
    validateWebBuildRoot(root),
    /remote|invalid|CSS|asset/i,
  );

  await writeFile(join(root, "assets", "app.css"), ":root { color-scheme: light; }");
  await writeFile(
    join(root, "index.html"),
    '<div id="root"></div><script type="module" src="/assets/../legacy.html"></script><link rel="stylesheet" href="/assets/app.css">',
  );
  await assert.rejects(
    validateWebBuildRoot(root),
    /assets|outside|invalid/i,
  );
});
