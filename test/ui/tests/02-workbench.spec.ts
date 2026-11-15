import { test, expect } from '@playwright/test';
import { OverviewPage } from '../src/pages';
import { count, one, sql, workspaceId } from '../src/db';

/**
 * 人物设定：每天上班先看工作台的负责人。
 *
 * 他不点进任何二级页，只靠这一屏判断「昨天跑得怎么样、哪条链路红了」。
 * 所以这一屏的每个数字都必须和库里对得上——差一个就会让人误判。
 */

const STATUS_LABEL: Record<string, string> = {
  success: '成功', failed: '失败', running: '运行中', queued: '排队中',
  canceled: '已取消', waiting: '等待', pending: '待执行',
};

test.describe('工作台总览', () => {
  test('TC-OVW-01 四张统计卡与库中实际数量一致', async ({ page }) => {
    const ws = await workspaceId();
    const overview = new OverviewPage(page);
    await overview.open();

    const stats = await overview.stats();
    expect(Object.keys(stats)).toEqual(['数据源', '模型', '管道', '近期成功率']);

    const [dsN, modelN, pipeN, dsSetN] = await Promise.all([
      count('datasource', 'workspace_id = $1', [ws]),
      count('model', 'workspace_id = $1', [ws]),
      count('pipeline', 'workspace_id = $1', [ws]),
      count('dataset', 'workspace_id = $1', [ws]),
    ]);

    expect(Number(stats['数据源']), '数据源计数').toBe(dsN);
    expect(Number(stats['模型']), '模型计数').toBe(modelN);
    expect(Number(stats['管道']), '管道计数').toBe(pipeN);

    // 「已采集表」在数据源卡片的小字里，也必须等于 dataset 表行数
    const sub = await page.locator('#view .stat').first().locator('.stat-sub').innerText();
    expect(sub.replace(/\D/g, '')).toBe(String(dsSetN));
  });

  test('TC-OVW-02 最近运行列表逐行对库，顺序与状态不漂移', async ({ page }) => {
    const ws = await workspaceId();
    const overview = new OverviewPage(page);
    await overview.open();

    const rows = overview.recentRows();
    const dbRows = await sql<{ id: string; status: string; biz_date: string; pipeline_name: string }>(
      `SELECT i.id, i.status, i.biz_date::text AS biz_date, COALESCE(p.name, '') AS pipeline_name
         FROM instance i LEFT JOIN pipeline p ON p.id = i.pipeline_id
        WHERE i.workspace_id = $1
        ORDER BY i.created_at DESC
        LIMIT 12`, [ws],
    );

    await expect(rows).toHaveCount(dbRows.length);

    for (let i = 0; i < dbRows.length; i++) {
      const cells = (await rows.nth(i).locator('td').allInnerTexts()).map(t => t.replace(/\s+/g, ' ').trim());
      const [instNo, pipe, bizDate, status] = cells;
      expect(instNo, `第 ${i + 1} 行实例号后 8 位`).toBe(dbRows[i].id.slice(-8));
      expect(pipe, `第 ${i + 1} 行管道名`).toBe(dbRows[i].pipeline_name);
      expect(bizDate, `第 ${i + 1} 行业务日期`).toBe(dbRows[i].biz_date);
      expect(status, `第 ${i + 1} 行状态`).toBe(STATUS_LABEL[dbRows[i].status] ?? dbRows[i].status);
    }
  });

  test('TC-OVW-03 近期成功率按最近 12 次运行计算，不是拍脑袋的固定值', async ({ page }) => {
    const ws = await workspaceId();
    const overview = new OverviewPage(page);
    await overview.open();

    const rows = await sql<{ status: string }>(
      `SELECT status FROM instance WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT 12`, [ws],
    );
    const success = rows.filter(r => r.status === 'success').length;
    const expected = rows.length ? Math.round(success * 100 / rows.length) : 0;

    const stats = await overview.stats();
    expect(stats['近期成功率']).toBe(expected + '%');
    await expect(page.locator('#view .stat').nth(3).locator('.stat-sub'))
      .toContainText(`最近 ${rows.length} 次运行`);
  });

  test('TC-OVW-04 链路健康条：每条管道一行，状态取该管道最近一次运行', async ({ page }) => {
    const ws = await workspaceId();
    const overview = new OverviewPage(page);
    await overview.open();

    const pipes = await sql<{ id: string; name: string }>(
      `SELECT id, name FROM pipeline WHERE workspace_id = $1 ORDER BY created_at`, [ws],
    );
    await expect(overview.pipelineStrip()).toHaveCount(pipes.length);

    for (const p of pipes) {
      const strip = overview.pipelineStrip().filter({ hasText: p.name }).first();
      await expect(strip).toBeVisible();

      const last = await one<{ status: string }>(
        `SELECT status FROM instance WHERE workspace_id = $1 AND pipeline_id = $2
          ORDER BY created_at DESC LIMIT 1`, [ws, p.id],
      );
      const expectedLabel = last ? STATUS_LABEL[last.status] ?? last.status : '未运行';
      await expect(strip, `管道 ${p.name} 的健康状态`).toContainText(expectedLabel);
    }
  });

  test('TC-OVW-05 点一行运行记录能打开详情抽屉，节点数与库一致', async ({ page }) => {
    const ws = await workspaceId();
    const overview = new OverviewPage(page);
    await overview.open();

    const target = await one<{ id: string; pipeline_name: string; biz_date: string; tasks: number }>(
      `SELECT i.id, COALESCE(p.name,'') AS pipeline_name, i.biz_date::text AS biz_date,
              (SELECT count(*)::int FROM task_instance t WHERE t.instance_id = i.id) AS tasks
         FROM instance i LEFT JOIN pipeline p ON p.id = i.pipeline_id
        WHERE i.workspace_id = $1
        ORDER BY i.created_at DESC
        LIMIT 1`, [ws],
    );
    expect(target, '库里应至少有一次运行').toBeTruthy();

    await overview.recentRows().first().click();

    const drawer = page.locator('.drawer');
    await expect(drawer).toBeVisible();
    await expect(drawer.locator('.drawer-head')).toContainText(target!.pipeline_name);
    await expect(drawer.locator('.drawer-head')).toContainText(target!.biz_date);
    await expect(drawer).toContainText(target!.id);

    // 交互预期：节点明细来自 task_instance，不是前端拼的空壳
    const nodeStat = drawer.locator('.stat').filter({ hasText: '节点' }).locator('.stat-value');
    await expect(nodeStat).toHaveText(String(target!.tasks));

    const nodeTable = drawer.locator('table.dt').first();
    if (target!.tasks > 0) {
      await expect(nodeTable.locator('tbody tr')).toHaveCount(target!.tasks);
      const dbNodes = await sql<{ node_id: string }>(
        'SELECT node_id FROM task_instance WHERE instance_id = $1 ORDER BY node_id', [target!.id],
      );
      const shown = (await nodeTable.innerText()).replace(/\s+/g, ' ');
      for (const n of dbNodes) expect(shown, `节点 ${n.node_id} 应出现在抽屉里`).toContain(n.node_id);
    } else {
      await expect(nodeTable.locator('tbody tr')).toHaveCount(0);
    }

    // 交互预期：点关闭后抽屉与遮罩都不残留
    await drawer.locator('[data-act="drawer-close"]').last().click();
    await expect(drawer).toHaveCount(0);
  });
});
