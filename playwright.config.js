const { defineConfig } = require("@playwright/test");

const token = "playwright-browser-token-0123456789";

module.exports = defineConfig({
  testDir: "./tests/browser",
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  timeout: 30_000,
  expect: { timeout: 7_500 },
  reporter: process.env.CI
    ? [["line"], ["html", { open: "never" }]]
    : [["list"], ["html", { open: "never" }]],
  use: {
    baseURL: "http://127.0.0.1:17337",
    browserName: "chromium",
    channel: process.env.CI ? undefined : "chromium",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "retain-on-failure"
  },
  webServer: {
    command: "node tests/browser/fixture-server.js",
    url: "http://127.0.0.1:17337/",
    timeout: 120_000,
    reuseExistingServer: false,
    env: {
      ...process.env,
      EWASD_BROWSER_TOKEN: token
    }
  },
  projects: [
    {
      name: "desktop-standard",
      use: {
        viewport: { width: 1440, height: 1000 },
        deviceScaleFactor: 1
      }
    },
    {
      name: "desktop-1440p",
      use: {
        viewport: { width: 2560, height: 1440 },
        deviceScaleFactor: 1
      }
    }
  ]
});
