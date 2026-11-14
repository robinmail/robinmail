import { defineConfig, devices } from '@playwright/test'

// The end-to-end test runs against a real robinmail with the development
// instance of contrib/dev/seed.sh (see the Makefile, target "e2e").
//
// ROBINMAIL_E2E_URL is where the webmail is: the Go server with the
// webmail built in (make e2e uses http://127.0.0.1:8080). Without it the
// test starts Vite's dev server, which proxies the API to that Go server.
const url = process.env.ROBINMAIL_E2E_URL

export default defineConfig({
  testDir: 'e2e',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  timeout: 60_000,
  use: {
    baseURL: url ?? 'http://localhost:5173',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } }],
  webServer: url
    ? undefined
    : { command: 'npm run dev', url: 'http://localhost:5173', reuseExistingServer: true, timeout: 60_000 },
})
