import { expect, test } from "@playwright/test";

const routes = [
  "/funding",
  "/global-stocks",
  "/crypto-options",
  "/orderbook",
  "/trading",
  "/accounts",
  "/reports",
  "/ai-settings",
];

const viewports = [
  { width: 1024, height: 576 },
  { width: 1093, height: 614 },
  { width: 1280, height: 720 },
  { width: 1366, height: 768 },
  { width: 1536, height: 864 },
  { width: 1920, height: 1080 },
];

for (const viewport of viewports) {
  test.describe(`${viewport.width}x${viewport.height}`, () => {
    test.use({ viewport });

    for (const route of routes) {
      test(`${route} does not create page-level horizontal overflow`, async ({
        page,
      }) => {
        await page.goto(route, { waitUntil: "domcontentloaded" });
        const overflow = await page.evaluate(() => ({
          scrollWidth: document.documentElement.scrollWidth,
          clientWidth: document.documentElement.clientWidth,
          tableScrollers: document.querySelectorAll("[data-wide-table-scroll]")
            .length,
        }));
        expect(overflow.scrollWidth).toBe(overflow.clientWidth);
      });
    }
  });
}
