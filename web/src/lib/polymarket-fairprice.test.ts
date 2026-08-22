import { describe, expect, it } from "vitest";

import {
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
});
