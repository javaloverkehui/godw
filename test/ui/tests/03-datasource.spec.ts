import { test, expect } from '@playwright/test';
import { SourcesPage } from '../src/pages';
import { acceptDialogs, closeModal, toasts } from '../src/ui';
import { count, one, sql, workspaceId } from '../src/db';
import { PREFIX } from '../src/config';

/**
 * 人物设定：新来的数据工程同学，被安排接入公司第一个 PostgreSQL。
 *
 * 他手会抖：名字忘了填、密码输错、端口填成 1。系统必须每次都给他一句人话，
 * 而且——这是重点——不许在数据库里留下半条脏记录。
 */

const dsName = (suffix: string) => `${PREFIX}_ds_${suffix}`;

test.describe('数据源接入', () => {
  test.beforeEach(async ({ page }) => acceptDialogs(page));

  test('TC-DS-01 接入成功后：列表可见、凭证加密落库、接口不回传明文', async ({ page }) => {
    const ws = await workspaceId();
    const name = dsName('ok');
    const sources = new SourcesPage(page);
    await sources.open();

    const res = await sources.create({ name, password: 'godw' });
    expect(res.status).toBe(201);
    // 安全预期：响应里不出现 secret / password 字段（明文密码只在请求里出现一次）
    expect(res.body).not.toMatch(/"secret"|"password"/i);

    await expect(toasts(page).filter({ hasText: '数据源已接入' })).toBeVisible();
    await expect(sources.row(name)).toBeVisible();
    await expect(sources.row(name)).toContainText('127.0.0.1:5432/godw');
    await expect(sources.row(name)).toContainText('正常');

    const row = await one<{ status: string; type: string; config: Record<string, unknown>; has_secret: boolean; checked_at: Date | null }>(
      `SELECT status, type, config, secret_enc IS NOT NULL AS has_secret, checked_at
         FROM datasource WHERE workspace_id = $1 AND name = $2`, [ws, name],
    );
    expect(row, '数据源应落库').toBeTruthy();
    expect(row!.type).toBe('postgres');
    expect(row!.status).toBe('ok');
    expect(row!.has_secret, '凭证必须以密文形式留存').toBe(true);
    expect(row!.checked_at, '接入时会真实连一次并记录检查时间').not.toBeNull();
    expect(row!.config).toMatchObject({ host: '127.0.0.1', port: 5432, database: 'godw', user: 'godw' });
  });

  test('TC-DS-02 名称为空：前端拦下并提示，不发请求、不落库', async ({ page }) => {
    const ws = await workspaceId();
    const sources = new SourcesPage(page);
    await sources.open();

    const posts: string[] = [];
    page.on('request', r => {
      if (r.url().endsWith('/api/v1/datasources') && r.method() === 'POST') posts.push(r.url());
    });

    await sources.openCreate();
    await page.click('[data-act="save-ds"]');

    // 交互预期：给出「请填写名称」，弹窗不关闭，用户不用重填其他字段
    await expect(toasts(page).filter({ hasText: '请填写名称' })).toBeVisible();
    await expect(page.locator('.modal')).toBeVisible();
    await expect(page.locator('#ds-host')).toHaveValue('127.0.0.1');

    // 数据预期：一个请求都没发，库里当然也没有
    expect(posts, '空名称不应发起创建请求').toEqual([]);
    expect(await count('datasource', 'workspace_id = $1 AND name = $2', [ws, ''])).toBe(0);
    await closeModal(page);
  });

  test('TC-DS-03 连不上时：报错要说清原因，且不写入半条记录', async ({ page }) => {
    const ws = await workspaceId();
    const name = dsName('bad');
    const sources = new SourcesPage(page);
    await sources.open();

    await sources.openCreate();
    await page.fill('#ds-name', name);
    await page.selectOption('#ds-type', 'postgres');
    await page.fill('#ds-host', '127.0.0.1');
    await page.fill('#ds-port', '1');            // 必然拒绝连接
    await page.fill('#ds-db', 'godw');
    await page.fill('#ds-user', 'godw');
    await page.fill('#ds-pass', 'godw');
    await page.click('[data-act="save-ds"]');

    // 交互预期：错误提示可读（包含连接/拒绝/失败等线索），弹窗保留以便修正
    await expect(toasts(page).filter({ hasText: /连接|拒绝|失败|refused/i }).first())
      .toBeVisible({ timeout: 20_000 });
    await expect(page.locator('.modal')).toBeVisible();

    // 数据预期：不留任何数据源行
    expect(await count('datasource', 'workspace_id = $1 AND name = $2', [ws, name])).toBe(0);
    await closeModal(page);
  });

  test('TC-DS-04 已接入数据源可测试连接，检查时间会前移', async ({ page }) => {
    const ws = await workspaceId();
    const name = dsName('ping');
    const sources = new SourcesPage(page);
    await sources.open();
    await sources.create({ name });
    await expect(sources.row(name)).toBeVisible();

    const before = await one<{ checked_at: Date }>(
      'SELECT checked_at FROM datasource WHERE workspace_id = $1 AND name = $2', [ws, name],
    );

    await page.waitForTimeout(1100); // 时间戳精度到秒，等一秒才能看出前移
    await sources.test(name);
    await expect(toasts(page).filter({ hasText: '连接正常' })).toBeVisible({ timeout: 15_000 });

    const after = await one<{ status: string; checked_at: Date }>(
      'SELECT status, checked_at FROM datasource WHERE workspace_id = $1 AND name = $2', [ws, name],
    );
    expect(after!.status).toBe('ok');
    expect(new Date(after!.checked_at).getTime()).toBeGreaterThan(new Date(before!.checked_at).getTime());
  });

  test('TC-DS-05 采集元数据：toast 报的表数与 dataset 落库行数一致', async ({ page }) => {
    const ws = await workspaceId();
    const name = dsName('collect');
    const sources = new SourcesPage(page);
    await sources.open();
    await sources.create({ name });
    await expect(sources.row(name)).toBeVisible();

    await sources.collect(name);
    const toast = toasts(page).filter({ hasText: /已采集/ });
    await expect(toast).toBeVisible({ timeout: 30_000 });
    const said = Number((await toast.innerText()).replace(/\D/g, ''));
    expect(said, 'toast 里应带上表数').toBeGreaterThan(0);

    const ds = await one<{ id: string }>(
      'SELECT id FROM datasource WHERE workspace_id = $1 AND name = $2', [ws, name],
    );
    const rows = await count('dataset', 'datasource_id = $1', [ds!.id]);
    expect(rows, 'dataset 行数应等于 toast 宣称的数量').toBe(said);

    // 采集快照必须带列信息，否则「表结构」页会是空壳
    const emptyCols = await count('dataset', 'datasource_id = $1 AND jsonb_array_length(columns) = 0', [ds!.id]);
    expect(emptyCols, '不应存在零字段的表快照').toBe(0);
  });

  test('TC-DS-06 删除数据源：二次确认、行消失、采集快照级联清理', async ({ page }) => {
    const ws = await workspaceId();
    const name = dsName('del');
    const sources = new SourcesPage(page);
    await sources.open();
    await sources.create({ name });
    await expect(sources.row(name)).toBeVisible();

    await sources.collect(name);
    await expect(toasts(page).filter({ hasText: /已采集/ })).toBeVisible({ timeout: 30_000 });
    const ds = await one<{ id: string }>(
      'SELECT id FROM datasource WHERE workspace_id = $1 AND name = $2', [ws, name],
    );
    expect(await count('dataset', 'datasource_id = $1', [ds!.id])).toBeGreaterThan(0);

    await sources.remove(name);
    await expect(toasts(page).filter({ hasText: '已删除' })).toBeVisible();
    await expect(sources.row(name)).toHaveCount(0);

    // 数据预期：数据源与它采集的元数据一起消失（外键级联，不留孤儿）
    await expect.poll(() => count('datasource', 'id = $1', [ds!.id])).toBe(0);
    expect(await count('dataset', 'datasource_id = $1', [ds!.id])).toBe(0);
  });

  test.afterAll(async () => {
    await sql('DELETE FROM datasource WHERE workspace_id = $1 AND name LIKE $2', [await workspaceId(), PREFIX + '%']);
  });
});
