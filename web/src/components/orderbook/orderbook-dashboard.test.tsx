// @vitest-environment jsdom

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { SpreadChart } from "./orderbook-dashboard";

afterEach(cleanup);

describe("SpreadChart", () => {
  it("positions points by real time, labels Beijing time, and breaks gaps", () => {
    const startMs = Date.UTC(2026, 7, 15, 0);
    const endMs = Date.UTC(2026, 7, 15, 4);
    const { container } = render(
      <SpreadChart
        startMs={startMs}
        endMs={endMs}
        points={[
          { timestamp: "2026-08-15T00:00:00.000Z", spreadBps: 1 },
          { timestamp: "2026-08-15T01:00:00.000Z", spreadBps: 2 },
          { timestamp: "2026-08-15T02:00:00.000Z", spreadBps: null },
          { timestamp: "2026-08-15T03:00:00.000Z", spreadBps: 1.5 },
        ]}
      />,
    );

    expect(container.textContent).toContain("08:00");
    expect(container.textContent).toContain("12:00");
    expect(
      container.querySelectorAll('path[stroke="var(--primary)"]'),
    ).toHaveLength(2);
  });
});
