// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const navigation = vi.hoisted(() => ({
  params: new URLSearchParams(),
}));

const polls = vi.hoisted(() => ({
  manual: 0,
  twap: 0,
  arb: 0,
}));

vi.mock("next/navigation", () => ({
  useSearchParams: () => navigation.params,
}));

vi.mock("@/components/auth/auth-gate", () => ({
  AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("@/components/trading/manual-trading", () => ({
  ManualTradingView: function ManualTradingView() {
    React.useEffect(() => {
      polls.manual += 1;
      const timer = window.setInterval(() => {
        polls.manual += 1;
      }, 20);
      return () => window.clearInterval(timer);
    }, []);
    return <div role="region" aria-label="手动交易视图">MANUAL_VIEW</div>;
  },
}));

vi.mock("@/components/trading/twap-trading", () => ({
  TwapTradingView: function TwapTradingView() {
    React.useEffect(() => {
      polls.twap += 1;
      const timer = window.setInterval(() => {
        polls.twap += 1;
      }, 20);
      return () => window.clearInterval(timer);
    }, []);
    return <div role="region" aria-label="TWAP 交易视图">TWAP_VIEW</div>;
  },
}));

vi.mock("@/components/trading/arbitrage-trading", () => ({
  ArbitrageTradingView: function ArbitrageTradingView() {
    React.useEffect(() => {
      polls.arb += 1;
      const timer = window.setInterval(() => {
        polls.arb += 1;
      }, 20);
      return () => window.clearInterval(timer);
    }, []);
    return <div role="region" aria-label="套利交易视图">ARBITRAGE_VIEW</div>;
  },
}));

import { TradingWorkspace } from "./page";

const completePrefill =
  "mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTC&legAQuote=USDT&legBExchange=okx&legBContract=perpetual&legBBase=BTC&legBQuote=USDT";

describe("TradingPage", () => {
  afterEach(() => {
    cleanup();
    navigation.params = new URLSearchParams();
    polls.manual = 0;
    polls.twap = 0;
    polls.arb = 0;
  });

  it("switches an already mounted trading page when a valid arbitrage query arrives", async () => {
    const view = render(<TradingWorkspace searchParamsOverride={new URLSearchParams()} />);
    expect(await screen.findByRole("region", { name: "手动交易视图" })).toBeTruthy();

    view.rerender(
      <TradingWorkspace searchParamsOverride={new URLSearchParams(completePrefill)} />,
    );

    await waitFor(() =>
      expect(screen.getByRole("region", { name: "套利交易视图" })).toBeTruthy(),
    );
    expect(screen.queryByRole("region", { name: "手动交易视图" })).toBeNull();
  });

  it("does not remount or leave manual when an empty query arrives", async () => {
    const view = render(<TradingWorkspace searchParamsOverride={new URLSearchParams()} />);
    expect(await screen.findByRole("region", { name: "手动交易视图" })).toBeTruthy();
    const manualPolls = polls.manual;
    view.rerender(<TradingWorkspace searchParamsOverride={new URLSearchParams()} />);
    expect(screen.getByRole("region", { name: "手动交易视图" })).toBeTruthy();
    expect(polls.manual).toBe(manualPolls);
  });

  it("only resumes the active trading view after the other two have been visited", async () => {
    render(<TradingWorkspace searchParamsOverride={new URLSearchParams()} />);
    expect(await screen.findByRole("region", { name: "手动交易视图" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /TWAP/ }));
    expect(await screen.findByRole("region", { name: "TWAP 交易视图" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /套利交易/ }));
    expect(await screen.findByRole("region", { name: "套利交易视图" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /手动交易/ }));
    expect(await screen.findByRole("region", { name: "手动交易视图" })).toBeTruthy();

    const manual = polls.manual;
    const twap = polls.twap;
    const arb = polls.arb;
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(polls.twap).toBe(twap);
    expect(polls.arb).toBe(arb);
    expect(polls.manual).toBeGreaterThan(manual);
  });
});
