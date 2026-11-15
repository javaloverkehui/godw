import { expect, Locator, Page } from '@playwright/test';
import { gotoRoute, readTable, rowByText, waitApi } from './ui';

/**
 * 页面对象：所有选择器只出现在这里。
 *
 * 改版时只需要改本文件；用例里写的是「点接入数据源」这样的业务语言，
 * 不写 CSS。选择器一律用 data-act / 可见文案 / 表单 id，
 * 不用层级路径，布局调整不会误伤。
 */

export class OverviewPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'overview'); }

  /** 顶部四张统计卡：返回 { 数据源: '2', 模型: '8', ... } */
  async stats(): Promise<Record<string, string>> {
    const out: Record<string, string> = {};
    for (const card of await this.page.locator('#view .stat').all()) {
      const label = (await card.locator('.stat-label').innerText()).trim();
      const value = (await card.locator('.stat-value').innerText()).trim();
      out[label] = value;
    }
    return out;
  }

  pipelineStrip() { return this.page.locator('#pipeline-strip .row'); }
  recentRows() { return this.page.locator('#view table tbody tr'); }
}

export class SourcesPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'sources'); }

  row(name: string): Locator { return rowByText(this.page, name); }

  async openCreate(): Promise<void> {
    await this.page.click('[data-act="new-ds"]');
    await expect(this.page.locator('.modal')).toBeVisible();
  }

  /** 填表并保存，返回后端响应状态与 body。 */
  async create(input: {
    name: string; type?: string; host?: string; port?: string;
    database?: string; user?: string; password?: string;
  }): Promise<{ status: number; body: string }> {
    await this.openCreate();
    if (input.name) await this.page.fill('#ds-name', input.name);
    // 表单默认选中第一项（clickhouse），必须显式选，否则会拿错引擎去连库
    await this.page.selectOption('#ds-type', input.type ?? 'postgres');
    await this.page.fill('#ds-host', input.host ?? '127.0.0.1');
    await this.page.fill('#ds-port', input.port ?? '5432');
    await this.page.fill('#ds-db', input.database ?? 'godw');
    await this.page.fill('#ds-user', input.user ?? 'godw');
    await this.page.fill('#ds-pass', input.password ?? 'godw');
    const resp = waitApi(this.page, '/datasources');
    await this.page.click('[data-act="save-ds"]');
    const r = await resp;
    return { status: r.status(), body: await r.text() };
  }

  async test(name: string): Promise<void> {
    await this.row(name).locator('[data-act="test-ds"]').click();
  }

  async collect(name: string): Promise<void> {
    await this.row(name).locator('[data-act="collect-ds"]').click();
  }

  async remove(name: string): Promise<void> {
    await this.row(name).locator('[data-act="del-ds"]').click();
  }
}

export class ModelsPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'models'); }

  treeItem(name: string): Locator { return this.page.locator(`#m-tree [data-model="${name}"]`); }

  /** 走「新建模型 → 手写定义」，这是唯一能精确控制 YAML 的入口。 */
  async openYamlEditor(): Promise<void> {
    await this.page.click('[data-act="m-new"]');
    await this.page.locator('.wiz-choice[data-mode="blank"]').click();
    await this.page.click('#wiz-next');
    await expect(this.page.locator('#model-yaml')).toBeVisible();
  }

  async previewDDL(): Promise<string> {
    await this.page.click('[data-act="preview-ddl"]');
    const box = this.page.locator('#ddl-preview');
    await expect(box).toContainText('DDL 预览');
    return (await box.innerText()).trim();
  }

  async publishYaml(yaml: string): Promise<{ status: number; body: string }> {
    await this.page.fill('#model-yaml', yaml);
    const resp = waitApi(this.page, '/models', 'POST');
    await this.page.click('[data-act="save-model"]');
    const r = await resp;
    return { status: r.status(), body: await r.text() };
  }

  async delete(name: string): Promise<void> {
    await this.treeItem(name).locator('[data-mdel]').click();
  }

  async check(): Promise<void> { await this.page.click('[data-act="m-check"]'); }
  async publish(): Promise<void> { await this.page.click('[data-act="m-publish"]'); }
}

export class PipelinesPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'pipelines'); }

  item(name: string) { return this.page.locator(`[data-act="sel-pipeline"][data-name="${name}"]`); }
  async select(name: string) { await this.item(name).click(); }

  /** 新建管道向导第一步：勾一个产出模型。 */
  async openWizard() {
    await this.page.click('[data-act="new-pipeline"]');
    await expect(this.page.locator('.modal.wiz')).toBeVisible();
  }

  /** 新建管道向导：勾产出模型 → 确认来源 → 填名与调度 → 发布。 */
  async publishWizard(name: string, opts: { model?: string; schedule?: '手动' } = {}): Promise<void> {
    await this.openWizard();
    await this.page.check(`[data-pick="${opts.model ?? 'dwd_order'}"]`);
    await this.page.click('#wiz-next');
    await expect(this.page.locator('#wiz-body')).toContainText('产出模型');
    await this.page.click('#wiz-next');
    await expect(this.page.locator('#wz-pname')).toBeVisible();

    await this.page.fill('#wz-pname', name);
    if (opts.schedule === '手动') {
      await this.page.locator('#wiz-body').getByText('手动', { exact: true }).click();
    }
    const runNow = this.page.locator('#wz-runnow');
    if (await runNow.isChecked()) await runNow.uncheck();

    const resp = this.page.waitForResponse(r => r.url().endsWith('/api/v1/pipelines') && r.request().method() === 'POST');
    await this.page.locator('.modal button', { hasText: '发布管道' }).click();
    await resp;
  }

  async toggle(name: string) {
    await this.page.locator(`[data-act="toggle-pl"][data-name="${name}"]`).click();
  }

  async trigger(name: string) {
    await this.page.locator(`[data-act="trigger-pl"][data-name="${name}"]`).click();
  }

  /** 编辑管道：改描述后「保存并发布」，会产生新版本。 */
  async editDescription(name: string, desc: string): Promise<{ status: number; body: string }> {
    await this.page.locator(`[data-act="edit-pipeline"][data-name="${name}"]`).click();
    await expect(this.page.locator('#pe-desc')).toBeVisible();
    await this.page.fill('#pe-desc', desc);
    const resp = this.page.waitForResponse(r => r.url().endsWith('/api/v1/pipelines') && r.request().method() === 'POST');
    await this.page.locator(`[data-act="save-pipeline"][data-name="${name}"]`).click();
    const r = await resp;
    return { status: r.status(), body: await r.text() };
  }

  nodeRows() { return this.page.locator('#view table').last().locator('tbody tr'); }
}

