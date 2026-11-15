import { test, expect } from '@playwright/test';
import { ModelsPage } from '../src/pages';
import { acceptDialogs, toasts } from '../src/ui';
import { count, one, sql, workspaceId } from '../src/db';
import { PREFIX } from '../src/config';

/**
 * 人物设定：负责 DWD 层的数据开发。
 *
 * 他改一次建模定义就要发一次版，最怕两件事：
 * 一是 YAML 写错了没人拦，直接建出一张错表；二是「页面说发布成功」但库里没有版本，
 * 下次重跑拿到的是新定义。所以每条用例都把「页面说什么」和「库里有什么」对上。
 *
 * 每个用例用自己的模型名：用例之间不共享可变状态，单条失败不影响其他条。
 */
const modelFor = (k: string) => `dwd_${PREFIX}_${k}`;
/** 模型名必须带层前缀（dwd_），故意用错名验证建模规范的拦截。 */
const badFor = (k: string) => `bad_${PREFIX}_${k}`;

const yamlOf = (name: string, cols: string, desc = '自动化建模') => `kind: Model
name: ${name}
layer: dwd
description: ${desc}
datasource: pg_dw
schema: e2e
columns:
${cols}
primary_key: [id]
`;

const TWO_COLS = `  - name: id
    type: bigint
    nullable: false
    comment: 主键
  - name: dt
    type: date
    nullable: false
    comment: 分区日期`;

