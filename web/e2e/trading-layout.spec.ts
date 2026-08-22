import { expect, test } from "@playwright/test";

const viewports = [
  { width: 1024, height: 576 },
  { width: 1280, height: 720 },
];

for (const viewport of viewports) {
  test.describe(`trading ${viewport.width}x${viewport.height}`, () => {
    test.use({ viewport });

    test("does not overflow horizontally and keeps a vertical scroller", async ({ page }) => {
      await page.goto("/trading", { waitUntil: "domcontentloaded" });
      const overflow = await page.evaluate(() => ({
        scrollWidth: document.documentElement.scrollWidth,
        clientWidth: document.documentElement.clientWidth,
      }));
      expect(overflow.scrollWidth).toBe(overflow.clientWidth);

      const scroller = page.locator("[data-trading-scroll]");
      const loginGate = page.getByRole("heading", { name: "需要登录" });
      if (await scroller.count()) {
        await expect(scroller).toHaveCSS("overflow-y", /auto|scroll/);
        await expect(scroller.getByText("手动交易")).toBeVisible();
        await expect(scroller.getByRole("button", { name: "当前委托" })).toBeVisible();
        await expect(scroller.getByRole("button", { name: "订单历史" })).toBeVisible();
      } else {
        await expect(loginGate).toBeVisible();
      }
    });
  });
}
