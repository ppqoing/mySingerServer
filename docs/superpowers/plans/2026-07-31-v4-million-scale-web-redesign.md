# V4 Million-Scale Web Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the embedded legacy HTML pages with a V4 glass-style React operations console that uses real Agent, scan, analysis, group, and deletion APIs and remains usable with million-file datasets.

**Architecture:** Keep React source in an isolated `webui/` Vite project and emit two static entry points into `internal/gui/web/` for Go embedding. Extend `/api/groups` with backward-compatible server filters, aggregate sizes, sorting, and optional member pagination; keep all components behind typed API adapters. Use server pagination plus DOM virtualization, explicit selection semantics, and the existing two-step delete contract.

**Tech Stack:** Node 22.15+, React 19.2.8, React Router 7.18.2, TypeScript 5.9.3, Vite 8.2.0, TanStack React Virtual 3.14.9, Vitest 4.1.10, Testing Library 16.3.2, jsdom 29.0.0, Go 1.22+ embedded `fs.FS`.

## Global Constraints

- Production assets must be local; no CDN, remote font, remote image, or runtime asset dependency.
- Use React `HashRouter`; `/` starts at `#/overview`, while `/groups` starts at `#/groups`.
- Preserve the original pages as `/legacy.html` and `/legacy-groups.html`.
- The group list requests exactly 100 rows per page and virtualizes visible rows.
- Group member requests use `member_page=1..N` and `member_size=100`; virtualize the current member page.
- Header select-all affects only the loaded page; filter changes clear selection.
- Representative and offline files cannot be selected for deletion.
- Soft delete is the default; prepare and execute remain separate requests.
- Test/development fixtures never produce fake success in production builds.
- Generated artwork has a CSS-gradient fallback and must not reduce text contrast below WCAG AA.
- The current workspace has no `.git` metadata. Replace commit steps with a checkpoint note listing touched files and passing commands; do not initialize a repository.

---

## File Map

### Frontend project

- Create `webui/package.json` — pinned scripts and dependencies.
- Create `webui/package-lock.json` — npm reproducibility lock.
- Create `webui/tsconfig.json` — strict browser TypeScript.
- Create `webui/vite.config.ts` — two HTML inputs and build output.
- Create `webui/eslint.config.js` — flat ESLint configuration.
- Create `webui/index.html` — root entry.
- Create `webui/groups.html` — compatibility entry that selects `#/groups`.
- Create `webui/public/legacy.html` — preserved current root page.
- Create `webui/public/legacy-groups.html` — preserved current groups page.
- Create `webui/src/main.tsx` — React bootstrap.
- Create `webui/src/app/App.tsx` — router and application composition.
- Create `webui/src/app/navigation.ts` — navigation metadata.
- Create `webui/src/api/contracts.ts` — frontend domain contracts.
- Create `webui/src/api/client.ts` — abortable JSON client and typed errors.
- Create `webui/src/api/appApi.ts` — endpoint adapter.
- Create `webui/src/api/appApi.test.ts` — endpoint contract tests.
- Create `webui/src/hooks/usePolling.ts` — focus-aware polling.
- Create `webui/src/hooks/usePagedGroups.ts` — group query state/cache.
- Create `webui/src/hooks/useSelection.ts` — explicit member selection.
- Create `webui/src/hooks/hooks.test.tsx` — hook tests.
- Create `webui/src/components/AppShell.tsx` — V4 navigation and responsive shell.
- Create `webui/src/components/AsyncState.tsx` — loading/empty/error states.
- Create `webui/src/components/Modal.tsx` — accessible focus-trapped dialog.
- Create `webui/src/components/VirtualTable.tsx` — reusable virtualized table surface.
- Create `webui/src/features/overview/OverviewPage.tsx`.
- Create `webui/src/features/agents/AgentsPage.tsx`.
- Create `webui/src/features/scans/ScansPage.tsx`.
- Create `webui/src/features/analysis/AnalysisPage.tsx`.
- Create `webui/src/features/groups/GroupsPage.tsx`.
- Create `webui/src/features/groups/GroupFilters.tsx`.
- Create `webui/src/features/groups/GroupTable.tsx`.
- Create `webui/src/features/groups/GroupDetail.tsx`.
- Create `webui/src/features/deletion/DeleteDialog.tsx`.
- Create `webui/src/features/deletion/DeleteStatusPanel.tsx`.
- Create focused `*.test.tsx` files beside each feature.
- Create `webui/src/styles/tokens.css`.
- Create `webui/src/styles/global.css`.
- Create `webui/src/assets/aurora-surface.png`.
- Create `webui/src/assets/media-placeholder.png`.
- Create `webui/src/test/setup.ts`.

