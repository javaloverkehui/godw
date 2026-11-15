import { test as setup, expect } from '@playwright/test';
import { login } from '../src/ui';
import { APP } from '../src/config';

/**
 * 全局登录：所有用例复用同一个 storageState。
 * 登录本身的行为在 01-auth.spec.ts 里单独验证，这里只负责「把门打开」。
 */
setup('登录一次，复用会话', async ({ page }) => {
  await login(page);
  await expect(page.locator('#version-badge')).toContainText('role:');

  const token = await page.evaluate(() => localStorage.getItem('godw.token'));
  expect(token, '登录后应写入本地 token').toBeTruthy();

  await page.context().storageState({ path: 'auth.json' });
  console.log(`[setup] 已登录 ${APP.email}，会话写入 auth.json`);
});
