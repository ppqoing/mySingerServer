import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, test, vi } from "vitest";
import { AppShell } from "./AppShell";
import { AsyncState } from "./AsyncState";
import { VirtualTable } from "./VirtualTable";

function setViewport(width: number) {
  const listeners = new Set<(event: MediaQueryListEvent) => void>();
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: query.includes("1099") && width < 1100,
      media: query,
      onchange: null,
      addEventListener: (_: string, listener: (event: MediaQueryListEvent) => void) => listeners.add(listener),
      removeEventListener: (_: string, listener: (event: MediaQueryListEvent) => void) => listeners.delete(listener),
      dispatchEvent: () => false
    }))
  });
}

function renderShell(path = "/overview") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell>
        <h1>页面内容</h1>
        <button type="button">主内容操作</button>
      </AppShell>
    </MemoryRouter>
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("AppShell", () => {
  test("renders six operational links and marks the active route", () => {
    setViewport(1440);
    renderShell("/groups");

    for (const label of ["总览", "Agent", "扫描任务", "一筛分析", "重复组", "删除审计"]) {
      expect(screen.getByRole("link", { name: label })).toBeInTheDocument();
    }
    expect(screen.getByRole("link", { name: "重复组" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByText("中央服务：正常")).toBeInTheDocument();
  });

  test("moves focus to the first link and traps Tab in the narrow navigation drawer", async () => {
    setViewport(900);
    const user = userEvent.setup();
    renderShell();

    const toggle = screen.getByRole("button", { name: "打开导航" });
    await user.click(toggle);

    const navigation = screen.getByRole("navigation", { name: "工作区" });
    const links = within(navigation).getAllByRole("link");
    const close = within(navigation).getByRole("button", { name: "关闭导航" });
    const lastLink = links[links.length - 1];
    await waitFor(() => expect(links[0]).toHaveFocus());

    close.focus();
    await user.tab({ shift: true });
    expect(lastLink).toHaveFocus();
    await user.tab();
    expect(close).toHaveFocus();
    expect(screen.getByRole("button", { name: "主内容操作" })).not.toHaveFocus();
  });

  test("closes with Escape and restores focus to the external toggle", async () => {
    setViewport(900);
    const user = userEvent.setup();
    renderShell();

    const toggle = screen.getByRole("button", { name: "打开导航" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("aria-controls", "app-primary-navigation");

    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    await waitFor(() => expect(screen.getByRole("link", { name: "总览" })).toHaveFocus());
    await user.keyboard("{Escape}");

    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveFocus();
  });

  test("closes through the internal close control and restores toggle focus", async () => {
    setViewport(900);
    const user = userEvent.setup();
    renderShell();

    const toggle = screen.getByRole("button", { name: "打开导航" });
    await user.click(toggle);
    const navigation = screen.getByRole("navigation", { name: "工作区" });
    await user.click(within(navigation).getByRole("button", { name: "关闭导航" }));

    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await waitFor(() => expect(toggle).toHaveFocus());
  });

  test("closes after route selection and restores toggle focus", async () => {
    setViewport(900);
    const user = userEvent.setup();
    renderShell();

    const toggle = screen.getByRole("button", { name: "打开导航" });
    await user.click(toggle);
    await user.click(screen.getByRole("link", { name: "重复组" }));

    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await waitFor(() => expect(toggle).toHaveFocus());
  });

  test("uses a scrim and makes the main content inert only while the drawer is open", async () => {
    setViewport(900);
    const user = userEvent.setup();
    renderShell();

    const toggle = screen.getByRole("button", { name: "打开导航" });
    const main = screen.getByRole("main");
    expect(main).not.toHaveAttribute("inert");

    await user.click(toggle);
    expect(main).toHaveAttribute("inert");
    const scrim = screen.getByRole("button", { name: "关闭导航遮罩" });
    await user.click(scrim);

    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(main).not.toHaveAttribute("inert");
    await waitFor(() => expect(toggle).toHaveFocus());
  });
});

describe("AsyncState", () => {
  test("renders an honest error with an optional retry button", async () => {
    const retry = vi.fn();
    const user = userEvent.setup();
    render(<AsyncState state="error" error="服务暂不可用" onRetry={retry} />);

    expect(screen.getByRole("alert")).toHaveTextContent("服务暂不可用");
    await user.click(screen.getByRole("button", { name: "重试" }));
    expect(retry).toHaveBeenCalledOnce();
  });
});

describe("VirtualTable", () => {
  test("keeps a bounded subset of a large item list mounted", async () => {
    vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(512);
    vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(800);
    const items = Array.from({ length: 10_000 }, (_, index) => `row-${index}`);
    render(
      <VirtualTable
        ariaLabel="大型数据表"
        items={items}
        estimateSize={() => 44}
        overscan={3}
        rowKey={(item) => item}
        renderRow={(item) => <span>{item}</span>}
      />
    );

    expect(screen.getByRole("list", { name: "大型数据表" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByRole("listitem").length).toBeLessThan(100));
    expect(screen.queryByText("row-9999")).not.toBeInTheDocument();
  });
});
