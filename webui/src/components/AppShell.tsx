import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode
} from "react";
import { NavLink } from "react-router-dom";
import { navigation } from "../app/navigation";
import "../styles/global.css";

const narrowQuery = "(max-width: 1099px)";

function useNarrowViewport() {
  const getMatches = () => window.matchMedia?.(narrowQuery).matches ?? false;
  const [isNarrow, setIsNarrow] = useState(getMatches);

  useEffect(() => {
    const mediaQuery = window.matchMedia?.(narrowQuery);
    if (!mediaQuery) return;
    const update = () => setIsNarrow(mediaQuery.matches);
    update();
    mediaQuery.addEventListener("change", update);
    return () => mediaQuery.removeEventListener("change", update);
  }, []);

  return isNarrow;
}

export function AppShell({ children }: { children: ReactNode }) {
  const isNarrow = useNarrowViewport();
  const [drawerOpen, setDrawerOpen] = useState(false);
  const navigationRef = useRef<HTMLElement>(null);
  const firstLinkRef = useRef<HTMLAnchorElement>(null);
  const toggleRef = useRef<HTMLButtonElement>(null);
  const restoreToggleFocusRef = useRef(false);
  const isDrawerVisible = isNarrow ? drawerOpen : true;

  const closeDrawer = useCallback(() => {
    restoreToggleFocusRef.current = true;
    setDrawerOpen(false);
  }, []);

  useEffect(() => {
    if (isNarrow && drawerOpen) {
      firstLinkRef.current?.focus();
    }
  }, [drawerOpen, isNarrow]);

  useEffect(() => {
    if (drawerOpen || !restoreToggleFocusRef.current) return;
    restoreToggleFocusRef.current = false;
    toggleRef.current?.focus();
  }, [drawerOpen]);

  useEffect(() => {
    if (!isNarrow || !drawerOpen) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") closeDrawer();
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => document.removeEventListener("keydown", closeOnEscape);
  }, [closeDrawer, drawerOpen, isNarrow]);

  const trapDrawerFocus = (event: ReactKeyboardEvent<HTMLElement>) => {
    if (!isNarrow || !drawerOpen || event.key !== "Tab") return;
    const navigationElement = navigationRef.current;
    if (!navigationElement) return;
    const focusable = Array.from(navigationElement.querySelectorAll<HTMLElement>(
      "button:not([disabled]):not([tabindex='-1']), a[href]:not([tabindex='-1'])"
    ));
    if (focusable.length === 0) return;
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

  const toggleDrawer = () => {
    if (drawerOpen) {
      closeDrawer();
      return;
    }
    restoreToggleFocusRef.current = false;
    setDrawerOpen(true);
  };

  return (
    <div className="app-shell">
      <nav
        aria-hidden={!isDrawerVisible}
        aria-label="工作区"
        className="app-shell__nav"
        data-open={isDrawerVisible}
        id="app-primary-navigation"
        onKeyDown={trapDrawerFocus}
        ref={navigationRef}
      >
        {isNarrow && drawerOpen ? (
          <button className="app-shell__nav-close" onClick={closeDrawer} type="button">
            关闭导航
          </button>
        ) : null}
        <p className="app-shell__brand">媒体去重控制台</p>
        {navigation.map(({ label, to }, index) => (
          <NavLink
            className="app-shell__nav-link"
            key={to}
            onClick={() => {
              if (isNarrow) closeDrawer();
            }}
            ref={index === 0 ? firstLinkRef : undefined}
            tabIndex={isDrawerVisible ? undefined : -1}
            to={to}
          >
            {label}
          </NavLink>
        ))}
      </nav>
      {isNarrow && drawerOpen ? (
        <button
          aria-label="关闭导航遮罩"
          className="app-shell__scrim"
          onClick={closeDrawer}
          tabIndex={-1}
          type="button"
        />
      ) : null}
      <header className="app-shell__header">
        <button
          aria-controls="app-primary-navigation"
          aria-expanded={drawerOpen}
          className="app-shell__menu-toggle"
          onClick={toggleDrawer}
          ref={toggleRef}
          type="button"
        >
          {drawerOpen ? "关闭导航" : "打开导航"}
        </button>
        <strong>媒体去重控制台</strong>
        <span className="app-shell__status">中央服务：正常</span>
      </header>
      <main className="app-shell__main" inert={isNarrow && drawerOpen}>{children}</main>
    </div>
  );
}
