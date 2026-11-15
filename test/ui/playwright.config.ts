import { defineConfig } from '@playwright/test';
import { APP } from './src/config';

/**
 * UI 自动化配置。
 *
 * workers=1：用例共用同一套元数据库与同一个登录态，
 * 并行会把「我新建的数据源」跑到别人的断言里，收益远小于噪声。
 */
export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: [
    ['list'],
    ['html', { outputFolder: 'reports/html', open: 'never' }],
  ],
  use: {
    baseURL: APP.baseURL,
    channel: 'chrome',          // 用本机 Chrome，不额外下载浏览器
    headless: true,
    viewport: { width: 1600, height: 1000 },
    actionTimeout: 10_000,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'setup', testMatch: /auth\.setup\.ts/ },
    { name: 'ui', dependencies: ['setup'], use: { storageState: 'auth.json' } },
  ],
});
