import { test, expect } from '@playwright/test';
import { gotoRoute, readTable } from '../src/ui';
import { count, one, workspaceId } from '../src/db';

/**
 * 人物设定：接手一张陌生表的分析师，和被要求「把图表嵌到周报里」的运营。
 *
 * 前者的入口是「数据文档」：字段、注释、上下游要能一页读完。
 * 后者的入口是「BI 图表」：预览必须真的查了数，SQL 要能拿出来复核。
 * 这两页都极易退化成「长得像文档的静态页」，所以断言都落在「与元数据一致」上。
 */
test.describe('数据文档与 BI 图表', () => {
  test('TC-CAT-01 数据文档的字段清单与模型定义完全一致', async ({ page }) => {
    const ws = await workspaceId();
    // 定义存在版本快照里：model 表只有 active_version，spec 在 model_version 上
    const model = await one<{ name: string; spec: { columns?: { name: string }[]; schema?: string; table?: string } }>(
      `SELECT m.name, v.spec
         FROM model m JOIN model_version v ON v.model_id = m.id AND v.version = m.active_version
        WHERE m.workspace_id = $1 AND m.active_version > 0
        ORDER BY jsonb_array_length(v.spec->'columns') DESC LIMIT 1`, [ws],
    );
    test.skip(!model, '当前工作区没有已发布的模型');

    await gotoRoute(page, 'catalog');
    await page.locator(`#cat-nav [data-model="${model!.name}"]`).click();
    await expect(page.locator('#cat-main')).toContainText(model!.name);

    // 字段表：行数与名称集合都要和 spec.columns 对得上
    const table = await readTable(page, '#cat-main');
    const expected = (model!.spec.columns || []).map(c => c.name);
    expect(table.rows.length).toBe(expected.length);
    const shown = table.rows.map(r => r[0]);
    expect(new Set(shown)).toEqual(new Set(expected));

    // 物理表名来自定义，不是前端猜的
    const physical = model!.spec.table || model!.name;
    await expect(page.locator('#cat-main .doc-meta')).toContainText(physical);
  });

  test('TC-CAT-02 按字段名搜索：能定位到持有该字段的模型', async ({ page }) => {
    const ws = await workspaceId();
    const hit = await one<{ model: string; column: string }>(
      `SELECT m.name AS model, c->>'name' AS column
         FROM model m
         JOIN model_version v ON v.model_id = m.id AND v.version = m.active_version,
              jsonb_array_elements(v.spec->'columns') c
        WHERE m.workspace_id = $1 AND m.active_version > 0
        ORDER BY c->>'name' LIMIT 1`, [ws],
    );
    test.skip(!hit, '没有可用字段做搜索');

    await gotoRoute(page, 'catalog');
    await page.fill('#cat-q', hit!.column);
    await expect(page.locator(`#cat-nav [data-model="${hit!.model}"]`)).toBeVisible();
  });

  test('TC-BI-01 图表列表与库一致，预览真的查了数，SQL 可复核', async ({ page }) => {
    const ws = await workspaceId();
    const charts = await count('public.chart', 'workspace_id = $1', [ws]);
    test.skip(charts === 0, '当前工作区还没有图表');

    await gotoRoute(page, 'bi');
    const items = page.locator('#view [data-chart]');
    await expect(items).toHaveCount(charts);

    const first = await one<{ name: string; dataset: string }>(
      "SELECT name, spec->>'dataset' AS dataset FROM public.chart WHERE workspace_id = $1 ORDER BY name LIMIT 1", [ws],
    );
    await page.locator(`#view [data-chart="${first!.name}"]`).click();

    // 交互预期：预览区给出「N 个数据点」，不是空图
    await expect(page.locator('#view')).toContainText('数据点', { timeout: 15_000 });

    // 交互预期：SQL 在页面内展开（不是弹窗），且指向该图表的数据集
    await page.click('[data-act="bi-sql"]');
    const sqlBox = page.locator('#bi-sqlbox');
    await expect(sqlBox).toBeVisible();
    await expect(sqlBox).toContainText(/SELECT/i);
    await expect(sqlBox).toContainText(first!.dataset);
  });
});