export class InstancesPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'instances'); }

  rows() { return this.page.locator('#view tr.inst-row'); }
  totalText() { return this.page.locator('#view .filter-bar .hint', { hasText: '共' }).first(); }

  async filterStatus(status: string) {
    await this.page.selectOption('#fq-status', status);
    await this.page.waitForFunction(() => !document.querySelector('#view')?.innerText.includes('加载中…'));
  }

  async filterPipeline(name: string) {
    await this.page.selectOption('#fq-pipeline', name);
    await this.page.waitForFunction(() => !document.querySelector('#view')?.innerText.includes('加载中…'));
  }

  async openDetail(id: string) {
    await this.page.locator(`[data-act="open-instance"][data-id="${id}"]`).first().click();
    await expect(this.page.locator('.drawer')).toBeVisible();
  }

  async retry(id: string) {
    await this.page.locator(`[data-act="retry-inst"][data-id="${id}"]`).first().click();
  }

  async switchView(label: '列表' | '矩阵') {
    await this.page.locator('[data-act="inst-view"]').filter({ hasText: label }).click();
    await this.page.waitForFunction(() => !document.querySelector('#view')?.innerText.includes('加载中…'));
  }

  matrixCells() { return this.page.locator('#view .matrix-cell, #view table.matrix td'); }
}

export class SqlPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'sql'); }

  async run(query: string, opts: { datasource?: string; limit?: string } = {}) {
    if (opts.datasource) await this.page.selectOption('#sql-ds', opts.datasource);
    if (opts.limit) await this.page.selectOption('#sql-limit', opts.limit);
    await this.page.fill('#sql-editor textarea', query);
    const resp = waitApi(this.page, '/sql/query');
    await this.page.click('[data-act="sql-run"]');
    const r = await resp;
    return { status: r.status(), body: await r.text() };
  }

  result() { return this.page.locator('#sql-result'); }
  async resultTable() { return readTable(this.page, '#sql-result'); }
  errorBox() { return this.page.locator('#sql-result .err-box'); }
}

export class GovernancePage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'governance'); }

  async tab(label: string) {
    await this.page.locator('.tab').filter({ hasText: label }).click();
    await this.page.waitForFunction(() => !document.querySelector('#view')?.innerText.includes('加载中…'));
  }

  async addColumnPolicy(p: { table: string; column: string; classification: string; mask: string }) {
    await this.page.click('[data-act="new-col-policy"]');
    await this.page.fill('#cp-table', p.table);
    await this.page.fill('#cp-col', p.column);
    await this.page.selectOption('#cp-class', p.classification);
    await this.page.selectOption('#cp-mask', p.mask);
    const resp = waitApi(this.page, '/policies/columns');
    await this.page.click('[data-act="save-col-policy"]');
    const r = await resp;
    return { status: r.status(), body: await r.text() };
  }

  async addRowPolicy(p: { table: string; role: string; filter: string; desc?: string }) {
    await this.page.click('[data-act="new-row-policy"]');
    await this.page.fill('#rp-table', p.table);
    await this.page.selectOption('#rp-role', p.role);
    await this.page.fill('#rp-filter', p.filter);
    if (p.desc) await this.page.fill('#rp-desc', p.desc);
    const resp = waitApi(this.page, '/policies/rows');
    await this.page.click('[data-act="save-row-policy"]');
    const r = await resp;
    return { status: r.status(), body: await r.text() };
  }

  async createShare(p: { kind: string; target: string; ttl: string }) {
    await this.page.click('[data-act="new-share"]');
    await this.page.selectOption('#sh-kind', p.kind);
    await this.page.fill('#sh-target', p.target);
    await this.page.fill('#sh-ttl', p.ttl);
    const resp = waitApi(this.page, '/shares');
    await this.page.click('[data-act="save-share"]');
    const r = await resp;
    return { status: r.status(), body: await r.text() };
  }
}

export class QualityPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'quality'); }
  stats() { return this.page.locator('#view .stat'); }
  summaryRows() { return this.page.locator('#view table').first().locator('tbody tr'); }
}

export class AuditPage {
  constructor(private page: Page) {}
  async open() { await gotoRoute(this.page, 'audit'); }
  rows() { return this.page.locator('#view table tbody tr'); }
  first() { return this.rows().first(); }
}