### Go and build integration

- Modify `internal/gui/groups.go` — backward-compatible scalable group query.
- Modify `internal/gui/groups_test.go` — query and embedded-page assertions.
- Modify `internal/gui/httpapi_test.go` — React entry/static-asset smoke tests.
- Delete `internal/gui/delete_web_test.go` after equivalent React deletion tests pass.
- Replace generated `internal/gui/web/index.html`.
- Replace generated `internal/gui/web/groups.html`.
- Create generated `internal/gui/web/legacy.html`.
- Create generated `internal/gui/web/legacy-groups.html`.
- Create generated `internal/gui/web/assets/*`.
- Create `scripts/build-web.ps1`.
- Modify `scripts/build.ps1` — invoke web build before `gui.exe`.
- Modify `.gitignore` — ignore `webui/node_modules`, `webui/dist`, and `.superpowers`.

---

### Task 1: Establish the isolated React build and test baseline

**Files:**
- Create: `webui/package.json`
- Create: `webui/tsconfig.json`
- Create: `webui/vite.config.ts`
- Create: `webui/eslint.config.js`
- Create: `webui/index.html`
- Create: `webui/groups.html`
- Create: `webui/src/main.tsx`
- Create: `webui/src/app/App.tsx`
- Create: `webui/src/app/App.test.tsx`
- Create: `webui/src/test/setup.ts`

**Interfaces:**
- Produces: `App(): JSX.Element`.
- Produces: `npm run test`, `npm run lint`, and `npm run build`.
- Consumes: no application code.

- [ ] **Step 1: Create the pinned package manifest**

```json
{
  "name": "mysinger-webui",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "test": "vitest run",
    "test:watch": "vitest",
    "lint": "eslint .",
    "build": "tsc --noEmit && vite build"
  },
  "dependencies": {
    "@tanstack/react-virtual": "3.14.9",
    "react": "19.2.8",
    "react-dom": "19.2.8",
    "react-router-dom": "7.18.2"
  },
  "devDependencies": {
    "@eslint/js": "10.0.1",
    "@testing-library/jest-dom": "7.0.0",
    "@testing-library/react": "16.3.2",
    "@testing-library/user-event": "14.6.1",
    "@types/react": "19.2.18",
    "@types/react-dom": "19.2.4",
    "@vitejs/plugin-react": "6.0.5",
    "eslint": "10.8.0",
    "eslint-plugin-react-hooks": "7.1.1",
    "eslint-plugin-react-refresh": "0.5.3",
    "globals": "17.8.0",
    "jsdom": "29.0.0",
    "typescript": "5.9.3",
    "typescript-eslint": "8.65.0",
    "vite": "8.2.0",
    "vitest": "4.1.10"
  }
}
```

- [ ] **Step 2: Install dependencies and create `package-lock.json`**

Run: `npm install --prefix webui`

Expected: exit 0, `webui/package-lock.json` exists, and npm reports no unresolved peer dependency.

- [ ] **Step 3: Write the failing application smoke test**

```tsx
import { render, screen } from "@testing-library/react";
import { App } from "./App";

test("renders the six operational workspaces", () => {
  render(<App />);
  for (const label of ["总览", "Agent", "扫描任务", "一筛分析", "重复组", "删除审计"]) {
    expect(screen.getByRole("link", { name: label })).toBeInTheDocument();
  }
});
```

- [ ] **Step 4: Run the smoke test and verify RED**

Run: `npm test --prefix webui -- --run src/app/App.test.tsx`

