import * as React from "react";

import { cn } from "@/lib/utils";

export function PageFrame({
  className,
  children,
}: {
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      data-page-frame
      className={cn(
        "flex min-h-[var(--app-page-min-height)] min-w-0 max-w-full flex-col gap-3",
        className,
      )}
    >
      {children}
    </div>
  );
}

export const WorkspacePanel = React.forwardRef<
  HTMLDivElement,
  {
    className?: string;
    children: React.ReactNode;
  }
>(function WorkspacePanel({ className, children }, ref) {
  return (
    <div
      ref={ref}
      data-workspace-panel
      className={cn(
        "grid min-h-0 min-w-0 overflow-hidden rounded-xl border bg-card shadow-sm",
        "h-[var(--app-panel-height)] max-h-[var(--app-panel-height)]",
        className,
      )}
    >
      {children}
    </div>
  );
});

export function WideTableScroll({
  className,
  children,
}: {
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      data-wide-table-scroll
      className={cn("min-w-0 max-w-full overflow-auto", className)}
    >
      {children}
    </div>
  );
}
