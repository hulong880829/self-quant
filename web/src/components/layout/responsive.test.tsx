// @vitest-environment jsdom

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import {
  PageFrame,
  WideTableScroll,
  WorkspacePanel,
} from "./responsive";

describe("layout containers", () => {
  afterEach(() => {
    cleanup();
  });

  it("keeps page frames from forcing horizontal overflow", () => {
    const { container } = render(
      <div data-page-root className="min-w-0 max-w-full overflow-x-clip">
        <PageFrame>
          <WideTableScroll>
            <table className="min-w-[1420px]">
              <tbody>
                <tr>
                  <td>wide</td>
                </tr>
              </tbody>
            </table>
          </WideTableScroll>
        </PageFrame>
      </div>,
    );

    const root = container.querySelector("[data-page-root]");
    const frame = container.querySelector("[data-page-frame]");
    const scroller = container.querySelector("[data-wide-table-scroll]");
    expect(root?.className).toContain("overflow-x-clip");
    expect(frame?.className).toContain("min-w-0");
    expect(scroller?.className).toContain("overflow-auto");
    expect(scroller?.querySelector("table")?.className).toContain("min-w-[1420px]");
  });

  it("applies dvh-based panel height instead of a fixed 620px floor", () => {
    const { container } = render(<WorkspacePanel>panel</WorkspacePanel>);
    const panel = container.querySelector("[data-workspace-panel]");
    expect(panel?.className).toContain("h-[var(--app-panel-height)]");
    expect(panel?.className).not.toContain("min-h-[620px]");
  });
});