Expected: FAIL because `App` and test setup do not exist.

- [ ] **Step 5: Add strict config, two Vite HTML inputs, test setup, and minimal app**

`vite.config.ts` must use:

```ts
build: {
  outDir: "../internal/gui/web",
  emptyOutDir: true,
  rollupOptions: {
    input: {
      index: resolve(__dirname, "index.html"),
      groups: resolve(__dirname, "groups.html")
    }
  }
},
test: {
  environment: "jsdom",
  setupFiles: "./src/test/setup.ts",
  restoreMocks: true
}
```

`index.html` must set `window.location.hash = "#/overview"` only when the hash is empty. `groups.html` must do the same for `#/groups`. Both then import `/src/main.tsx`.

- [ ] **Step 6: Run baseline verification**

Run: `npm test --prefix webui -- --run src/app/App.test.tsx`

Expected: PASS.

Run: `npm run lint --prefix webui`

Expected: PASS.

- [ ] **Step 7: Record checkpoint**

Record touched files and the two passing commands in the task plan. Do not run `git commit` because `.git` is absent.

---

### Task 2: Extend the group API for server-side million-scale browsing

**Files:**
- Modify: `internal/gui/groups.go`
- Modify: `internal/gui/groups_test.go`

**Interfaces:**
- Consumes: existing `GroupHandlers.handleList` and `handleDetail`.
- Produces: `GroupSummary.TotalBytes`, `GroupSummary.WastedBytes`.
- Produces: list parameters `q`, `machine`, `min_members`, `sort`.
- Produces: detail parameters `member_page`, `member_size`.
- Preserves: requests with only current parameters and current JSON fields.

- [ ] **Step 1: Add failing list contract tests**

Add table-driven tests that call:

```text
/api/groups?kind=image&page=2&size=100&q=poster&machine=agent-a&min_members=3&sort=reclaim_desc
```

Assert:

```go
if gotArgs := db.QueryArgs(); !reflect.DeepEqual(
    gotArgs,
    []any{"image", "agent-a", "poster", int64(3), 100, 100},
) { ... }
```

Also assert that the response includes:

```json
{"total_bytes":3000,"wasted_bytes":2000}
```

Define `total_bytes` as the sum of all live member sizes and `wasted_bytes` as
`max(total_bytes - largest_live_member_size, 0)`, so reclaim estimates remain
valid even for similar groups whose member sizes differ.

Add invalid-input cases for `q` longer than 256 runes, machine longer than 128 runes, `min_members=0`, and unknown `sort`.

- [ ] **Step 2: Run the focused Go test and verify RED**

Run: `go test -count=1 ./internal/gui -run 'TestGroupsList.*Scale|TestGroupsList.*Filter'`

Expected: FAIL because the new query parameters and fields are absent.

- [ ] **Step 3: Implement fixed-parameter filtering and validated sort**

Add:

```go
type groupListQuery struct {
    kind       string
    page       int
    size       int
    query      string
    machine    string
    minMembers int64
    sort       string
}
```

Allowed sort values:

```go
const (
    groupSortMembers = "members_desc"
    groupSortNewest  = "newest"
    groupSortReclaim = "reclaim_desc"
)
```

Build only the `ORDER BY` fragment from this closed enum. Keep all user values as PostgreSQL parameters. Use an `all_live` CTE for complete group membership and a `matching_groups` CTE for filter inclusion so a machine/path filter never changes member counts or byte totals.

- [ ] **Step 4: Add failing member pagination tests**

Request:

```text
/api/groups/2481?member_page=2&member_size=100
```

Assert the count query runs and member query receives `LIMIT 100 OFFSET 100`. Assert JSON has:

```json
{"member_total":248,"member_page":2,"member_size":100}
```

Also verify a request without member pagination preserves the existing all-members behavior.

- [ ] **Step 5: Implement optional member pagination**

Extend `GroupDetail`:

```go
MemberTotal int64 `json:"member_total"`
MemberPage  int   `json:"member_page,omitempty"`
MemberSize  int   `json:"member_size,omitempty"`
```

