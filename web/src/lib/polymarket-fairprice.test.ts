import { describe, expect, it } from "vitest";

import {
  isFairPriceStale,
  mergeFairPricePoints,
  sameFairPriceSequence,
} from "./polymarket-fairprice";
import type { PolymarketFairPricePoint } from "@/types/polymarket";

function point(
  timestamp: string,
  sourceWallNS: string,
  sequence: string,
  modelId = "fp-v1:a",
): PolymarketFairPricePoint {
  return {
    timestamp,
    sourceWallNS,
    ringEpoch: "1",
    sequence,
    modelId,
    price: 100,
    degraded: false,
    degradedReasons: [],
  };
}

describe("Polymarket fair price series", () => {
  it("keeps the newest source in each one-second bucket", () => {
    const older = point("2026-08-13T13:40:01.100Z", "100", "1");
    const newer = point("2026-08-13T13:40:01.900Z", "200", "2");
    expect(mergeFairPricePoints([older], [newer])).toEqual([newer]);
  });

  it("does not connect different model versions", () => {
    const oldModel = point("2026-08-13T13:40:01Z", "100", "1", "fp-v1:a");
    const newModel = point("2026-08-13T13:40:02Z", "200", "2", "fp-v1:b");
    expect(mergeFairPricePoints([oldModel], [newModel])).toEqual([newModel]);
  });

  it("deduplicates heartbeat identity by epoch and sequence", () => {
    const current = point("2026-08-13T13:40:01Z", "100", "7");
    expect(sameFairPriceSequence(current, {
      ...current,
      timestamp: "2026-08-13T13:40:02Z",
    })).toBe(true);
    expect(sameFairPriceSequence(current, {
      ...current,
      ringEpoch: "2",
      sequence: "1",
    })).toBe(false);
  });

  it("filters merged points by source wall time", () => {
    const start = "2026-08-24T10:35:00Z";
    const end = "2026-08-24T10:40:00Z";
    const stale = point(
      "2026-08-24T10:36:00Z",
      (BigInt(new Date("2026-08-24T09:59:59Z").getTime()) * 1_000_000n).toString(),
      "1",
    );
    const fresh = point(
      "2026-08-24T10:36:01Z",
      (BigInt(new Date("2026-08-24T10:36:01Z").getTime()) * 1_000_000n).toString(),
      "2",
    );
    expect(mergeFairPricePoints(
      [stale],
      [fresh],
      2000,
      { start, end, nowMs: new Date(end).getTime() },
    )).toEqual([fresh]);
  });

  it("marks a fair point stale from its source wall time", () => {
    const now = new Date("2026-08-24T10:36:20Z").getTime();
    const old = point(
      "2026-08-24T10:36:19Z",
      (BigInt(now - 16_000) * 1_000_000n).toString(),
      "1",
    );
    expect(isFairPriceStale(old, now)).toBe(true);
  });

  it("allows small browser and source clock skew", () => {
    const now = new Date("2026-08-24T10:36:20Z").getTime();
    const withinTolerance = point(
      "2026-08-24T10:36:24Z",
      (BigInt(now + 4_000) * 1_000_000n).toString(),
      "1",
    );
    const beyondTolerance = point(
      "2026-08-24T10:36:26Z",
      (BigInt(now + 6_000) * 1_000_000n).toString(),
      "2",
    );
    expect(isFairPriceStale(withinTolerance, now)).toBe(false);
    expect(isFairPriceStale(beyondTolerance, now)).toBe(true);
  });
});
