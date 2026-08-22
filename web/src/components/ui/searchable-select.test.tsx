// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SearchableSelect, optionMatchesQuery } from "./searchable-select";

const options = [
  { value: "7", label: "BTC / USDT · BTCUSDT", keywords: ["BTC", "USDT", "BTCUSDT"] },
  { value: "8", label: "ETH / USDT · ETHUSDT", keywords: ["ETH", "USDT", "ETHUSDT"] },
];

describe("SearchableSelect", () => {
  afterEach(() => {
    cleanup();
  });

  it("matches compact and mixed-case queries", () => {
    expect(optionMatchesQuery(options[0], "btc/usdt")).toBe(true);
    expect(optionMatchesQuery(options[0], "BTC-USDT")).toBe(true);
    expect(optionMatchesQuery(options[0], "btcusdt")).toBe(true);
    expect(optionMatchesQuery(options[0], "eth")).toBe(false);
  });

  it("filters options and supports keyboard selection", () => {
    const onValueChange = vi.fn();
    render(
      <SearchableSelect
        aria-label="交易标的"
        value="7"
        onValueChange={onValueChange}
        options={options}
        placeholder="搜索标的"
      />,
    );
    const input = screen.getByRole("combobox", { name: "交易标的" });
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "eth" } });
    expect(screen.getByText("ETH / USDT · ETHUSDT")).toBeTruthy();
    expect(screen.queryByText("BTC / USDT · BTCUSDT")).toBeNull();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onValueChange).toHaveBeenCalledWith("8");
  });

  it("shows an empty state for unmatched queries", () => {
    render(
      <SearchableSelect
        aria-label="交易标的"
        value=""
        onValueChange={() => undefined}
        options={options}
        emptyText="无匹配标的"
      />,
    );
    fireEvent.focus(screen.getByRole("combobox", { name: "交易标的" }));
    fireEvent.change(screen.getByRole("combobox", { name: "交易标的" }), {
      target: { value: "xyz" },
    });
    expect(screen.getByText("无匹配标的")).toBeTruthy();
  });
});
