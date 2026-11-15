import { test, expect } from '@playwright/test';
import { InstancesPage, PipelinesPage } from '../src/pages';
import { acceptDialogs, expectToast } from '../src/ui';
import { count, one, sql, waitForDb, workspaceId } from '../src/db';
import { PREFIX } from '../src/config';

/**
 * 人物设定：值班的数据运维。
 *
 * 他的动作全是「出事了才做」：停一条乱跑的管道、手动补一次、把失败的重跑、
 * 打开某次运行看它到底卡在哪个节点。这些动作一旦不生效，损失是当天的数据，
 * 所以每条都要落到 instance / task_instance 上验一遍。
 */
const pipeFor = (k: string) => `${PREFIX}_pipe_${k}`;

async function createPipeline(page: import('@playwright/test').Page, name: string): Promise<void> {
  const pipes = new PipelinesPage(page);
  await pipes.open();
  await pipes.openWizard();
  await page.check('[data-pick="dwd_order"]');
  await page.click('#wiz-next');
  await expect(page.locator('#wiz-body')).toContainText('产出模型');
  await page.click('#wiz-next');
  await expect(page.locator('#wz-pname')).toBeVisible();

  await page.fill('#wz-pname', name);
  await page.locator('#wiz-body').getByText('手动', { exact: true }).click();
  const runNow = page.locator('#wz-runnow');
  if (await runNow.isChecked()) await runNow.uncheck();

  await page.locator('.modal button', { hasText: '发布管道' }).click();
  // 发布成功后向导不弹 toast，而是在原地渲染「已发布」结果页
  await expect(page.locator('.wiz-done')).toContainText(`${name} 已发布 v1`, { timeout: 15_000 });
  await page.keyboard.press('Escape');
}

