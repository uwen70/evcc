import { test, expect, type Page } from "@playwright/test";
import { start, stop, baseUrl } from "./evcc";
import { enableExperimental, expectModalHidden, expectModalVisible } from "./utils";

test.use({ baseURL: baseUrl() });

test.afterEach(async () => {
  await stop();
});

const SELECT_ALL = "ControlOrMeta+KeyA";

async function goToConfig(page: Page) {
  await page.goto("/#/config");
  await enableExperimental(page);
}

test.describe("messaging", async () => {
  test("save a comment", async ({ page }) => {
    await start();
    await goToConfig(page);

    await page.getByTestId("messaging").getByRole("button", { name: "edit" }).click();
    const modal = await page.getByTestId("messaging-modal");
    await expectModalVisible(modal);

    await modal.locator(".monaco-editor .view-line").nth(0).click();
    for (let i = 0; i < 4; i++) {
      await page.keyboard.press(SELECT_ALL, { delay: 10 });
      await page.keyboard.press("Backspace", { delay: 10 });
    }
    await page.keyboard.type("# hello world");
    await page.getByRole("button", { name: "Save" }).click();
    await expectModalHidden(modal);

    page.reload();

    await page.getByTestId("messaging").getByRole("button", { name: "edit" }).click();
    await expectModalVisible(modal);
    await expect(modal).toContainText("# hello world");
  });
});
