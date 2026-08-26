import { useEffect, useRef, useState } from "react";

export interface CopyButtonProps {
  readonly text: string;
  readonly label?: string;
  readonly className?: string;
}

/** 复制文本到剪贴板，成功后短暂显示"已复制"。 */
export function CopyButton({ text, label = "复制", className }: CopyButtonProps) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const timerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => {
    if (timerRef.current !== undefined) {
      clearTimeout(timerRef.current);
    }
  }, []);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setState("copied");
    } catch {
      setState("failed");
    }
    if (timerRef.current !== undefined) {
      clearTimeout(timerRef.current);
    }
    timerRef.current = setTimeout(() => setState("idle"), 2000);
  };

  const shown = state === "copied" ? "已复制" : state === "failed" ? "复制失败" : label;
  return (
    <button type="button" className={className ?? "copy-button"} onClick={copy} aria-live="polite">
      {shown}
    </button>
  );
}
