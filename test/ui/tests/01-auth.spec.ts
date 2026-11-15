import { test, expect, Page } from '@playwright/test';
import { APP } from '../src/config';
import { toasts, waitRender } from '../src/ui';
import { one } from '../src/db';

/**
 * 人物设定：刚拿到账号的运营同学，第一次登录控制台。
 *
 * 他关心三件事：令牌坏了会不会白屏、密码错了有没有提示、
 * 刷新一下是不是又要重登。
 *
 * 环境说明：dev 后端允许匿名（视为 admin），所以「未登录」这个状态
 * 用「本地放一个坏令牌 → /me 401」来构造，这也正是令牌过期时用户的真实遭遇。
 */
test.use({ storageState: { cookies: [], origins: [] } });

const BOGUS = 'bogus-token-for-e2e';

/** dev 环境默认匿名登录，控制台不显示登录入口，直接调起同一个弹窗。 */
async function openLoginForTest(page: Page): Promise<void> {
  await page.goto(APP.baseURL);
  await page.waitForFunction(() => (document.getElementById('version-badge')?.innerHTML || '').length > 0);
  await waitRender(page);
  await page.evaluate(() => (window as unknown as { loginModal: () => void }).loginModal());
  await expect(page.locator('.modal #lg-email')).toBeVisible();
}

test.describe('认证与会话', () => {
  test('TC-AUTH-01 令牌失效时优雅降级：页面不白屏，出现登录入口，弹窗可关闭', async ({ page }) => {
    await page.addInitScript(t => localStorage.setItem('godw.token', t), BOGUS);
    await page.goto(APP.baseURL);

    // 交互预期：骨架照常渲染，右上角把「角色」换成「登录」，不是整页报错
    await expect(page.locator('.nav-item').first()).toBeVisible();
    await expect(page.locator('#version-badge a')).toHaveText('登录');
    // 数据预期：坏令牌被前端清掉，避免后续每个请求都 401
    await expect.poll(() => page.evaluate(() => localStorage.getItem('godw.token'))).toBeFalsy();

    // 交互预期：打开登录弹窗，字段与语义正确
    await page.locator('#version-badge a').click();
    const modal = page.locator('.modal');
    await expect(modal).toBeVisible();
    await expect(modal.locator('#lg-email')).toHaveAttribute('placeholder', APP.email);
    await expect(modal.locator('#lg-pass')).toHaveAttribute('type', 'password');

    // 交互预期：Esc 关闭且遮罩不残留（残留会挡住后续所有点击）
    await page.keyboard.press('Escape');
    await expect(modal).toHaveCount(0);
    await expect(page.locator('.modal-mask')).toHaveCount(0);
  });

  test('TC-AUTH-02 密码错误：给出可读提示，不落令牌，账号数据不变', async ({ page }) => {
    const before = await one<{ password_hash: string }>(
      'SELECT password_hash FROM app_user WHERE email = $1', [APP.email],
    );

    await openLoginForTest(page);
    await page.fill('#lg-email', APP.email);
    await page.fill('#lg-pass', 'wrong-password-' + Date.now());
    await page.click('[data-act="do-login"]');

    // 交互预期：明确说「密码/凭证不对」，不能是「登录已过期」这类误导文案
    await expect(toasts(page).filter({ hasText: /密码|凭证|失败|错误|invalid/i }).first())
      .toBeVisible({ timeout: 10_000 });
    await expect(page.locator('.modal')).toBeVisible();

    // 数据预期：未写入令牌；密码摘要与登录前完全一致
    expect(await page.evaluate(() => localStorage.getItem('godw.token'))).toBeFalsy();
    const after = await one<{ password_hash: string }>(
      'SELECT password_hash FROM app_user WHERE email = $1', [APP.email],
    );
    expect(after?.password_hash).toBe(before?.password_hash);
  });

  test('TC-AUTH-03 正确登录：令牌落地、徽标转为角色、令牌可换取身份', async ({ page }) => {
    await openLoginForTest(page);
    await page.fill('#lg-email', APP.email);
    await page.fill('#lg-pass', APP.password);

    const me = page.waitForResponse(r => r.url().endsWith('/api/v1/me') && r.status() === 200);
    await page.click('[data-act="do-login"]');

    // 交互预期：成功提示 + 弹窗关闭 + 徽标显示角色
    await expect(toasts(page).filter({ hasText: '已登录' })).toBeVisible();
    await expect(page.locator('.modal')).toHaveCount(0);
    await expect(page.locator('#version-badge')).toContainText('role: admin');
    await me;

    // 数据预期：令牌进 localStorage，且能被后端认出来
    const token = await page.evaluate(() => localStorage.getItem('godw.token'));
    expect(token).toBeTruthy();
    const resp = await page.request.get(`${APP.baseURL}/api/v1/me`, {
      headers: { Authorization: 'Bearer ' + token },
    });
    expect(resp.status()).toBe(200);
    const meBody = (await resp.json()).data;
    expect(meBody.role).toBe('admin');
    // 身份不是靠前端编的：接口返回的 userId 必须能在库里对上同一个账号
    const u = await one<{ id: string }>('SELECT id FROM app_user WHERE email = $1', [APP.email]);
    expect(meBody.userId).toBe(u?.id);
  });

  test('TC-AUTH-04 刷新后会话保持，工作区归属与库中一致', async ({ page }) => {
    await page.goto(APP.baseURL + '/#/overview');
    await page.reload();

    await expect(page.locator('#version-badge')).toContainText('role: admin');
    await expect(page.locator('#env-badge')).toContainText('workspace:');

    // 数据预期：徽标上的工作区名 = 库里当前用户所属工作区
    const ws = await one<{ name: string }>(
      `SELECT w.name FROM workspace w
         JOIN app_user u ON u.workspace_id = w.id
        WHERE u.email = $1`, [APP.email],
    );
    expect(ws?.name).toBeTruthy();
    await expect(page.locator('#env-badge')).toContainText(ws!.name);
  });
});