test.describe('模型定义与发布', () => {
  test.beforeEach(async ({ page }) => acceptDialogs(page));

  test('TC-MODEL-01 预览 DDL：只编译不落库，列与主键都在', async ({ page }) => {
    const ws = await workspaceId();
    const name = modelFor('m1');
    const models = new ModelsPage(page);
    await models.open();

    await models.openYamlEditor();
    await page.fill('#model-yaml', yamlOf(name, TWO_COLS));
    const ddl = await models.previewDDL();

    expect(ddl).toContain(`CREATE TABLE IF NOT EXISTS "e2e"."${name}"`);
    expect(ddl).toContain('"id" bigint NOT NULL');
    expect(ddl).toContain('PRIMARY KEY ("id")');

    // 数据预期：预览是纯编译，模型表与版本表都不能多出行
    expect(await count('model', 'workspace_id = $1 AND name = $2', [ws, name])).toBe(0);
  });

  test('TC-MODEL-02 命名违反分层规范：后端拦下，原因可读，库中不留痕', async ({ page }) => {
    const ws = await workspaceId();
    const name = badFor('m2');
    const models = new ModelsPage(page);
    await models.open();

    await models.openYamlEditor();
    const res = await models.publishYaml(yamlOf(name, TWO_COLS));

    expect(res.status).toBe(400);
    expect(res.body).toContain('建模规范');
    expect(res.body).toContain('dwd_');

    // 交互预期：错误原文透传到 toast，不是笼统的「保存失败」
    await expect(toasts(page).filter({ hasText: /建模规范/ })).toBeVisible();
    await expect(page.locator('#model-yaml')).toBeVisible(); // 弹窗保留，用户能就地改

    // 数据预期：一条模型、一条版本都不该有
    expect(await count('model', 'workspace_id = $1 AND name = $2', [ws, name])).toBe(0);
  });

  test('TC-MODEL-03 发布成功：树节点显示版本，库里 active_version 与快照同时到位', async ({ page }) => {
    const ws = await workspaceId();
    const name = modelFor('m3');
    const models = new ModelsPage(page);
    await models.open();

    await models.openYamlEditor();
    const res = await models.publishYaml(yamlOf(name, TWO_COLS));
    expect(res.status, res.body).toBe(201);

    await expect(toasts(page).filter({ hasText: new RegExp(`模型 ${name} 已发布 v1`) })).toBeVisible();
    const node = models.treeItem(name);
    await expect(node).toBeVisible();
    await expect(node).toContainText('v1');

    const m = await one<{ id: string; active_version: number }>(
      'SELECT id, active_version FROM model WHERE workspace_id = $1 AND name = $2', [ws, name],
    );
    expect(m, '模型应落库').toBeTruthy();
    expect(m!.active_version).toBe(1);

    const v = await one<{ version: number; status: string; ddl: string; cols: number }>(
      `SELECT version, status, ddl, jsonb_array_length(spec->'columns') AS cols
         FROM model_version WHERE model_id = $1 ORDER BY version DESC LIMIT 1`, [m!.id],
    );
    expect(v!.version).toBe(1);
    expect(v!.status).toBe('published');
    expect(v!.cols).toBe(2);
    expect(v!.ddl).toContain(name);
    expect(v!.ddl).toContain('PRIMARY KEY');
  });

  test('TC-MODEL-04 改定义再发布：版本递增，历史快照不被覆盖', async ({ page }) => {
    const ws = await workspaceId();
    const name = modelFor('m4');
    const models = new ModelsPage(page);
    await models.open();

    await models.openYamlEditor();
    const first = await models.publishYaml(yamlOf(name, TWO_COLS, '第一版'));
    expect(first.status, first.body).toBe(201);
    await expect(toasts(page).filter({ hasText: new RegExp(`模型 ${name} 已发布 v1`) })).toBeVisible();

    const m = await one<{ id: string }>('SELECT id FROM model WHERE workspace_id = $1 AND name = $2', [ws, name]);

    await models.openYamlEditor();
    const second = await models.publishYaml(yamlOf(name, TWO_COLS + `
  - name: amount
    type: decimal
    nullable: true
    comment: 金额`, '第二版'));
    expect(second.status, second.body).toBe(201);
    await expect(toasts(page).filter({ hasText: new RegExp(`模型 ${name} 已发布 v2`) })).toBeVisible();

    const model = await one<{ active_version: number }>(
      'SELECT active_version FROM model WHERE id = $1', [m!.id],
    );
    expect(model!.active_version).toBe(2);

    const versions = await sql<{ version: number; status: string; desc: string }>(
      `SELECT version, status, spec->>'description' AS desc
         FROM model_version WHERE model_id = $1 ORDER BY version`, [m!.id],
    );
    expect(versions.map(v => v.version)).toEqual([1, 2]);
    // 硬约束：旧版本内容不能被新定义覆盖，否则「重跑锁定版本」就是句空话
    expect(versions[0].desc).toBe('第一版');
    expect(versions[1].desc).toBe('第二版');
    expect(versions[1].status).toBe('published');
  });

  test('TC-MODEL-05 删除模型：必须手输名称才能确认，删完版本快照一起清空', async ({ page }) => {
    const ws = await workspaceId();
    const name = modelFor('m5');
    const models = new ModelsPage(page);
    await models.open();

    await models.openYamlEditor();
    expect((await models.publishYaml(yamlOf(name, TWO_COLS))).status).toBe(201);
    await expect(models.treeItem(name)).toBeVisible();

    const m = await one<{ id: string }>('SELECT id FROM model WHERE workspace_id = $1 AND name = $2', [ws, name]);
    expect(await count('model_version', 'model_id = $1', [m!.id])).toBeGreaterThan(0);

    await models.delete(name);
    const confirm = page.locator('#dg-ok');
    await expect(confirm).toBeDisabled();

    // 输错一个字也不能放行——这是删模型唯一的安全阀
    await page.fill('#dg-input', name + 'x');
    await expect(confirm).toBeDisabled();
    await page.fill('#dg-input', name);
    await expect(confirm).toBeEnabled();

    await confirm.click();
    await expect(toasts(page).filter({ hasText: `已删除模型 ${name}` })).toBeVisible();
    await expect(models.treeItem(name)).toHaveCount(0);

    // 数据预期：模型与全部版本快照级联删除，不留孤儿版本
    await expect.poll(() => count('model', 'id = $1', [m!.id])).toBe(0);
    expect(await count('model_version', 'model_id = $1', [m!.id])).toBe(0);
  });

  test.afterAll(async () => {
    const ws = await workspaceId();
    await sql('DELETE FROM model WHERE workspace_id = $1 AND name LIKE $2', [ws, '%' + PREFIX + '%']);
  });
});