test.describe('管道编排与实例运行', () => {
  test.beforeEach(async ({ page }) => acceptDialogs(page));

  test('TC-PIPE-01 新建管道：未选产出模型被拦，发布后版本与节点结构落库', async ({ page }) => {
    const ws = await workspaceId();
    const name = pipeFor('new');
    const pipes = new PipelinesPage(page);
    await pipes.open();

    await pipes.openWizard();
    // 交互预期：不选模型直接下一步，给出明确原因而不是静默失败
    await page.click('#wiz-next');
    await expectToast(page, '至少选一个要产出的模型');
    await expect(page.locator('#wiz-body')).toContainText('DWD');

    await page.check('[data-pick="dwd_order"]');
    await page.click('#wiz-next');
    await expect(page.locator('#wiz-body')).toContainText('产出模型');
    await page.click('#wiz-next');
    await expect(page.locator('#wz-pname')).toBeVisible();

    await page.fill('#wz-pname', name);
    await page.locator('#wiz-body').getByText('手动', { exact: true }).click();
    const runNow = page.locator('#wz-runnow');
    if (await runNow.isChecked()) await runNow.uncheck();
    await page.locator('.modal button', { hasText: '发布管道' }).click();
    await expect(page.locator('.wiz-done')).toContainText(`${name} 已发布 v1`, { timeout: 15_000 });
    await page.keyboard.press('Escape');

    // 数据预期：管道 + 版本快照，节点指向刚勾的模型
    const p = await one<{ id: string; enabled: boolean; active_version: number; schedule: string }>(
      'SELECT id, enabled, active_version, schedule FROM pipeline WHERE workspace_id = $1 AND name = $2',
      [ws, name],
    );
    expect(p, '管道应落库').toBeTruthy();
    expect(p!.active_version).toBe(1);
    expect(p!.enabled).toBe(true);
    expect(p!.schedule, '选择「手动」后不应带 cron').toBe('');

    const v = await one<{ status: string; nodes: number; model: string }>(
      `SELECT status,
              jsonb_array_length(spec->'nodes') AS nodes,
              spec->'nodes'->0->>'model' AS model
         FROM pipeline_version WHERE pipeline_id = $1`, [p!.id],
    );
    expect(v!.status).toBe('published');
    expect(v!.nodes).toBe(1);
    expect(v!.model).toBe('dwd_order');
  });

  test('TC-PIPE-02 停用/启用：状态与库同步，页面徽标跟着变', async ({ page }) => {
    const ws = await workspaceId();
    const name = pipeFor('toggle');
    await createPipeline(page, name);

    const pipes = new PipelinesPage(page);
    await pipes.open();
    await pipes.select(name);
    const toggle = page.locator(`[data-act="toggle-pl"][data-name="${name}"]`);
    await expect(toggle).toHaveText('停用');

    await pipes.toggle(name);
    await expectToast(page, '已停用');
    await expect.poll(async () => (await one<{ enabled: boolean }>(
      'SELECT enabled FROM pipeline WHERE workspace_id = $1 AND name = $2', [ws, name],
    ))!.enabled).toBe(false);
    await expect(toggle).toHaveText('启用');

    await pipes.toggle(name);
    await expectToast(page, '已启用');
    await expect.poll(async () => (await one<{ enabled: boolean }>(
      'SELECT enabled FROM pipeline WHERE workspace_id = $1 AND name = $2', [ws, name],
    ))!.enabled).toBe(true);
  });

  test('TC-PIPE-03 立即运行：每次手动触发都是一次新运行，实例与节点同步落库', async ({ page }) => {
    const ws = await workspaceId();
    const name = pipeFor('trigger');
    await createPipeline(page, name);

    const pipes = new PipelinesPage(page);
    await pipes.open();
    await pipes.select(name);
    await pipes.trigger(name);

    // 交互预期：触发后自动跳到运行实例页，并给出实例号
    await expectToast(page, /已创建实例/);
    await expect(page).toHaveURL(/#\/instances/);
    await expect(page.locator('.page-title')).toHaveText('运行实例');

    const p = await one<{ id: string }>('SELECT id FROM pipeline WHERE workspace_id = $1 AND name = $2', [ws, name]);
    const inst = await waitForDb(
      () => one<{ id: string; status: string; trigger_type: string; biz_date: string }>(
        'SELECT id, status, trigger_type, biz_date::text AS biz_date FROM instance WHERE pipeline_id = $1',
        [p!.id],
      ),
      r => !!r && ['queued', 'running', 'success', 'failed', 'canceled'].includes(r.status),
      '触发后应有实例记录',
    );
    expect(inst!.trigger_type).toBe('manual');
    expect(inst!.biz_date).toMatch(/^\d{4}-\d{2}-\d{2}$/);

    // 数据预期：节点表随实例一起生成，节点来自管道版本
    expect(await count('task_instance', 'instance_id = $1', [inst!.id])).toBe(1);

    // 手动触发是「再跑一次」的语义：不覆盖历史，而是新增一条独立实例
    await pipes.open();
    await pipes.select(name);
    await pipes.trigger(name);
    await expectToast(page, /已创建实例/);

    const rows = await waitForDb(
      () => sql<{ id: string; trigger_type: string }>(
        'SELECT id, trigger_type FROM instance WHERE pipeline_id = $1 ORDER BY created_at DESC', [p!.id],
      ),
      rs => rs.length >= 2,
      '第二次触发应新增实例',
    );
    expect(rows[0].id).not.toBe(inst!.id);
    expect(rows.every(r => r.trigger_type === 'manual')).toBe(true);
    expect(await count('task_instance', 'instance_id = $1', [rows[0].id])).toBe(1);
  });

  test('TC-PIPE-04 实例筛选与分页：筛选后的条数与库一致，行状态不混入其他状态', async ({ page }) => {
    const ws = await workspaceId();
    const name = pipeFor('filter');
    await createPipeline(page, name);

    // 先造两条属于自己的运行记录，再在「只看这条管道」的范围内断言：
    // 直接数全库会被调度器和其他用例正在跑的实例带偏，那是假失败。
    const pipes = new PipelinesPage(page);
    await pipes.open();
    await pipes.select(name);
    await pipes.trigger(name);
    await expectToast(page, /已创建实例/);
    await pipes.open();
    await pipes.select(name);
    await pipes.trigger(name);
    await expectToast(page, /已创建实例/);

    const p = await one<{ id: string }>('SELECT id FROM pipeline WHERE workspace_id = $1 AND name = $2', [ws, name]);
    await waitForDb(
      () => count('instance', `pipeline_id = $1 AND status IN ('success','failed','canceled')`, [p!.id]),
      n => n === 2, '两条实例都应进入终态',
    );

    const instances = new InstancesPage(page);
    await instances.open();
    await instances.filterPipeline(name);
    await expect(instances.totalText()).toContainText('共 2 条');
    await expect(instances.rows()).toHaveCount(2);

    // 状态筛选：条数与库一致，且每行状态都真的是所选状态
    for (const [value, label] of [['success', '成功'], ['failed', '失败']] as const) {
      await instances.filterStatus(value);
      const total = await count('instance', 'pipeline_id = $1 AND status = $2', [p!.id, value]);
      await expect(instances.totalText()).toContainText(`共 ${total} 条`);

      const rows = instances.rows();
      await expect(rows).toHaveCount(Math.min(total, 20));
      for (const row of await rows.all()) {
        expect(await row.innerText(), `筛选 ${value} 时不应混入其他状态`).toContain(label);
      }
      if (total === 0) {
        await expect(page.locator('#view')).toContainText('没有符合条件的实例');
      }
    }

    // 分页：每页 10 条时行数不超过 10，且总数文案与库一致
    await instances.filterStatus('');
    await page.selectOption('#pg-size', '10');
    await expect.poll(async () => instances.rows().count()).toBeLessThanOrEqual(10);
    await expect(instances.totalText()).toContainText('共 2 条');
  });

  test('TC-PIPE-05 批量重跑：生成 retry 触发的新实例，勾选区随之清空', async ({ page }) => {
    const ws = await workspaceId();
    const name = pipeFor('retry');
    await createPipeline(page, name);

    const pipes = new PipelinesPage(page);
    await pipes.open();
    await pipes.select(name);
    await pipes.trigger(name);
    await expectToast(page, /已创建实例|实例已存在/);

    const p = await one<{ id: string }>('SELECT id FROM pipeline WHERE workspace_id = $1 AND name = $2', [ws, name]);
    await waitForDb(
      () => count('instance', 'pipeline_id = $1', [p!.id]), n => n >= 1, '实例应生成',
    );

    const instances = new InstancesPage(page);
    await instances.open();
    await instances.filterPipeline(name);
    const row = instances.rows().first();
    await row.locator('.inst-ck').check();
    await expect(page.locator('#view')).toContainText('已选 1 条');

    await page.click('[data-act="batch-retry"]');
    await expectToast(page, /重跑完成：成功 1/);
    await expect(page.locator('#view')).not.toContainText('已选 1 条');

    // 数据预期：新增一条 trigger_type=retry 的实例，指向同一条管道
    const rows = await waitForDb(
      () => sql<{ trigger_type: string }>(
        'SELECT trigger_type FROM instance WHERE pipeline_id = $1 ORDER BY created_at DESC', [p!.id],
      ),
      rs => rs.length >= 2 && rs[0].trigger_type === 'retry',
      '批量重跑应产生 retry 实例',
    );
    expect(rows.every(r => ['manual', 'retry'].includes(r.trigger_type))).toBe(true);
  });

  test('TC-PIPE-06 实例详情：节点表与 task_instance 对齐，渲染 SQL 来自版本快照', async ({ page }) => {
    const ws = await workspaceId();
    const name = pipeFor('detail');
    await createPipeline(page, name);

    const pipes = new PipelinesPage(page);
    await pipes.open();
    await pipes.select(name);
    await pipes.trigger(name);
    await expectToast(page, /已创建实例|实例已存在/);

    const p = await one<{ id: string }>('SELECT id FROM pipeline WHERE workspace_id = $1 AND name = $2', [ws, name]);
    const inst = await waitForDb(
      () => one<{ id: string; status: string }>(
        'SELECT id, status FROM instance WHERE pipeline_id = $1', [p!.id],
      ),
      r => !!r, '实例应存在',
    );

    const instances = new InstancesPage(page);
    await instances.open();
    await instances.filterPipeline(name);
    await instances.openDetail(inst!.id);

    const drawer = page.locator('.drawer');
    await expect(drawer).toBeVisible();
    const tasks = await sql<{ node_id: string; status: string }>(
      'SELECT node_id, status FROM task_instance WHERE instance_id = $1', [inst!.id],
    );
    await expect(drawer.locator('table.dt').first().locator('tbody tr')).toHaveCount(tasks.length);
    for (const t of tasks) await expect(drawer).toContainText(t.node_id);

    // 交互预期：渲染 SQL 真的取到了内容（不是一直转圈的骨架屏）
    await expect(drawer.locator('#sql-box')).not.toContainText('加载中', { timeout: 15_000 });
    await expect(drawer.locator('#sql-box')).toContainText(/SELECT|select/);
  });

  test('TC-PIPE-07 编辑管道描述并发布：版本递增，历史运行仍挂在同一条管道上', async ({ page }) => {
    const ws = await workspaceId();
    const name = pipeFor('edit');
    await createPipeline(page, name);

    const pipes = new PipelinesPage(page);
    await pipes.open();
    await pipes.select(name);
    await pipes.trigger(name);
    await expectToast(page, /已创建实例/);

    const p = await one<{ id: string }>('SELECT id FROM pipeline WHERE workspace_id = $1 AND name = $2', [ws, name]);
    const before = await count('instance', 'pipeline_id = $1', [p!.id]);

    await pipes.open();
    const res = await pipes.editDescription(name, '改过的描述 · e2e');
    expect(res.status, res.body).toBe(201);
    await expectToast(page, new RegExp(`已发布 ${name} v2`));

    const after = await one<{ active_version: number; description: string }>(
      'SELECT active_version, description FROM pipeline WHERE id = $1', [p!.id],
    );
    expect(after!.active_version).toBe(2);
    expect(after!.description).toBe('改过的描述 · e2e');

    const versions = await sql<{ version: number; status: string }>(
      'SELECT version, status FROM pipeline_version WHERE pipeline_id = $1 ORDER BY version', [p!.id],
    );
    expect(versions.map(v => v.version)).toEqual([1, 2]);
    expect(versions[1].status).toBe('published');

    // 改名才会丢历史；只改描述不该让运行记录消失
    expect(await count('instance', 'pipeline_id = $1', [p!.id])).toBe(before);
  });

  test.afterAll(async () => {
    const ws = await workspaceId();
    await sql('DELETE FROM pipeline WHERE workspace_id = $1 AND name LIKE $2', [ws, PREFIX + '%']);
  });
});
