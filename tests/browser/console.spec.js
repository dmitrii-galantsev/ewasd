"use strict";

const fs = require("node:fs");
const path = require("node:path");
const AxeBuilder = require("@axe-core/playwright").default;
const { test, expect } = require("@playwright/test");

const token = "playwright-browser-token-0123456789";

test.describe.configure({ mode: "serial" });

async function pair(page) {
  const consoleErrors = [];
  page.on("console", message => {
    if (message.type() === "error" || message.type() === "warning") {
      consoleErrors.push(`${message.type()}: ${message.text()}`);
    }
  });
  page.on("pageerror", error => consoleErrors.push(`pageerror: ${error.message}`));
  await page.goto(`/?token=${token}`);
  await expect(page).toHaveURL("/");
  await expect(page.getByRole("heading", { name: "Browser Fixture", level: 2 })).toBeVisible();
  return consoleErrors;
}

async function snapshot(page) {
  return page.evaluate(async () => {
    const response = await fetch("/api/v1/snapshot", {
      headers: { Authorization: `Bearer ${sessionStorage.getItem("ewasd-token")}` }
    });
    if (!response.ok) throw new Error(`snapshot failed: ${response.status}`);
    return response.json();
  });
}

test("target viewport has no overflow, undersized controls, console errors, or serious accessibility findings", async ({ page }, testInfo) => {
  const consoleErrors = await pair(page);

  const layout = await page.evaluate(() => {
    const controls = [...document.querySelectorAll("button, input, select")]
      .filter(element => {
        const rect = element.getBoundingClientRect();
        const style = getComputedStyle(element);
        return rect.width > 0 && rect.height > 0 && style.display !== "none" && style.visibility !== "hidden";
      })
      .map(element => {
        const rect = element.getBoundingClientRect();
        return {
          label: (element.innerText || element.ariaLabel || element.value || element.tagName).trim(),
          width: Math.round(rect.width),
          height: Math.round(rect.height)
        };
      });
    return {
      viewport: { width: innerWidth, height: innerHeight },
      horizontalOverflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      undersizedControls: controls.filter(control => control.width < 44 || control.height < 44),
      sidebar: getComputedStyle(document.querySelector(".sidebar")).display
    };
  });

  expect(layout.horizontalOverflow).toBe(false);
  expect(layout.undersizedControls).toEqual([]);
  expect(layout.sidebar).toBe("flex");

  const accessibility = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
    .analyze();
  expect(accessibility.violations.filter(item => item.impact === "critical" || item.impact === "serious")).toEqual([]);
  expect(consoleErrors).toEqual([]);

  const screenshotDir = path.join(process.cwd(), "browser-artifacts", "responsive");
  fs.mkdirSync(screenshotDir, { recursive: true });
  await page.screenshot({ path: path.join(screenshotDir, `${testInfo.project.name}.png`), fullPage: true });
});

test("activity, safety, and adoption dialog remain responsive and accessible", async ({ page }) => {
  const consoleErrors = await pair(page);
  for (const destination of ["Activity", "Safety"]) {
    await page.getByRole("button", { name: destination, exact: true }).click();
    const accessibility = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
      .analyze();
    expect(accessibility.violations.filter(item => item.impact === "critical" || item.impact === "serious")).toEqual([]);
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
    expect(overflow).toBe(false);
  }

  await page.locator('[data-view="projects"]:visible').click();
  await page.getByRole("button", { name: "Adopt path" }).click();
  const dialog = page.getByRole("dialog", { name: "Preview adoption" });
  await expect(dialog).toBeVisible();
  const dialogAccessibility = await new AxeBuilder({ page })
    .include("#entry-dialog")
    .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
    .analyze();
  expect(dialogAccessibility.violations.filter(item => item.impact === "critical" || item.impact === "serious")).toEqual([]);
  expect(consoleErrors).toEqual([]);
});

test("an already-managed path produces a blocked plan with no apply action", async ({ page }) => {
  await pair(page);
  await page.getByRole("button", { name: "Adopt path" }).click();
  await page.getByRole("textbox", { name: /Relative path/ }).fill("AGENT.md");
  await page.getByRole("button", { name: "Build read-only plan" }).click();
  const blocked = page.getByRole("dialog", { name: "No changes needed" });
  await expect(blocked).toContainText("already managed and healthy");
  await expect(blocked.getByRole("button", { name: "No write needed" })).toBeDisabled();
});

test("adoption requires a visible plan and updates activity only after verified apply", async ({ page }, testInfo) => {
  await pair(page);
  const relativePath = `adopt-${testInfo.project.name}.txt`;
  await page.getByRole("button", { name: "Adopt path" }).click();
  await page.getByRole("textbox", { name: /Relative path/ }).fill(relativePath);
  await page.getByRole("button", { name: "Build read-only plan" }).click();

  const plan = page.getByRole("dialog", { name: "Review before applying" });
  await expect(plan).toContainText(`copy · ${relativePath}`);
  await expect(plan).toContainText("normal files, directories, and foreign links are never overwritten");
  await plan.getByRole("button", { name: "Apply adopt" }).click();

  await expect(page.getByText(relativePath, { exact: true })).toBeVisible();
  await expect(page.getByText(`Adopted ${relativePath}`, { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Activity" }).click();
  await expect(page.getByText("Adopted local content with a rollback backup").first()).toBeVisible();
});

test("a reviewed reconcile plan is rejected when the filesystem step set changes", async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== "desktop-1440p", "single shared fixture mutation");
  await pair(page);
  const state = await snapshot(page);
  const project = state.projects.find(candidate => candidate.name === "Browser Fixture");
  const agent = path.join(project.root, "AGENT.md");
  const claude = path.join(project.root, ".claude");

  fs.unlinkSync(agent);
  await page.reload();
  await expect(page.getByRole("button", { name: "Reconcile 1" })).toBeVisible();
  await page.getByRole("button", { name: "Reconcile 1" }).click();
  await expect(page.getByRole("dialog", { name: "Review before applying" })).toContainText("link · AGENT.md");

  fs.unlinkSync(claude);
  await page.getByRole("button", { name: "Apply reconcile" }).click();
  await expect(page.getByRole("alert")).toContainText("filesystem preconditions changed after review");
  expect(fs.existsSync(agent)).toBe(false);
  expect(fs.existsSync(claude)).toBe(false);
});
