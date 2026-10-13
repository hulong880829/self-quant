import { expect, test } from "@playwright/test";

const viewports = [
  { width: 1280, height: 720 },
  { width: 375, height: 667 },
];

for (const viewport of viewports) {
  test.describe(`jev-arb ${viewport.width}x${viewport.height}`, () => {
    test.use({ viewport });

    test("does not overflow and keeps trade legs readable", async ({ page }) => {
      await page.goto("/funding/jev-arb", { waitUntil: "domcontentloaded" });
      const overflow = await page.evaluate(() => ({
        scrollWidth: document.documentElement.scrollWidth,
        clientWidth: document.documentElement.clientWidth,
      }));
      expect(overflow.scrollWidth).toBeLessThanOrEqual(overflow.clientWidth + 1);

      const heading = page.getByRole("heading", { name: "JEV 纸面套利" });
      const loginGate = page.getByRole("heading", { name: "需要登录" });
      if (await heading.count()) {
        await expect(heading).toBeVisible();
        await expect(page.getByText("成交")).toBeVisible();
        const overlapping = await page.evaluate(() => {
          const cells = [...document.querySelectorAll("td")];
          return cells.some((cell) => cell.scrollWidth > cell.clientWidth + 2);
        });
        expect(overlapping).toBe(false);
      } else {
        await expect(loginGate).toBeVisible();
      }
    });
  });
}