Only add `LIMIT/OFFSET` when either member parameter is present. Require both parameters together and validate `member_size` in `1..500`.

- [ ] **Step 6: Run group API tests**

Run: `go test -count=1 ./internal/gui -run '^TestGroups'`

Expected: all group handler tests pass.

- [ ] **Step 7: Record checkpoint**

Record `internal/gui/groups.go`, `internal/gui/groups_test.go`, and the passing command.

---

### Task 3: Implement typed API contracts, client errors, and polling primitives

**Files:**
- Create: `webui/src/api/contracts.ts`
- Create: `webui/src/api/client.ts`
- Create: `webui/src/api/appApi.ts`
- Create: `webui/src/api/appApi.test.ts`
- Create: `webui/src/hooks/usePolling.ts`
- Create: `webui/src/hooks/usePagedGroups.ts`
- Create: `webui/src/hooks/useSelection.ts`
- Create: `webui/src/hooks/hooks.test.tsx`

**Interfaces:**
- Produces: `AppApi` with `listAgents`, `listTasks`, `startScan`, `getAnalysisStatus`, `runAnalysis`, `listGroups`, `getGroup`, `prepareDelete`, `executeDelete`, `getDeleteStatus`.
- Produces: `ApiError` with `status`, `message`, and `retryable`.
- Produces: `useSelection(scopeKey: string, protectedIDs: ReadonlySet<number>)`.

- [ ] **Step 1: Define exact frontend contracts**

At minimum:

```ts
export type GroupKind = "exact" | "image" | "video";
export type DeleteMode = "soft" | "hard";

export interface GroupSummary {
  id: number;
  kind: GroupKind;
  memberCount: number;
  repMachine: string;
  repPath: string;
  machines: string[];
  createdAt: string;
  totalBytes: number;
  wastedBytes: number;
}

export interface GroupMember {
  fileId: number;
  machineId: string;
  path: string;
  size: number;
  mtime: number;
  score: unknown;
}
```

Use camelCase only inside React. Convert all snake_case JSON in `appApi.ts`.

- [ ] **Step 2: Write failing API tests**

Mock `global.fetch` and assert:

- group query encodes all filter values with `URLSearchParams`;
- non-2xx `{error}` becomes `ApiError`;
- 204 and malformed JSON fail closed;
- abort errors are preserved as aborts;
- delete prepare posts sorted unique member IDs;
- delete execute sends the exact token/mode contract.

- [ ] **Step 3: Run API tests and verify RED**

Run: `npm test --prefix webui -- --run src/api/appApi.test.ts`

Expected: FAIL because the modules do not exist.

- [ ] **Step 4: Implement the minimal API layer**

The public interface must be:

```ts
export interface AppApi {
  listAgents(signal?: AbortSignal): Promise<AgentStatus[]>;
  listTasks(signal?: AbortSignal): Promise<ScanTask[]>;
  startScan(input: StartScanInput, signal?: AbortSignal): Promise<{ taskId: string }>;
  getAnalysisStatus(signal?: AbortSignal): Promise<AnalysisStatus>;
  runAnalysis(signal?: AbortSignal): Promise<void>;
  listGroups(query: GroupQuery, signal?: AbortSignal): Promise<GroupPage>;
  getGroup(id: number, memberPage: number, memberSize: number, signal?: AbortSignal): Promise<GroupDetail>;
  prepareDelete(memberIds: number[], signal?: AbortSignal): Promise<DeletePreparation>;
  executeDelete(confirmToken: string, mode: DeleteMode, signal?: AbortSignal): Promise<{ taskId: string }>;
  getDeleteStatus(taskId: string, signal?: AbortSignal): Promise<DeleteTaskStatus>;
}
```

- [ ] **Step 5: Write failing selection and polling hook tests**

Assert:

- selection is sorted and unique;
- protected IDs cannot be selected;
- changing `scopeKey` clears selection;
- polling uses 2 seconds while focused, 10 seconds while hidden;
- terminal predicates stop polling;
- aborted stale requests never replace fresh state.

- [ ] **Step 6: Implement hooks and bounded page cache**

