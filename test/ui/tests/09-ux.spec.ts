import { test, expect } from '@playwright/test';
import { gotoRoute, waitRender } from '../src/ui';
import { APP } from '../src/config';

/**
 * 人物设定：每天要在十几个页面之间来回跳的开发者。
 *
 * 这些用例不查数据，只盯「手感和可靠性」：每个入口点得开、刷新不迷路、
 * 切页不残留浮层、快捷键能省一次鼠标。这类问题不冒烟，但会天天磨人。
 */

const NAV = [
  '工作台', '运行实例', '运维中心', '平台运维',
  '数据源', '表结构', '模型设计', '管道', '编排', '指标', 'BI 图表', '看板', '自助查询',
  '数据文档', '血缘', '质量', '治理', '审计',
];

test.describe('交互与可用性', () => {
  test('TC-UX-01 侧栏每个入口都能打开，且不出现错误块', async ({ page }) => {
    await gotoRoute(page, 'overview');

    for (const label of NAV) {
      await page.locator('.nav-item').filter({ hasText: label }).click();
      await waitRender(page);

      // 交互预期：面包屑非空、主区域有内容、没有报错卡片
      await expect(page.locator('#crumb')).not.toBeEmpty();
      await expect(page.locator('#view .err-box'), `${label} 页不应出现错误块`).toHaveCount(0);
      await expect(
        page.locator('.nav-item.active'),
        `${label} 应处于选中态`,
      ).toContainText(label);
    }
  });

  test('TC-UX-02 主题切换写入本地，刷新后保持', async ({ page }) => {
    await gotoRoute(page, 'overview');
    const html = page.locator('html');
    const before = await html.getAttribute('data-theme');

    await page.click('#theme-btn');
    const flipped = before === 'light' ? 'dark' : 'light';
    await expect(html).toHaveAttribute('data-theme', flipped);
    // 按钮文案跟着变，否则用户不知道自己刚切到哪
    await expect(page.locator('#theme-btn')).toContainText(flipped === 'light' ? '暗色' : '亮色');

    await page.reload();
    await expect(html).toHaveAttribute('data-theme', flipped);
    expect(await page.evaluate(() => localStorage.getItem('godw.theme'))).toBe(flipped);

    // 复原，避免影响后续用例的截图与视觉断言
    await page.click('#theme-btn');
    await expect(html).toHaveAttribute('data-theme', before!);
  });

  test('TC-UX-03 深链接刷新后仍停在原页面，侧栏高亮正确', async ({ page }) => {
    await page.goto(`${APP.baseURL}/#/quality`);
    await page.reload();
    await waitRender(page);

    await expect(page.locator('.page-title')).toHaveText('质量');
    await expect(page.locator('.nav-item.active')).toContainText('质量');
    await expect(page).toHaveURL(/#\/quality$/);
  });

  test('TC-UX-04 浏览器后退切页时自动收起弹窗，不留遮罩挡住新页面', async ({ page }) => {
    await gotoRoute(page, 'overview');
    await navTo(page, '数据源');
    await page.click('[data-act="new-ds"]');
    await expect(page.locator('.modal-mask')).toBeVisible();

    // 弹窗遮罩盖住了侧栏，真实用户此时只能按浏览器后退
    await page.goBack();
    await waitRender(page);

    await expect(page.locator('.modal-mask')).toHaveCount(0);
    await expect(page.locator('.page-title')).toBeHidden();
    await expect(page.locator('#crumb')).toContainText('工作台');
    // 遮罩消失后页面必须真的可交互
    await expect(page.locator('.nav-item').filter({ hasText: '数据源' })).toBeEnabled();
  });

  test('TC-UX-05 Ctrl+K 唤起全局搜索，命中后能直接跳转', async ({ page }) => {
    await gotoRoute(page, 'overview');

    await page.keyboard.press('Control+k');
    const input = page.locator('#gs-input');
    await expect(input).toBeFocused();

    await input.fill('dwd_order');
    const hit = page.locator('.search-hit').filter({ hasText: 'dwd_order' }).first();
    await expect(hit).toBeVisible({ timeout: 10_000 });

    await hit.click();
    await waitRender(page);
    // 交互预期：跳转并打开该模型的详情，不是把名字填进搜索框就完事
    await expect(page.locator('.search-hit')).toHaveCount(0);
    await expect(page.locator('#view')).toContainText('dwd_order');
    await expect(page.locator('#view .err-box')).toHaveCount(0);
  });

  test('TC-UX-06 搜索无结果时给出空态提示，而不是空白浮层', async ({ page }) => {
    await gotoRoute(page, 'overview');
    await page.click('#gs-input');
    await page.fill('#gs-input', '绝不可能存在的表_' + Date.now());

    await expect(page.locator('#gs-pop')).toContainText('没有匹配', { timeout: 10_000 });
    await page.keyboard.press('Escape');
  });
});

/** 用侧栏切换（不是改 hash）：验证的是「用户点导航」这条路径。 */
async function navTo(page: import('@playwright/test').Page, label: string): Promise<void> {
  await page.locator('.nav-item').filter({ hasText: label }).click();
  await waitRender(page);
}
