// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const nav = vi.hoisted(() => ({ pathname: "/funding" }));
const auth = vi.hoisted(() => ({
  status: "loading" as "loading" | "anonymous" | "authenticated",
  user: null as { username: string; permission: string } | null,
}));

vi.mock("next/navigation", () => ({
  usePathname: () => nav.pathname,
}));

vi.mock("@/components/auth/auth-provider", () => ({
  useAuth: () => ({
    status: auth.status,
    user: auth.user,
  }),
}));

import { PersistentWorkspaceBoundary } from "./persistent-workspace-boundary";

let fundingMounts = 0;
let fundingFetches = 0;
let tradingMounts = 0;
let tradingFetches = 0;

function FundingProbe() {
  const id = React.useId();
  const [mountId] = React.useState(() => {
    fundingMounts += 1;
    return fundingMounts;
  });
  React.useEffect(() => {
    fundingFetches += 1;
  }, []);
  return (
    <div data-funding-instance={mountId}>
      <input aria-label="资金费搜索" defaultValue="" />
      <span>{id}</span>
    </div>
  );
}

function TradingProbe() {
  const [mountId] = React.useState(() => {
    tradingMounts += 1;
    return tradingMounts;
  });
  React.useEffect(() => {
    tradingFetches += 1;
  }, []);
  return (
    <div data-trading-instance={mountId}>
      <input aria-label="目标仓位" defaultValue="10000" />
    </div>
  );
}

function Harness() {
  const path = nav.pathname;
  return (
    <PersistentWorkspaceBoundary>
      {path === "/funding" ? (
        <FundingProbe />
      ) : path === "/trading" ? (
        <TradingProbe />
      ) : (
        <div>其他页面</div>
      )}
    </PersistentWorkspaceBoundary>
  );
}

describe("PersistentWorkspaceBoundary", () => {
  beforeEach(() => {
    nav.pathname = "/funding";
    auth.status = "authenticated";
    auth.user = { username: "alice", permission: "user" };
    fundingMounts = 0;
    fundingFetches = 0;
    tradingMounts = 0;
    tradingFetches = 0;
  });

  afterEach(cleanup);

  it("captures the first funding visit synchronously without remounting or double fetching", () => {
    render(<Harness />);
    expect(screen.getByLabelText("资金费搜索")).toBeTruthy();
    expect(fundingMounts).toBe(1);
    expect(fundingFetches).toBe(1);
    expect(document.querySelector("[data-workspace-epoch]")?.getAttribute("data-workspace-epoch")).toBe(
      "0",
    );
  });

  it("keeps funding filters after a round trip through trading", () => {
    const view = render(<Harness />);
    fireEvent.change(screen.getByLabelText("资金费搜索"), { target: { value: "ETH" } });
    nav.pathname = "/trading";
    view.rerender(<Harness />);
    expect(screen.getByLabelText("目标仓位")).toBeTruthy();
    nav.pathname = "/funding";
    view.rerender(<Harness />);
    expect((screen.getByLabelText("资金费搜索") as HTMLInputElement).value).toBe("ETH");
    expect(fundingMounts).toBe(1);
    expect(fundingFetches).toBe(2);
  });

  it("keeps trading drafts and does not reset them on a bare /trading visit", () => {
    nav.pathname = "/trading";
    const view = render(<Harness />);
    fireEvent.change(screen.getByLabelText("目标仓位"), { target: { value: "2500" } });
    nav.pathname = "/funding";
    view.rerender(<Harness />);
    nav.pathname = "/trading";
    view.rerender(<Harness />);
    expect((screen.getByLabelText("目标仓位") as HTMLInputElement).value).toBe("2500");
    expect(tradingMounts).toBe(1);
    expect(tradingFetches).toBe(2);
  });

  it("does not bump epoch or remount when loading resolves to the first owner", () => {
    auth.status = "loading";
    auth.user = null;
    const view = render(<Harness />);
    expect(fundingMounts).toBe(1);
    expect(document.querySelector("[data-workspace-epoch]")?.getAttribute("data-workspace-epoch")).toBe(
      "0",
    );
    auth.status = "authenticated";
    auth.user = { username: "alice", permission: "user" };
    view.rerender(<Harness />);
    expect(document.querySelector("[data-workspace-epoch]")?.getAttribute("data-workspace-epoch")).toBe(
      "0",
    );
    expect(fundingMounts).toBe(1);
    fireEvent.change(screen.getByLabelText("资金费搜索"), { target: { value: "BTC" } });
    expect((screen.getByLabelText("资金费搜索") as HTMLInputElement).value).toBe("BTC");
  });

  it("bumps epoch and clears both slots when a resolved owner actually changes", () => {
    nav.pathname = "/funding";
    const view = render(<Harness />);
    fireEvent.change(screen.getByLabelText("资金费搜索"), { target: { value: "ETH" } });
    nav.pathname = "/trading";
    view.rerender(<Harness />);
    fireEvent.change(screen.getByLabelText("目标仓位"), { target: { value: "8888" } });

    auth.user = { username: "bob", permission: "user" };
    view.rerender(<Harness />);
    expect(document.querySelector("[data-workspace-epoch]")?.getAttribute("data-workspace-epoch")).toBe(
      "1",
    );
    expect(screen.queryByLabelText("资金费搜索")).toBeNull();
    expect((screen.getByLabelText("目标仓位") as HTMLInputElement).value).toBe("10000");
    expect(tradingMounts).toBe(2);

    nav.pathname = "/funding";
    view.rerender(<Harness />);
    expect((screen.getByLabelText("资金费搜索") as HTMLInputElement).value).toBe("");
    expect(fundingMounts).toBe(2);
  });
});
