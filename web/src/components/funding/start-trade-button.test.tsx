// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  requireAuth: vi.fn(),
  push: vi.fn(),
  createIdempotencyKey: vi.fn(),
}));

vi.mock("@/components/auth/auth-provider", () => ({
  useAuth: () => ({
    status: "anonymous",
    requireAuth: mocks.requireAuth,
  }),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: mocks.push }),
}));

vi.mock("@/lib/idempotency-key", () => ({
  createIdempotencyKey: mocks.createIdempotencyKey,
}));

import { StartTradeButton } from "./start-trade-button";

const href =
  "/trading?mode=arbitrage&legAExchange=binance&legAContract=spot&legABase=BTW&legAQuote=USDT";

function prefillHref(requestId: string) {
  const url = new URL(href, "https://selfquant.invalid");
  url.searchParams.set("prefillRequestId", requestId);
  return `${url.pathname}${url.search}`;
}

describe("StartTradeButton", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("opens login with the request-id URL and does not navigate when unauthenticated", () => {
    mocks.createIdempotencyKey.mockReturnValue("req-login");
    mocks.requireAuth.mockReturnValue(false);
    render(<StartTradeButton href={href} />);
    fireEvent.click(screen.getByRole("button", { name: "开启交易" }));
    const nextHref = prefillHref("req-login");
    expect(mocks.requireAuth).toHaveBeenCalledWith(nextHref);
    expect(mocks.push).not.toHaveBeenCalled();
    expect(nextHref).toContain("prefillRequestId=req-login");
  });

  it("navigates and authenticates with the same prefillRequestId URL", () => {
    mocks.createIdempotencyKey.mockReturnValue("req-nav");
    mocks.requireAuth.mockReturnValue(true);
    render(<StartTradeButton href={href} />);
    fireEvent.click(screen.getByRole("button", { name: "开启交易" }));
    const nextHref = prefillHref("req-nav");
    expect(mocks.requireAuth).toHaveBeenCalledWith(nextHref);
    expect(mocks.push).toHaveBeenCalledWith(nextHref);
  });

  it("assigns a new prefillRequestId on each click", () => {
    mocks.createIdempotencyKey
      .mockReturnValueOnce("req-1")
      .mockReturnValueOnce("req-2");
    mocks.requireAuth.mockReturnValue(true);
    render(<StartTradeButton href={href} />);
    const button = screen.getByRole("button", { name: "开启交易" });
    fireEvent.click(button);
    fireEvent.click(button);
    expect(mocks.push.mock.calls[0]?.[0]).toBe(prefillHref("req-1"));
    expect(mocks.push.mock.calls[1]?.[0]).toBe(prefillHref("req-2"));
  });
});
