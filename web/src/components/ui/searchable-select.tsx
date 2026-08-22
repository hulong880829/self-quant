"use client";

import * as React from "react";

import { cn } from "@/lib/utils";

export type SearchableSelectOption = {
  value: string;
  label: string;
  keywords?: string[];
};

export function compactSearchText(value: string): string {
  return value.toLowerCase().replace(/[/\\\-\s_]/g, "");
}

export function optionMatchesQuery(option: SearchableSelectOption, query: string): boolean {
  const trimmed = query.trim();
  if (!trimmed) return true;
  const lower = trimmed.toLowerCase();
  const compact = compactSearchText(trimmed);
  const haystacks = [option.label, option.value, ...(option.keywords ?? [])];
  return haystacks.some((item) => {
    const text = String(item ?? "");
    return text.toLowerCase().includes(lower) || compactSearchText(text).includes(compact);
  });
}

export function SearchableSelect({
  value,
  onValueChange,
  options,
  placeholder = "搜索…",
  emptyText = "无匹配结果",
  disabled = false,
  maxResults = 50,
  "aria-label": ariaLabel,
}: {
  value: string;
  onValueChange: (value: string) => void;
  options: SearchableSelectOption[];
  placeholder?: string;
  emptyText?: string;
  disabled?: boolean;
  maxResults?: number;
  "aria-label"?: string;
}) {
  const rootRef = React.useRef<HTMLDivElement>(null);
  const inputRef = React.useRef<HTMLInputElement>(null);
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [activeIndex, setActiveIndex] = React.useState(0);
  const selected = options.find((item) => item.value === value) ?? null;
  const matches = React.useMemo(
    () => options.filter((item) => optionMatchesQuery(item, query)).slice(0, maxResults),
    [options, query, maxResults],
  );
  const safeActiveIndex = matches.length === 0 ? 0 : Math.min(activeIndex, matches.length - 1);

  React.useEffect(() => {
    function onPointerDown(event: PointerEvent) {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
        setQuery("");
        setActiveIndex(0);
      }
    }
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, []);

  function choose(option: SearchableSelectOption) {
    onValueChange(option.value);
    setQuery("");
    setActiveIndex(0);
    setOpen(false);
    inputRef.current?.blur();
  }

  function onKeyDown(event: React.KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Escape") {
      event.preventDefault();
      setOpen(false);
      setQuery("");
      setActiveIndex(0);
      return;
    }
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setOpen(true);
      setActiveIndex((current) => (matches.length === 0 ? 0 : (current + 1) % matches.length));
      return;
    }
    if (event.key === "ArrowUp") {
      event.preventDefault();
      setOpen(true);
      setActiveIndex((current) =>
        matches.length === 0 ? 0 : (current - 1 + matches.length) % matches.length,
      );
      return;
    }
    if (event.key === "Enter" && open) {
      event.preventDefault();
      const option = matches[safeActiveIndex];
      if (option) choose(option);
    }
  }

  return (
    <div ref={rootRef} className="relative">
      <input
        ref={inputRef}
        type="text"
        role="combobox"
        aria-expanded={open}
        aria-autocomplete="list"
        aria-controls="searchable-select-listbox"
        aria-label={ariaLabel}
        disabled={disabled}
        placeholder={placeholder}
        value={open ? query : (selected?.label ?? "")}
        onFocus={() => {
          setOpen(true);
          setQuery("");
        }}
        onChange={(event) => {
          setQuery(event.target.value);
          setOpen(true);
          setActiveIndex(0);
        }}
        onKeyDown={onKeyDown}
        className={cn(
          "h-9 w-full rounded-lg border border-input bg-background px-3 text-sm outline-none transition-colors",
          "focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50",
          "disabled:cursor-not-allowed disabled:bg-input/50 disabled:opacity-50",
        )}
      />
      {open ? (
        <ul
          id="searchable-select-listbox"
          role="listbox"
          className="absolute z-50 mt-1 max-h-56 w-full overflow-y-auto rounded-lg border bg-popover p-1 text-sm shadow-md"
        >
          {matches.length === 0 ? (
            <li className="px-2 py-2 text-muted-foreground">{emptyText}</li>
          ) : (
            matches.map((option, index) => (
              <li
                key={option.value}
                role="option"
                aria-selected={option.value === value}
                className={cn(
                  "cursor-pointer rounded-md px-2 py-1.5",
                  index === safeActiveIndex ? "bg-muted text-foreground" : "text-foreground",
                )}
                onMouseDown={(event) => event.preventDefault()}
                onMouseEnter={() => setActiveIndex(index)}
                onClick={() => choose(option)}
              >
                {option.label}
              </li>
            ))
          )}
        </ul>
      ) : null}
    </div>
  );
}