`usePagedGroups` may cache at most five pages per serialized query. Use `AbortController` on query/page changes. Do not add a global state library.

- [ ] **Step 7: Run focused frontend tests**

Run: `npm test --prefix webui -- --run src/api/appApi.test.ts src/hooks/hooks.test.tsx`

Expected: PASS.

- [ ] **Step 8: Record checkpoint**

Record API/hook files and passing commands.

---

### Task 4: Generate and integrate the V4 visual system and application shell

**Files:**
- Create: `webui/src/assets/aurora-surface.png`
- Create: `webui/src/assets/media-placeholder.png`
- Create: `webui/src/styles/tokens.css`
- Create: `webui/src/styles/global.css`
- Create: `webui/src/app/navigation.ts`
- Create: `webui/src/components/AppShell.tsx`
- Create: `webui/src/components/AppShell.test.tsx`
- Create: `webui/src/components/AsyncState.tsx`
- Create: `webui/src/components/Modal.tsx`
- Create: `webui/src/components/Modal.test.tsx`
- Create: `webui/src/components/VirtualTable.tsx`

**Interfaces:**
- Consumes: generated local PNG assets.
- Produces: `<AppShell>`, `<AsyncState>`, `<Modal>`, and `<VirtualTable>`.
- Produces: CSS tokens used by every feature.

- [ ] **Step 1: Generate two local raster assets**

Generate:

1. A low-contrast blue/indigo/violet/cyan aurora mesh, no text, no logos, no sharp focal point, suitable for `background-size: cover`.
2. An abstract media thumbnail placeholder with layered film-frame and image-plane shapes, no text, no people, no brand.

Save as the exact paths listed above and visually inspect both before use.

- [ ] **Step 2: Write failing shell and modal tests**

Assert:

- all six nav links render;
- active route has `aria-current="page"`;
- compact desktop nav becomes a drawer below 1100px;
- modal receives initial focus, traps Tab/Shift+Tab, closes with Escape, and restores prior focus;
- reduced-motion media query removes nonessential transitions.

- [ ] **Step 3: Run component tests and verify RED**

Run: `npm test --prefix webui -- --run src/components/AppShell.test.tsx src/components/Modal.test.tsx`

Expected: FAIL because components do not exist.

- [ ] **Step 4: Implement tokens and high-readability glass surfaces**

Use CSS fallback first:

```css
body {
  background:
    linear-gradient(135deg, rgba(219,232,255,.72), rgba(238,230,255,.72) 55%, rgba(223,244,243,.72)),
    url("../assets/aurora-surface.png") center / cover fixed;
}
```

Data tables and dialogs must use at least `rgba(255,255,255,.92)`. Danger actions use an opaque danger color.

- [ ] **Step 5: Implement the app shell and primitives**

Use semantic landmarks, buttons with text labels, `:focus-visible`, and no inline event attributes. `VirtualTable` wraps TanStack Virtual and accepts row estimate, overscan, items, and a render callback.

- [ ] **Step 6: Run component tests, lint, and image inspection**

Run: `npm test --prefix webui -- --run src/components`

Expected: PASS.

Run: `npm run lint --prefix webui`

Expected: PASS.

- [ ] **Step 7: Record checkpoint**

Record generated asset paths, visual inspection result, and passing commands.

---

### Task 5: Build Agent, scan, analysis, overview, and audit workspaces

**Files:**
- Create: `webui/src/features/overview/OverviewPage.tsx`
- Create: `webui/src/features/agents/AgentsPage.tsx`
- Create: `webui/src/features/agents/AgentsPage.test.tsx`
- Create: `webui/src/features/scans/ScansPage.tsx`
- Create: `webui/src/features/scans/ScansPage.test.tsx`
- Create: `webui/src/features/analysis/AnalysisPage.tsx`
- Create: `webui/src/features/analysis/AnalysisPage.test.tsx`
- Create: `webui/src/features/deletion/DeleteStatusPanel.tsx`
- Create: `webui/src/features/deletion/DeleteStatusPanel.test.tsx`

