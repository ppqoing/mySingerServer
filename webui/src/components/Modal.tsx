import { useEffect, useId, useLayoutEffect, useRef, type KeyboardEvent as ReactKeyboardEvent, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";
import { overlayLayer, registerOverlay, type OverlayHandle } from "./overlayStack";

export interface ModalProps {
  readonly children: ReactNode;
  readonly disableClose?: boolean;
  readonly disableEscape?: boolean;
  readonly initialFocusRef?: RefObject<HTMLElement | null>;
  readonly onClose: () => void;
  readonly open: boolean;
  readonly title: string;
}

const focusableSelector = "a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex='-1'])";

function getFocusable(container: HTMLElement) {
  return Array.from(container.querySelectorAll<HTMLElement>("*")).filter(
    (element) => element.matches(focusableSelector) &&
      !element.matches(":disabled") &&
      !element.hasAttribute("hidden") &&
      element.getAttribute("aria-hidden") !== "true"
  );
}

export function Modal({
  children,
  disableClose = false,
  disableEscape = false,
  initialFocusRef,
  onClose,
  open,
  title
}: ModalProps) {
  const backdropRef = useRef<HTMLDivElement>(null);
  const dialogRef = useRef<HTMLDivElement>(null);
  const overlayRef = useRef<OverlayHandle | undefined>(undefined);
  const titleId = useId();

  useLayoutEffect(() => {
    if (!open) return;
    const backdrop = backdropRef.current;
    const dialog = dialogRef.current;
    if (!backdrop || !dialog) return;
    const handle = registerOverlay([backdrop, dialog], {
      layer: overlayLayer.modal,
      restoreFocus: document.activeElement instanceof HTMLElement ? document.activeElement : null
    });
    overlayRef.current = handle;
    if (handle.isTop()) {
      const initialTarget = initialFocusRef?.current ?? getFocusable(dialog)[0] ?? dialog;
      initialTarget.focus();
    }
    return () => {
      overlayRef.current = undefined;
      handle.release();
    };
  }, [initialFocusRef, open]);

  useEffect(() => {
    if (!open || disableClose || disableEscape) return;
    const closeOnEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape" && overlayRef.current?.isTop()) onClose();
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => document.removeEventListener("keydown", closeOnEscape);
  }, [disableClose, disableEscape, onClose, open]);

  useLayoutEffect(() => {
    if (!open || !overlayRef.current?.isTop()) return;
    const dialog = dialogRef.current;
    const active = document.activeElement;
    if (!dialog || !(active instanceof HTMLElement)) return;
    if (!dialog.contains(active) || active.matches(":disabled")) {
      (getFocusable(dialog)[0] ?? dialog).focus();
    }
  });

  useEffect(() => {
    if (!open) return undefined;
    const keepFocusInside = (event: FocusEvent) => {
      if (!overlayRef.current?.isTop()) return;
      const dialog = dialogRef.current;
      if (!dialog || dialog.contains(event.target as Node)) return;
      (getFocusable(dialog)[0] ?? dialog).focus();
    };
    document.addEventListener("focusin", keepFocusInside);
    return () => document.removeEventListener("focusin", keepFocusInside);
  }, [open]);

  if (!open) return null;

  const trapFocus = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    if (event.key !== "Tab") return;
    const dialog = dialogRef.current;
    if (!dialog) return;
    const focusable = getFocusable(dialog);
    if (focusable.length === 0) {
      event.preventDefault();
      dialog.focus();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  return createPortal(
    <div className="modal__backdrop" data-testid="modal-backdrop" onClick={(event) => {
      if (!disableClose && overlayRef.current?.isTop() && event.target === event.currentTarget) onClose();
    }} ref={backdropRef}>
      <div aria-labelledby={titleId} aria-modal="true" className="modal__dialog" onKeyDown={trapFocus} ref={dialogRef} role="dialog" tabIndex={-1}>
        <div className="modal__header">
          <h2 className="modal__title" id={titleId}>{title}</h2>
          <button
            aria-label={`关闭 ${title}`}
            className="modal__close"
            disabled={disableClose}
            onClick={onClose}
            type="button"
          >
            关闭
          </button>
        </div>
        <div className="modal__content">{children}</div>
      </div>
    </div>,
    document.body
  );
}
