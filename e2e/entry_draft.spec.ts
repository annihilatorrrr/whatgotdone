import { expect, test } from "@playwright/test";
import { mockLoginAsUser, wipeDB } from "./helpers/test_apis.js";

test.beforeEach(async ({ page }) => {
  await wipeDB(page);
});

test("logs in and saves a draft", async ({ page }) => {
  const apiDraftGet = page.waitForRequest(
    (request) =>
      request.url().includes("/api/draft") && request.method() === "GET"
  );
  await mockLoginAsUser(page, "staging_jimmy");

  await expect(page).toHaveURL(/\/entry\/edit\/.+/g);

  // Wait for page to pull down any previous entry.
  await apiDraftGet;

  const apiDraftPut = page.waitForRequest(
    (request) =>
      request.url().includes("/api/draft") && request.method() === "PUT"
  );

  const entryText = "Saved a private draft at " + new Date().toISOString();

  await page.getByRole("textbox").clear();
  await page.getByRole("textbox").fill(entryText);

  // Wait for auto-save to complete.
  await expect(page.locator(".save-draft")).toContainText("Changes Saved");
  await apiDraftPut;

  // User should stay on the same page after saving a draft.
  await expect(page).toHaveURL(/\/entry\/edit\/.+/g);

  await page.getByRole("link", { name: "Recent" }).click();
  await expect(page).toHaveURL("/recent");

  // Private drafts should not appear on the recent page
  expect(await page.locator("#app").innerText).not.toContain(entryText);
});

test("don't overwrite draft until we successfully sync the latest draft from the server", async ({
  page,
  baseURL,
}) => {
  let apiDraftGetCalls = 0;
  let apiDraftPostCalls = 0;
  await page.route(baseURL + "/api/draft/*", (route) => {
    if (route.request().method() === "GET") {
      apiDraftGetCalls++;
      return route.fulfill({
        status: 500,
      });
    }
    if (route.request().method() === "POST") {
      apiDraftPostCalls++;
      return route.continue();
    }
  });

  await mockLoginAsUser(page, "staging_jimmy");
  await expect(page).toHaveURL(/\/entry\/edit\/.+/g);

  await expect(page.locator(".journal-markdown")).not.toBeVisible();
  await expect(page.locator(".save-draft")).not.toBeVisible();
  await expect(page.locator(".entry-form")).not.toBeVisible();

  expect(apiDraftGetCalls).toBeGreaterThan(0);
  expect(apiDraftPostCalls).toEqual(0);
});
