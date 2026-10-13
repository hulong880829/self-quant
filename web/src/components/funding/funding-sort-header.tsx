"use client";

import { ArrowDown, ArrowDownUp, ArrowUp, Info } from "lucide-react";

import { cn } from "@/lib/utils";

export function SortHeader({
  label,
  description,
  sorted,
  align = "left",
  onClick,
}: {
  label: string;
  description?: string;
  sorted: false | "asc" | "desc";
  align?: "left" | "right";
  onClick: () => void;
}) {
  const Icon = sorted === "asc" ? ArrowUp : sorted === "desc" ? ArrowDown : ArrowDownUp;
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "inline-flex items-center gap-1 whitespace-nowrap text-[11px] font-medium uppercase tracking-wider text-muted-foreground transition-colors hover:text-foreground",
        align === "right" && "w-full justify-end",
      )}
      title={description}
      aria-label={description ? `${label}：${description}` : label}
    >
      {label}
      {description ? <Info className="size-3 opacity-55" /> : null}
      <Icon className={cn("size-3", !sorted && "opacity-35")} />
    </button>
  );
}
