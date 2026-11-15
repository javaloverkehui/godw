import { test, expect } from '@playwright/test';
import { GovernancePage } from '../src/pages';
import { acceptDialogs, expectToast } from '../src/ui';
import { count, one, sql, waitForDb, workspaceId } from '../src/db';
import { APP, PREFIX } from '../src/config';

/**
 * 人物设定：数据安全负责人和业务方。
 *
 * 负责人要确认三件事：策略真的存下来了、分享出去的页面真的没有内部导航、
 * 每一步写操作都能在审计里查到人。业务方只有一个诉求：点开链接就能看，不用登录。
 */
const TABLE = `${PREFIX}_tbl`;
const SHARE_TARGET = 'gmv';

test.describe('治理：策略、分享与审计', () => {
  test.beforeEach(async ({ page }) => acceptDialogs(page));

  test('TC-GOV-01 新增列级脱敏策略：页面提示、列表可见、库里字段级落库', async ({ page }) => {
    const ws = await workspaceId();
    const gov = new GovernancePage(page);
    await gov.open();

    await gov.addColumnPolicy({ table: TABLE, column: 'phone', classification: 'secret', mask: 'phone' });
    await expectToast(page, '列策略已保存');

    const row = await one<{ classification: string; mask_type: string }>(
      'SELECT classification, mask_type FROM column_policy WHERE workspace_id = $1 AND table_name = $2 AND column_name = $3',
      [ws, TABLE, 'phone'],
    );
    expect(row, '列策略应落库').toBeTruthy();
    expect(row!.classification).toBe('secret');
    expect(row!.mask_type).toBe('phone');

    const shown = page.locator('#view tbody tr').filter({ hasText: TABLE });
    await expect(shown).toBeVisible();
    await expect(shown).toContainText('phone');
    await expect(shown).toContainText('secret');
  });

  test('TC-GOV-02 新增并删除行级策略：删除前二次确认，删完库里同步消失', async ({ page }) => {
    const ws = await workspaceId();
    const gov = new GovernancePage(page);
    await gov.open();
    await gov.tab('行级权限');

    await gov.addRowPolicy({ table: TABLE, role: 'viewer', filter: "status = 'paid'", desc: '外部访客只看已支付' });
    await expectToast(page, '行级策略已保存');

    const row = await waitForDb(
      () => one<{ id: string; filter_expr: string; description: string }>(
        'SELECT id, filter_expr, description FROM row_policy WHERE workspace_id = $1 AND table_name = $2 AND role = $3',
        [ws, TABLE, 'viewer'],
      ),
      r => !!r, '行级策略应落库',
    );
    expect(row!.filter_expr).toBe("status = 'paid'");
    expect(row!.description).toBe('外部访客只看已支付');

    const tr = page.locator('#view tbody tr').filter({ hasText: TABLE });
    await expect(tr).toContainText('viewer');
    await expect(tr).toContainText("status = 'paid'");

    await tr.locator('[data-act="del-row-policy"]').click();
    await expectToast(page, '已删除');
    await expect.poll(() => count('row_policy', 'id = $1', [row!.id])).toBe(0);
    await expect(page.locator('#view tbody tr').filter({ hasText: TABLE })).toHaveCount(0);
  });

  test('TC-GOV-03 分享链接：匿名可访问只读视图，删除后立即失效', async ({ page, browser }) => {
    const ws = await workspaceId();
    const gov = new GovernancePage(page);
    await gov.open();
    await gov.tab('分享');

    await gov.createShare({ kind: 'metric', target: SHARE_TARGET, ttl: '7' });
    await expectToast(page, /链接已复制|已创建/);

    const share = await waitForDb(
      () => one<{ token: string; kind: string; target: string; days: number }>(
        `SELECT token, kind, target,
                ROUND(EXTRACT(EPOCH FROM (expires_at - now())) / 86400)::int AS days
           FROM share_link WHERE workspace_id = $1 AND target = $2
          ORDER BY created_at DESC LIMIT 1`, [ws, SHARE_TARGET],
      ),
      r => !!r, '分享链接应落库',
    );
    expect(share!.kind).toBe('metric');
    expect(share!.days).toBeGreaterThanOrEqual(6);
    expect(share!.days).toBeLessThanOrEqual(7);
    await expect(page.locator('#view tbody tr').filter({ hasText: share!.token })).toBeVisible();

    // 匿名访客：不带任何令牌打开分享页
    const guest = await browser.newContext();
    const guestPage = await guest.newPage();
    const errors: string[] = [];
    guestPage.on('pageerror', e => errors.push(e.message));
    await guestPage.goto(`${APP.baseURL}/#/share/${share!.token}`);

    await expect(guestPage.locator('.page-title')).toHaveText(SHARE_TARGET, { timeout: 15_000 });
    await expect(guestPage.locator('#crumb')).toContainText('分享视图');
    // 安全预期：外部访客看不到内部导航与操作按钮
    await expect(guestPage.locator('.side')).toBeHidden();
    // 顶栏只留「刷新」：执行、切主题这类内部操作按钮必须消失
    await expect(guestPage.locator('[data-act=\"run-once\"]')).toBeHidden();
    await expect(guestPage.locator('.theme-btn')).toBeHidden();
    await expect(guestPage.locator('[data-act=\"reload\"]')).toBeVisible();
    expect(errors, '分享页不应有脚本异常').toEqual([]);
    await guest.close();

    // 回到控制台删除：库里先消失，链接随即失效
    await gov.tab('分享');
    await page.locator('#view tbody tr').filter({ hasText: share!.token })
      .locator('[data-act="del-share"]').click();
    await expectToast(page, '已删除');
    await expect.poll(() => count('share_link', 'token = $1', [share!.token])).toBe(0);

    const gone = await browser.newContext();
    const gonePage = await gone.newPage();
    await gonePage.goto(`${APP.baseURL}/#/share/${share!.token}`);
    await expect(gonePage.locator('#view')).toContainText('分享链接不存在或已过期', { timeout: 15_000 });
    await gone.close();
  });

  test('TC-GOV-04 写操作进审计：动作、资源、操作人都能对上', async ({ page }) => {
    const ws = await workspaceId();

    // 制造一条可追踪的写操作
    await sql(
      `INSERT INTO row_policy(id, workspace_id, table_name, role, filter_expr, description)
       VALUES ($1, $2, $3, 'editor', '1=1', 'audit probe')
       ON CONFLICT (workspace_id, table_name, role) DO UPDATE SET description = EXCLUDED.description`,
      [`rp_${PREFIX}_audit`, ws, TABLE + '_audit'],
    );

    const gov = new GovernancePage(page);
    await gov.open();
    await gov.tab('行级权限');
    await page.locator('#view tbody tr').filter({ hasText: TABLE + '_audit' })
      .locator('[data-act="del-row-policy"]').click();
    await expectToast(page, '已删除');

    const audit = await waitForDb(
      () => one<{ action: string; resource: string; actor: string }>(
        `SELECT action, resource, actor FROM audit_log
          WHERE workspace_id = $1 AND resource = $2 ORDER BY id DESC LIMIT 1`,
        [ws, `rp_${PREFIX}_audit`],
      ),
      r => !!r, '删除策略应写审计',
    );
    expect(audit!.action).toBe('policy.row.delete');
    expect(audit!.actor).toBeTruthy();

    await page.goto(`${APP.baseURL}/#/audit`);
    const firstRow = page.locator('#view tbody tr').first();
    await expect(firstRow).toContainText(audit!.action);
    await expect(firstRow).toContainText(`rp_${PREFIX}_audit`);
  });

  test.afterAll(async () => {
    const ws = await workspaceId();
    await sql('DELETE FROM column_policy WHERE workspace_id = $1 AND table_name LIKE $2', [ws, PREFIX + '%']);
    await sql('DELETE FROM row_policy WHERE workspace_id = $1 AND table_name LIKE $2', [ws, PREFIX + '%']);
    await sql('DELETE FROM share_link WHERE workspace_id = $1 AND target = $2', [ws, SHARE_TARGET]);
  });
});
