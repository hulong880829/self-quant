// @vitest-environment jsdom

import * as React from "react";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useVisibleMeasure } from "./visible-measure";

function Probe({
  measure,
  enabled,
}: {
  measure: () => void;
  enabled: boolean;
}) {
  useVisibleMeasure(measure, enabled);
  return null;
}

describe("useVisibleMeasure", () => {
  afterEach(cleanup);

  it("measures the visible list once and coalesces repeated rAF schedules", async () => {
    const measure = vi.fn();
    const view = render(<Probe measure={measure} enabled />);
    expect(measure).not.toHaveBeenCalled();
    await act(async () => {
      await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    });
    expect(measure).toHaveBeenCalledTimes(1);

    view.rerender(<Probe measure={measure} enabled />);
    await act(async () => {
      await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    });
    expect(measure).toHaveBeenCalledTimes(1);

    view.rerender(<Probe measure={measure} enabled={false} />);
    view.rerender(<Probe measure={measure} enabled />);
    view.rerender(<Probe measure={measure} enabled />);
    await act(async () => {
      await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    });
    expect(measure).toHaveBeenCalledTimes(2);
  });
});
