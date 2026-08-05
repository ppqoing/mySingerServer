import type { ReactNode } from "react";

export type AsyncStateKind = "loading" | "empty" | "error";

export interface AsyncStateProps {
  readonly state: AsyncStateKind | null;
  readonly message?: ReactNode;
  readonly error?: ReactNode;
  readonly onRetry?: () => void;
}

export function AsyncState({ state, message, error, onRetry }: AsyncStateProps) {
  if (state === null) return null;

  if (state === "loading") {
    return <section className="async-state" role="status">{message ?? "正在加载…"}</section>;
  }

  if (state === "empty") {
    return <section className="async-state" role="status">{message ?? "当前没有可显示的数据。"}</section>;
  }

  return (
    <section className="async-state async-state--error" role="alert">
      <p>{error ?? message ?? "加载失败，请稍后重试。"}</p>
      {onRetry ? <div className="async-state__actions"><button onClick={onRetry} type="button">重试</button></div> : null}
    </section>
  );
}
