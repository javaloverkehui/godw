import { expect, Page } from '@playwright/test';
import { APP } from './config';

/**
 * 与控制台交互的公共底座。
 *
 * 选择器策略：优先 `data-act`（前端事件委托的动作名）与可见文案，
 * 不用 nth-child / 绝对路径——那些一改布局就全红，且报错信息毫无指向性。
 * 所有选择器都收在 page object 里，UI 改版只改一处。
 */

/**
 * 登录。
 *
 * 注意：dev 环境后端允许匿名（且视为 admin），所以控制台默认不显示登录入口。
 * 为了拿到真实令牌、让审计日志记到具体账号，这里在缺少入口时直接打开登录弹窗。
 */
export async function login(page: Page): Promise<void> {
  await page.goto(APP.baseURL, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => (document.getElementById('version-badge')?.innerHTML || '').length > 0);
  // 首屏 render 开始时会把弹窗清空；等它渲染完再开登录框，否则刚开就被收掉
  await waitRender(page);

  if (await page.locator('#version-badge a').count()) {
    await page.locator('#version-badge a').click();
  } else if (!(await page.locator('.modal').count())) {
    await page.evaluate(() => (window as unknown as { loginModal: () => void }).loginModal());
  }
  await expect(page.locator('.modal #lg-email')).toBeVisible();

  await page.fill('#lg-email', APP.email);
  await page.fill('#lg-pass', APP.password);
  await page.click('[data-act="do-login"]');
  await expect(toasts(page).filter({ hasText: '已登录' }).first()).toBeVisible({ timeout: 10_000 });
  await page.waitForFunction(() => !!localStorage.getItem('godw.token'));
}

/** 未登录时打开登录弹窗（登录页没有独立路由，入口在右上角的版本徽标里）。 */
export async function openLoginModal(page: Page): Promise<void> {
  await page.goto(APP.baseURL, { waitUntil: 'domcontentloaded' });
  await page.locator('#version-badge a').click();
  await expect(page.locator('.modal')).toBeVisible();
}

/** 切换路由并等待首屏渲染完成（渲染是异步的，直接断言会读到「加载中…」）。 */
export async function gotoRoute(page: Page, route: string): Promise<void> {
  const target = `${APP.baseURL}/#/${route}`;
  // 已经在同一个 hash 上时 goto 不会触发导航，页面数据还是旧的（刚在弹窗里建完对象尤其明显）；
  // 这种时候显式 reload，保证每个 page object 的 open() 都拿到最新数据。
  if (page.url() === target) {
    await page.reload({ waitUntil: 'domcontentloaded' });
  } else {
    await page.goto(target, { waitUntil: 'domcontentloaded' });
  }
  await waitRender(page);
}

/** 不重载页面，只改 hash：用于验证「切页时清弹窗/抽屉」这类纯前端行为。 */
export async function navigate(page: Page, route: string): Promise<void> {
  await page.evaluate(r => { location.hash = '#/' + r; }, route);
  await waitRender(page);
}

export async function waitRender(page: Page): Promise<void> {
  await page.waitForFunction(() => {
    const v = document.getElementById('view');
    if (!v) return false;
    const text = (v.innerText || '').trim();
    return text.length > 0 && text !== '加载中…';
  }, undefined, { timeout: 15_000 });
}

/** 网络层断言：某个接口返回了什么，比只看页面更能定位问题。 */
export function waitApi(page: Page, path: string, method = 'POST') {
  return page.waitForResponse(r => r.url().includes(`/api/v1${path}`) && r.request().method() === method);
}

/** 页面上所有的 toast 文案。 */
export function toasts(page: Page) {
  return page.locator('#toast-root .toast');
}

export async function expectToast(page: Page, text: string | RegExp, timeout = 10_000): Promise<void> {
  await expect(toasts(page).filter({ hasText: text }).first()).toBeVisible({ timeout });
}

/** 确认类弹窗（原生 confirm）统一接受：测试要验证的是「删除后的结果」，不是浏览器对话框。 */
export function acceptDialogs(page: Page): void {
  page.on('dialog', d => d.accept());
}

/** 关闭可能残留的弹窗，避免它挡住下一次点击。 */
export async function closeModal(page: Page): Promise<void> {
  const modal = page.locator('#modal-root .modal');
  if (await modal.count()) await page.keyboard.press('Escape');
}

/** 读表格：返回表头与每行单元格文本，便于整表级断言。 */
export async function readTable(page: Page, scope = '#view'): Promise<{ headers: string[]; rows: string[][] }> {
  const table = page.locator(scope).locator('table').first();
  await expect(table).toBeVisible();
  const headers = await table.locator('thead th').allInnerTexts();
  const rows: string[][] = [];
  for (const row of await table.locator('tbody tr').all()) {
    rows.push((await row.locator('td').allInnerTexts()).map(s => s.replace(/\s+/g, ' ').trim()));
  }
  return { headers: headers.map(s => s.replace(/\s+/g, ' ').trim()), rows };
}

/** 按可见文案找表格行。 */
export function rowByText(page: Page, text: string) {
  return page.locator('#view tbody tr').filter({ hasText: text });
}

/** 侧栏进入某个页面（验证导航本身，而不是直接改 URL）。 */
export async function clickNav(page: Page, label: string): Promise<void> {
  await page.locator('.nav-item').filter({ hasText: label }).click();
  await waitRender(page);
}
