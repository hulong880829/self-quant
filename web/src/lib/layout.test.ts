import { describe, expect, it } from "vitest";

import {
  assistantMainPaddingPx,
  assistantSurface,
  navDensity,
  pageContentWidthPx,
  shouldDockAssistant,
  shouldUseSplitLayout,
} from "./layout";

describe("responsive layout predicates", () => {
  it("uses compact navigation below 1280px", () => {
    expect(navDensity(1093)).toBe("compact");
    expect(navDensity(1279)).toBe("compact");
    expect(navDensity(1280)).toBe("full");
    expect(navDensity(1920)).toBe("full");
  });

  it("docks the assistant only when width and height are both sufficient", () => {
    expect(shouldDockAssistant(1920, 1080)).toBe(true);
    expect(assistantSurface(1920, 1080)).toBe("dock");
    expect(shouldDockAssistant(1536, 864)).toBe(false);
    expect(shouldDockAssistant(1920, 614)).toBe(false);
    expect(shouldDockAssistant(1093, 614)).toBe(false);
    expect(assistantSurface(1536, 864)).toBe("sheet");
  });

  it("reserves assistant padding only in docked mode", () => {
    expect(assistantMainPaddingPx(false, true)).toBe(0);
    expect(assistantMainPaddingPx(true, true)).toBe(366);
    expect(assistantMainPaddingPx(true, false)).toBe(72);
  });

  it("keeps funding split off when assistant would create a third column", () => {
    const windows125 = pageContentWidthPx(1536, 864, true);
    expect(windows125).toBe(1536);
    expect(shouldUseSplitLayout(windows125, 380)).toBe(true);

    const dockedOpen = pageContentWidthPx(1600, 900, true);
    expect(dockedOpen).toBe(1234);
    expect(shouldUseSplitLayout(dockedOpen, 380)).toBe(false);

    const fullHdOpen = pageContentWidthPx(1920, 1080, true);
    expect(fullHdOpen).toBe(1554);
    expect(shouldUseSplitLayout(fullHdOpen, 380)).toBe(true);
  });
});
