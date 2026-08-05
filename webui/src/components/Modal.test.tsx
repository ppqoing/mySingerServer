import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, useState } from "react";
import { describe, expect, test } from "vitest";
import { Modal } from "./Modal";

function ModalHarness({
  disableClose = false,
  disableEscape = false,
  withInitialFocus = false
}: {
  disableClose?: boolean;
  disableEscape?: boolean;
  withInitialFocus?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const initialFocusRef = createRef<HTMLButtonElement>();
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>打开确认</button>
      <Modal
        disableClose={disableClose}
        disableEscape={disableEscape}
        initialFocusRef={withInitialFocus ? initialFocusRef : undefined}
        onClose={() => setOpen(false)}
        open={open}
        title="删除确认"
      >
        <button ref={initialFocusRef} type="button">保留文件</button>
        <button type="button">删除文件</button>
      </Modal>
    </>
  );
}

function StackedModalHarness() {
  const [outerOpen, setOuterOpen] = useState(false);
  const [innerOpen, setInnerOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOuterOpen(true)}>打开外层</button>
      <button data-testid="force-close-outer" type="button" onClick={() => setOuterOpen(false)}>
        强制关闭外层
      </button>
      <Modal onClose={() => setOuterOpen(false)} open={outerOpen} title="外层确认">
        <button type="button" onClick={() => setInnerOpen(true)}>打开内层</button>
      </Modal>
      <Modal onClose={() => setInnerOpen(false)} open={innerOpen} title="内层确认">
        <button type="button">内层操作</button>
      </Modal>
    </>
  );
}

function BusyModalHarness() {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  return <>
    <button type="button" onClick={() => setOpen(true)}>打开忙碌确认</button>
    <button type="button">页面外操作</button>
    <Modal disableClose={busy} onClose={() => setOpen(false)} open={open} title="忙碌确认">
      <fieldset disabled={busy}>
        <input aria-label="确认说明" />
        <button type="button" onClick={() => setBusy(true)}>提交任务</button>
      </fieldset>
    </Modal>
  </>;
}

describe("Modal", () => {
  test("moves focus to the supplied initial target", async () => {
    const user = userEvent.setup();
    render(<ModalHarness withInitialFocus />);

    await user.click(screen.getByRole("button", { name: "打开确认" }));
    expect(screen.getByRole("button", { name: "保留文件" })).toHaveFocus();
  });

  test("wraps Shift+Tab from the first focusable to the last and Tab back to the first", async () => {
    const user = userEvent.setup();
    render(<ModalHarness />);

    await user.click(screen.getByRole("button", { name: "打开确认" }));
    expect(screen.getByRole("button", { name: "关闭 删除确认" })).toHaveFocus();

    await user.tab({ shift: true });
    expect(screen.getByRole("button", { name: "删除文件" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "关闭 删除确认" })).toHaveFocus();
  });

  test("closes with Escape and restores the opener focus", async () => {
    const user = userEvent.setup();
    render(<ModalHarness />);
    const opener = screen.getByRole("button", { name: "打开确认" });

    await user.click(opener);
    await user.keyboard("{Escape}");

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(opener).toHaveFocus();
  });

  test("does not close from dialog content clicks and respects disabled Escape", async () => {
    const user = userEvent.setup();
    render(<ModalHarness disableEscape />);

    await user.click(screen.getByRole("button", { name: "打开确认" }));
    await user.click(screen.getByRole("button", { name: "删除文件" }));
    await user.keyboard("{Escape}");

    expect(screen.getByRole("dialog", { name: "删除确认" })).toBeInTheDocument();
  });

  test("closes only when the actual backdrop is clicked", async () => {
    const user = userEvent.setup();
    render(<ModalHarness />);

    await user.click(screen.getByRole("button", { name: "打开确认" }));
    await user.click(screen.getByTestId("modal-backdrop"));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  test("disableClose removes every dismiss path and makes the close button unfocusable", async () => {
    const user = userEvent.setup();
    render(<ModalHarness disableClose />);

    await user.click(screen.getByRole("button", { name: "打开确认" }));
    expect(screen.getByRole("button", { name: "关闭 删除确认" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "保留文件" })).toHaveFocus();

    await user.keyboard("{Escape}");
    await user.click(screen.getByTestId("modal-backdrop"));

    expect(screen.getByRole("dialog", { name: "删除确认" })).toBeInTheDocument();
  });

  test("keeps focus inside when a busy fieldset disables the active control", async () => {
    const user = userEvent.setup();
    render(<BusyModalHarness />);

    await user.click(screen.getByRole("button", { name: "打开忙碌确认" }));
    await user.click(screen.getByRole("button", { name: "提交任务" }));

    const dialog = screen.getByRole("dialog", { name: "忙碌确认" });
    expect(dialog).toHaveFocus();
    expect(screen.getByRole("button", { name: "提交任务" })).toBeDisabled();

    const outside = screen.getByRole("button", { name: "页面外操作", hidden: true });
    outside.focus();
    fireEvent.focusIn(outside);
    expect(dialog).toHaveFocus();
  });

  test("keeps only the top modal active and survives out-of-order lower-overlay removal", async () => {
    const user = userEvent.setup();
    const view = render(<StackedModalHarness />);
    const outerOpener = screen.getByRole("button", { name: "打开外层" });

    await user.click(outerOpener);
    await user.click(screen.getByRole("button", { name: "打开内层" }));

    const outer = screen.getByText("外层确认").closest('[role="dialog"]');
    const inner = screen.getByRole("dialog", { name: "内层确认" });
    const innerClose = screen.getByRole("button", { name: "关闭 内层确认" });
    expect(outer).not.toBeNull();
    expect(view.container).toHaveAttribute("aria-hidden", "true");
    expect(view.container).toHaveAttribute("inert");
    expect(outer).toHaveAttribute("aria-hidden", "true");
    expect(outer).toHaveAttribute("inert");
    expect(inner).not.toHaveAttribute("aria-hidden");
    expect(inner).not.toHaveAttribute("inert");
    expect(innerClose).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");

    fireEvent.click(screen.getByTestId("force-close-outer"));

    expect(outer).not.toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "内层确认" })).toBeInTheDocument();
    expect(innerClose).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");
    expect(view.container).toHaveAttribute("aria-hidden", "true");

    await user.keyboard("{Escape}");

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(document.body.style.overflow).toBe("");
    expect(view.container).not.toHaveAttribute("aria-hidden");
    expect(view.container).not.toHaveAttribute("inert");
    expect(outerOpener).toHaveFocus();
  });
});