**Interfaces:**
- Consumes: `AppApi`, `usePolling`, and shared async states.
- Produces: routed operational pages used by `App`.

- [ ] **Step 1: Write failing feature tests**

Required cases:

- Agent page shows online/offline text and last error without HTML interpretation.
- Scan form rejects no agent and empty roots.
- Roots split on `|`, trim whitespace, and preserve Windows backslashes.
- Successful scan displays the returned task ID.
- Task polling stops when every task is terminal.
- Analysis 409 is rendered as “已有分析正在运行”.
- Analysis preserves last stats when `last_err` is non-empty.
- Delete status shows per-machine progress and sanitized error codes.

- [ ] **Step 2: Run tests and verify RED**

Run: `npm test --prefix webui -- --run src/features/agents src/features/scans src/features/analysis src/features/deletion/DeleteStatusPanel.test.tsx`

Expected: FAIL because feature pages do not exist.

- [ ] **Step 3: Implement Agent and scan flows**

Scan request:

```ts
{
  machine_id: selectedMachine,
  roots: rootsText.split("|").map(value => value.trim()).filter(Boolean),
  phase: 1,
  rescan
}
```

Do not clear form fields after a failed request.

- [ ] **Step 4: Implement analysis and audit flows**

Polling interval: 2 seconds focused, 10 seconds hidden. Stop analysis polling when `running === false`; stop delete polling when `complete === true`.

- [ ] **Step 5: Implement overview as a composition of real data**

Do not invent aggregate totals absent from APIs. Label incomplete metrics as “当前已加载” rather than implying a global total.

- [ ] **Step 6: Run feature tests**

Run: `npm test --prefix webui -- --run src/features/agents src/features/scans src/features/analysis src/features/deletion/DeleteStatusPanel.test.tsx`

Expected: PASS.

- [ ] **Step 7: Record checkpoint**

Record feature files and passing command.

---

### Task 6: Implement the million-scale duplicate-group workbench

**Files:**
- Create: `webui/src/features/groups/GroupFilters.tsx`
- Create: `webui/src/features/groups/GroupTable.tsx`
- Create: `webui/src/features/groups/GroupDetail.tsx`
- Create: `webui/src/features/groups/GroupsPage.tsx`
- Create: `webui/src/features/groups/GroupsPage.css`
- Create: `webui/src/features/groups/GroupsPage.test.tsx`

**Interfaces:**
- Consumes: `AppApi.listGroups`, `AppApi.getGroup`, `usePagedGroups`, `useSelection`, `VirtualTable`.
- Produces: selection passed to deletion as sorted file IDs.

- [ ] **Step 1: Write failing workbench tests**

Assert:

- initial request is `kind=exact&page=1&size=100`;
- switching kind resets page, detail, and selection;
- 300ms search debounce emits one request;
- stale list/detail responses are ignored;
- header select-all affects only current page rows;
- representative ID is disabled and evicted if a refreshed detail promotes it;
- offline Agent members are disabled;
- table renders far fewer DOM rows than a synthetic 100-row page;
- a 1,000,000-file fixture still retains only the current 100 group summaries.

- [ ] **Step 2: Run test and verify RED**

Run: `npm test --prefix webui -- --run src/features/groups/GroupsPage.test.tsx`

Expected: FAIL because group components do not exist.

- [ ] **Step 3: Implement the three-column responsive layout**

Desktop:

```text
210px filters | minmax(620px, 1fr) group table | 350px detail
```

Below 1100px the detail becomes a right drawer. Mobile hides batch deletion.

- [ ] **Step 4: Implement server query and virtualized rows**

The query model is:

```ts
{
  kind,
  page,
  size: 100,
  q,
  machine,
  minMembers,
  sort
}
```

Use a 44px row estimate in compact mode, 56px in comfortable mode, and overscan 8.

- [ ] **Step 5: Implement paged member detail and score rendering**

Render score values as text only. Unknown score objects use a deterministic key/value list; never use `dangerouslySetInnerHTML`.

- [ ] **Step 6: Run group tests**

Run: `npm test --prefix webui -- --run src/features/groups/GroupsPage.test.tsx`

