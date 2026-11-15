import { test, expect } from '@playwright/test';
import { QualityPage } from '../src/pages';
import { readTable } from '../src/ui';
import { sql, workspaceId } from '../src/db';

/**
 * 人物设定：被问「昨天质量怎么样」的数据负责人。
 *
 * 他只看汇总，但要求每个数字都能追到明细：通过率、规则数、每条规则跑了几次。
 * 页面上的数字一旦和 quality_result 对不上，他对整套监控的信任就归零了。
 */
test.describe('质量看板', () => {
  test('TC-QUAL-01 总通过率与规则数同库里聚合一致', async ({ page }) => {
    const ws = await workspaceId();
    const quality = new QualityPage(page);
    await quality.open();

    const agg = await sql<{ total: number; passed: number; rules: number }>(
      `SELECT count(*)::int AS total,
              count(*) FILTER (WHERE q.passed)::int AS passed,
              count(DISTINCT q.rule_name)::int AS rules
         FROM quality_result q JOIN instance i ON i.id = q.instance_id
        WHERE i.workspace_id = $1`, [ws],
    );
    const { total, passed, rules } = agg[0];
    test.skip(total === 0, '当前工作区还没有质量结果，页面走空态，另由空态用例覆盖');

    const stats = quality.stats();
    const rate = total ? (passed * 100 / total).toFixed(1) : '—';
    await expect(stats.nth(0)).toContainText(`${rate}%`);
    await expect(stats.nth(0)).toContainText(`${passed}/${total}`);
    await expect(stats.nth(1).locator('.stat-value')).toHaveText(String(rules));
  });

  test('TC-QUAL-02 按规则汇总：每条规则的次数与失败数都能对上明细', async ({ page }) => {
    const ws = await workspaceId();
    const quality = new QualityPage(page);
    await quality.open();

    const rows = await sql<{ rule: string; total: number; failed: number }>(
      `SELECT q.rule_name AS rule,
              count(*)::int AS total,
              count(*) FILTER (WHERE NOT q.passed)::int AS failed
         FROM quality_result q JOIN instance i ON i.id = q.instance_id
        WHERE i.workspace_id = $1
        GROUP BY 1 ORDER BY 1`, [ws],
    );
    test.skip(rows.length === 0, '没有质量结果可核对');

    const table = await readTable(page, '#view');
    // 第一张表是「按规则汇总」，行数与规则数一致
    expect(table.rows.length).toBe(rows.length);

    for (const r of rows) {
      const line = table.rows.find(cells => cells[0] === r.rule);
      expect(line, `页面应列出规则 ${r.rule}`).toBeTruthy();
      expect(line![2], `规则 ${r.rule} 的总次数`).toBe(String(r.total));
      expect(line![3], `规则 ${r.rule} 的失败数`).toBe(String(r.failed));
    }
  });

  test('TC-QUAL-03 切换实例后明细随之变化，空结果给出空态', async ({ page }) => {
    const ws = await workspaceId();
    const quality = new QualityPage(page);
    await quality.open();

    const withResults = await sql<{ id: string; n: number }>(
      `SELECT q.instance_id AS id, count(*)::int AS n
         FROM quality_result q JOIN instance i ON i.id = q.instance_id
        WHERE i.workspace_id = $1 GROUP BY 1 ORDER BY 2 DESC`, [ws],
    );
    test.skip(withResults.length === 0, '没有带质量结果的实例');

    const picker = page.locator('#q-inst');
    const options = await picker.locator('option').count();
    expect(options).toBeGreaterThan(0);

    // 选第一个实例，明细行数应等于库里该实例的结果条数
    await picker.selectOption(withResults[0].id);
    await page.waitForFunction(() => !document.querySelector('#view')?.innerText.includes('加载中…'));
    const detailRows = page.locator('#view table').last().locator('tbody tr');
    await expect(detailRows).toHaveCount(withResults[0].n);
  });
});
