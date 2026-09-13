// @vitest-environment jsdom

import * as React from "react";
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { PolymarketPriceChart } from "./price-chart";

afterEach(cleanup);

describe("PolymarketPriceChart", () => {
  it("excludes a stale fair point from the active market window", () => {
    const staleWallNS = (
      BigInt(new Date("2026-08-24T09:59:59Z").getTime()) * 1_000_000n
    ).toString();
    const { container } = render(
      <PolymarketPriceChart
        points={[
          {
            timestamp: "2026-08-24T10:36:00Z",
            openPrice: 77_951,
            chainlinkPrice: 77_960,
          },
          {
            timestamp: "2026-08-24T10:37:00Z",
            openPrice: 77_951,
            chainlinkPrice: 77_964,
          },
        ]}
        fairPricePoints={[{
          timestamp: "2026-08-24T10:37:00Z",
          sourceWallNS: staleWallNS,
          ringEpoch: "1",
          sequence: "1",
          modelId: "fp-v1",
          price: 77_554,
          degraded: true,
          degradedReasons: ["book_crossed"],
        }]}
        openPrice={77_951}
        windowStart="2026-08-24T10:35:00Z"
        windowEnd="2026-08-24T10:40:00Z"
      />,
    );

    expect(container.querySelectorAll("circle")).toHaveLength(1);
    expect(container.textContent).toContain("Fair Price（待同步）");
    expect(container.textContent).not.toContain("77,554");
  });

  it("connects fair price points across sampling gaps", () => {
    const fairPoint = (timestamp: string, sequence: string, price: number) => ({
      timestamp,
      sourceWallNS: (
        BigInt(new Date(timestamp).getTime()) * 1_000_000n
      ).toString(),
      ringEpoch: "1",
      sequence,
      modelId: "fp-v1",
      price,
      degraded: false,
      degradedReasons: [],
    });
    const { container } = render(
      <PolymarketPriceChart
        points={[]}
        fairPricePoints={[
          fairPoint("2026-08-24T10:36:00Z", "1", 77_950),
          fairPoint("2026-08-24T10:36:10Z", "2", 77_960),
        ]}
        openPrice={77_951}
        windowStart="2026-08-24T10:35:00Z"
        windowEnd="2026-08-24T10:40:00Z"
      />,
    );

    const fairPath = container.querySelector(
      'path[stroke="var(--chart-3)"]',
    );
    expect(fairPath?.getAttribute("d")).toMatch(/^M .+ L /);
  });
});