Expected: PASS.

- [ ] **Step 7: Record checkpoint**

Record group feature files and passing command.

---

### Task 7: Port the full two-step deletion state machine to React

**Files:**
- Create: `webui/src/features/deletion/DeleteDialog.tsx`
- Create: `webui/src/features/deletion/DeleteDialog.test.tsx`
- Modify: `webui/src/features/groups/GroupsPage.tsx`
- Delete after replacement passes: `internal/gui/delete_web_test.go`

**Interfaces:**
- Consumes: sorted selected file IDs, `AppApi.prepareDelete`, `executeDelete`, and `getDeleteStatus`.
- Produces: accepted task ID, terminal callback, retry controls.

- [ ] **Step 1: Port the existing browser regression cases as failing React tests**

Cover every behavior currently asserted by `delete_web_test.go`:

- first click performs prepare only;
- prepare body contains sorted unique member IDs;
- second confirmation sends exact token and mode;
- soft delete is preselected;
- cancel and Escape never execute;
- prepare/execute in flight disable conflicting controls;
- focus remains trapped while busy;
- poll interval is at least 1 second;
- terminal completion stops polling and refreshes groups once;
- status failure retries the same accepted task, never a new execute;
- representative changes evict stale selection;
- malicious paths/error text render as text;
- view changes cannot cancel an execute awaiting acceptance.

- [ ] **Step 2: Run deletion tests and verify RED**

Run: `npm test --prefix webui -- --run src/features/deletion/DeleteDialog.test.tsx`

Expected: FAIL because the state machine does not exist.

- [ ] **Step 3: Implement explicit deletion phases**

Use:

```ts
type DeletePhase =
  | { name: "idle" }
  | { name: "preparing" }
  | { name: "confirming"; preparation: DeletePreparation }
  | { name: "executing"; preparation: DeletePreparation; mode: DeleteMode }
  | { name: "polling"; taskId: string }
  | { name: "poll-error"; taskId: string; error: ApiError }
  | { name: "terminal"; status: DeleteTaskStatus };
```

Never infer state from button labels or DOM.

- [ ] **Step 4: Run deletion tests and verify GREEN**

Run: `npm test --prefix webui -- --run src/features/deletion/DeleteDialog.test.tsx`

Expected: PASS.

- [ ] **Step 5: Remove the legacy inline-page browser test only after replacement coverage is green**

Delete `internal/gui/delete_web_test.go`. Keep all Go HTTP/service tests in `delete_http_test.go`, `delete_test.go`, and integration tests.

- [ ] **Step 6: Run both frontend deletion and Go deletion tests**

Run: `npm test --prefix webui -- --run src/features/deletion`

Expected: PASS.

Run: `go test -count=1 ./internal/gui -run 'Delete'`

Expected: PASS.

- [ ] **Step 7: Record checkpoint**

List the deleted legacy test and the exact React replacement cases.

---

### Task 8: Preserve legacy pages and integrate deterministic web builds

**Files:**
- Create: `webui/public/legacy.html`
- Create: `webui/public/legacy-groups.html`
- Create: `scripts/build-web.ps1`
- Modify: `scripts/build.ps1`
- Modify: `.gitignore`
- Modify: `internal/gui/groups_test.go`
- Modify: `internal/gui/httpapi_test.go`
- Generate: `internal/gui/web/index.html`
- Generate: `internal/gui/web/groups.html`
- Generate: `internal/gui/web/legacy.html`
- Generate: `internal/gui/web/legacy-groups.html`
- Generate: `internal/gui/web/assets/*`

**Interfaces:**
- Consumes: `npm ci` and `npm run build`.
- Produces: a self-contained `internal/gui/web` accepted by `go:embed web`.

- [ ] **Step 1: Copy the two current HTML files into `webui/public` before replacing output**

`legacy.html` is the current `internal/gui/web/index.html`.  
`legacy-groups.html` is the current `internal/gui/web/groups.html`.

- [ ] **Step 2: Write failing Go static-page tests**

Assert:

