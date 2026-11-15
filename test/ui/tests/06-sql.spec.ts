import { test, expect } from '@playwright/test';
import { SqlPage } from '../src/pages';
import { sql } from '../src/db';

/**
 * 人物设定：临时想核对一个数的分析师。
 *
 * 他不写代码，只会在控制台里敲 SQL。这个页面的价值有两条：
 * 数要真的查出来（不是前端造了一行假数据），以及写操作必须被挡住——
 * 一个「只读」入口如果能删库，比没有这个入口更危险。
 */
test.describe('自助查询（只读 SQL）', () => {
  test('TC-SQL-01 查询结果与数据库逐行一致', async ({ page }) => {
    const api = new SqlPage(page);
    await api.open();

    const res = await api.run('select id, name from workspace order by id', { datasource: 'pg_dw', limit: '100' });
    expect(res.status).toBe(200);

    const expected = await sql<{ id: string; name: string }>('SELECT id, name FROM workspace ORDER BY id');
    const table = await api.resultTable();
    // 表头在样式里做了大小写转换，innerText 拿到的是视觉文案；比较时忽略大小写
    expect(table.headers.map(h => h.toLowerCase())).toEqual(['id', 'name']);
    expect(table.rows.length).toBe(expected.length);

    // 逐行比对：ID 与名称都必须和库里一致，顺序也不能乱
    expected.forEach((row, i) => {
      expect(table.rows[i][0]).toBe(row.id);
      expect(table.rows[i][1]).toBe(row.name);
    });

    await expect(api.result()).toContainText(`${expected.length} 行`);
  });

  test('TC-SQL-02 写操作被拒：错误说清原因，数据没有被改动', async ({ page }) => {
    const before = await sql<{ id: string; name: string }>('SELECT id, name FROM workspace ORDER BY id LIMIT 1');
    const api = new SqlPage(page);
    await api.open();

    const res = await api.run(`update workspace set name = 'hacked_by_e2e' where id = '${before[0].id}'`);
    expect(res.status).toBe(400);

    await expect(api.errorBox()).toContainText('只允许 SELECT');
    await expect(api.errorBox()).toContainText('UPDATE');

    // 数据预期：写不进去才算真的只读
    const after = await sql<{ name: string }>('SELECT name FROM workspace WHERE id = $1', [before[0].id]);
    expect(after[0].name).toBe(before[0].name);
  });

  test('TC-SQL-03 多条语句被拒：分号后不允许再有内容', async ({ page }) => {
    const api = new SqlPage(page);
    await api.open();

    const res = await api.run('select 1 as a; select 2 as b');
    expect(res.status).toBe(400);
    await expect(api.errorBox()).toContainText('单条语句');
  });

  test('TC-SQL-04 行数上限生效：超限时截断并明确告知', async ({ page }) => {
    const api = new SqlPage(page);
    await api.open();

    const res = await api.run('select generate_series(1, 500) as i', { limit: '100' });
    expect(res.status).toBe(200);

    // 交互预期：提示里写明「已截断到 100 行」，用户才知道自己看到的不是全量
    await expect(api.result().locator('.card-head .hint')).toContainText('100 行');
    await expect(api.result().locator('.card-head .hint')).toContainText('已截断到 100 行');
    await expect(api.result().locator('tbody tr')).toHaveCount(100);
  });

  test('TC-SQL-05 空结果给出空态，而不是一张空表', async ({ page }) => {
    const api = new SqlPage(page);
    await api.open();

    const res = await api.run('select 1 as x where false');
    expect(res.status).toBe(200);
    await expect(api.result()).toContainText('查询成功，没有返回行');
    await expect(api.result().locator('table')).toHaveCount(0);
  });

  test('TC-SQL-06 Ctrl+Enter 直接执行，不用挪鼠标', async ({ page }) => {
    const api = new SqlPage(page);
    await api.open();

    await page.fill('#sql-editor textarea', 'select 7 as lucky');
    const resp = page.waitForResponse(r => r.url().includes('/api/v1/sql/query'));
    await page.locator('#sql-editor textarea').press('Control+Enter');
    expect((await resp).status()).toBe(200);

    await expect(api.result()).toContainText('1 行');
    const table = await api.resultTable();
    expect(table.rows[0][0]).toBe('7');
  });
});
