import { defineConfig, devices } from '@playwright/test'

// The browser tests run against a real robinmail with the webmail inside,
// on an instance seeded like the development one: contrib/dev/e2e.sh starts
// it and sets ROBINMAIL_E2E_URL ("make e2e").
const url = process.env.ROBINMAIL_E2E_URL ?? 'http://127.0.0.1:8085'

export default defineConfig({
  testDir: 'specs',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  timeout: 60_000,
  use: {
    baseURL: url,
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } }],
})