- `/` and `/groups` return HTML containing `id="root"`;
- `/legacy.html` and `/legacy-groups.html` return 200;
- both React entries reference only local `/assets/` files;
- one referenced JS and CSS asset can be read from `webFS()`;
- no HTML contains `http://`, `https://`, or a remote script/style URL.

- [ ] **Step 3: Run static-page tests and verify RED**

Run: `go test -count=1 ./internal/gui -run 'Embedded|Static|Web'`

Expected: FAIL before the React build output exists.

- [ ] **Step 4: Add `scripts/build-web.ps1`**

The script must:

1. resolve repository root;
2. require `node` and `npm`;
3. use `npm ci` when `package-lock.json` exists;
4. run `npm test`, `npm run lint`, and `npm run build`;
5. verify `internal/gui/web/index.html`, `groups.html`, and at least one asset exist;
6. fail without partially copying hand-written files.

- [ ] **Step 5: Invoke web build from `scripts/build.ps1`**

Call `scripts/build-web.ps1` after the `$MediacoreOnly` early return and before building `gui.exe`. Add an opt-out switch only for an explicit already-built-assets CI path; default builds the web UI.

- [ ] **Step 6: Build and rerun static-page tests**

Run: `& .\scripts\build-web.ps1`

Expected: frontend tests/lint/build pass and static assets are generated.

Run: `go test -count=1 ./internal/gui -run 'Embedded|Static|Web'`

Expected: PASS.

- [ ] **Step 7: Update `.gitignore`**

Add:

```gitignore
/.superpowers/
/webui/node_modules/
/webui/dist/
```

Do not ignore `internal/gui/web/assets/`.

- [ ] **Step 8: Record checkpoint**

Record the generated asset manifest and passing commands.

---

### Task 9: Integrate routes, run visual QA, and close verification

**Files:**
- Modify: `webui/src/app/App.tsx`
- Modify: `webui/src/app/App.test.tsx`
- Create: `docs/acceptance/2026-07-31-v4-web-redesign.md`

**Interfaces:**
- Consumes: all prior tasks.
- Produces: final embedded V4 console and acceptance evidence.

- [ ] **Step 1: Wire all routes**

Required routes:

```tsx
<Route path="/overview" element={<OverviewPage />} />
<Route path="/agents" element={<AgentsPage />} />
<Route path="/scans" element={<ScansPage />} />
<Route path="/analysis" element={<AnalysisPage />} />
<Route path="/groups" element={<GroupsPage />} />
<Route path="/audit" element={<DeleteStatusPanel />} />
```

Unknown routes redirect to `/overview`.

- [ ] **Step 2: Run the complete frontend gate**

Run: `npm test --prefix webui`

Expected: PASS.

Run: `npm run lint --prefix webui`

Expected: PASS.

Run: `npm run build --prefix webui`

Expected: PASS.

- [ ] **Step 3: Run focused and full Go gates**

Run: `go test -count=1 ./internal/gui ./cmd/gui`

Expected: PASS.

Run: `go test -count=1 ./...`

Expected: PASS, or document an unrelated pre-existing environment failure with exact output.

- [ ] **Step 4: Launch the local GUI or a test API fixture and inspect in a real browser**

Inspect:

- 1440×900;
- 1280×800;
- 1024×768 responsive detail drawer;
- keyboard-only navigation;
- group list scroll with 100 rows;
- member selection and prepare dialog;
- soft/hard mode warning;
- loading, empty, 409, 503, and network error states;
- console and network panels for request storms or uncaught errors.

- [ ] **Step 5: Capture visual evidence**

Save screenshots to the acceptance evidence directory or another non-production artifact directory. Verify generated UI assets are crisp, unobtrusive, and have CSS fallbacks.

- [ ] **Step 6: Write acceptance evidence**

Document:

- environment versions;
- commands and exit status;
- viewport checks;
- API fixtures or real service used;
- known limitations, especially offset pagination and PostgreSQL substring-search cost;
- confirmation that no remote assets loaded.

- [ ] **Step 7: Record final checkpoint**

List all changed/generated files and exact verification results. Do not claim success until every required gate has fresh output.
