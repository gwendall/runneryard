"use client";

import { useState } from "react";

export function Command({ children, label = "Terminal" }: { children: string; label?: string }) {
  const [status, setStatus] = useState<"idle" | "copied" | "failed">("idle");

  async function copy() {
    try {
      await navigator.clipboard.writeText(children);
      setStatus("copied");
    } catch {
      setStatus("failed");
    }
    window.setTimeout(() => setStatus("idle"), 1800);
  }

  return (
    <div className="command">
      <div className="command-bar">
        <span>{label}</span>
        <button type="button" onClick={copy} aria-live="polite">
          {status === "copied" ? "Copied" : status === "failed" ? "Select manually" : "Copy"}
        </button>
      </div>
      <pre><code>{children}</code></pre>
    </div>
  );
}
