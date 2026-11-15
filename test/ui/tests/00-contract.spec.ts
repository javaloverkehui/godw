import { test, expect } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { APP } from '../src/config';

/**
 * UI 契约自检 —— 让「脚本随 UI 变化而变化」这件事有落点。
 *
 * 思路：测试里用到的每个 data-act 与表单 id，都必须在当前前端产物里真实存在。
 * 前端一旦改了动作名/字段名，这条用例会先红，并直接告诉你去改哪个文件，
 * 而不是等到十几个业务用例以「元素找不到」的形式集体失败。
 */
const ROOTS = [path.join(__dirname, '..', 'src'), path.join(__dirname)];

function collectSources(dir: string, out: string[] = []): string[] {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) collectSources(p, out);
    else if (/\.ts$/.test(entry.name)) out.push(p);
  }
  return out;
}

function extract(pattern: RegExp, text: string): string[] {
  const found: string[] = [];
  for (const m of text.matchAll(pattern)) found.push(m[1]);
  return found;
}

test.describe('UI 契约', () => {
  test('TC-CONTRACT-01 脚本引用的 data-act 与表单 id 在产物中全部存在', async ({ request }) => {
    const index = await (await request.get('/')).text();
    const scripts = extract(/<script src="([^"]+)"/g, index);
    expect(scripts.length, '首页应加载前端脚本').toBeGreaterThan(3);

    const bundle = (await Promise.all(
      scripts.map(async s => (await request.get(s)).text()),
    )).join('\n');

    const files = ROOTS.flatMap(d => collectSources(d)).filter(f => !f.endsWith('00-contract.spec.ts'));
    const acts = new Set<string>();
    const ids = new Set<string>();

    for (const file of files) {
      const src = fs.readFileSync(file, 'utf8');
      extract(/data-act="([a-z0-9-]+)"/g, src).forEach(a => acts.add(a));
      // 只取字面量 id，模板拼接的（含 ${}）不参与契约，避免误报
      extract(/(?:fill|selectOption)\(['"]#([A-Za-z][\w-]*)['"]/g, src).forEach(i => ids.add(i));
      extract(/locator\(['"]#([A-Za-z][\w-]*)['"]\)/g, src).forEach(i => ids.add(i));
      extract(/expect\(page\.locator\('([A-Za-z][^']*)'\)\)/g, src).forEach(sel => {
        const m = sel.match(/^#([\w-]+)/);
        if (m) ids.add(m[1]);
      });
    }

    const missingActs = [...acts].filter(a => !bundle.includes(`data-act="${a}"`) && !bundle.includes(`case '${a}'`));
    const missingIds = [...ids].filter(i => !bundle.includes(`id="${i}"`) && !bundle.includes(`'${i}'`));

    expect(missingActs, `前端已不存在这些动作，请更新 src/pages.ts：${missingActs.join(', ')}`).toEqual([]);
    expect(missingIds, `前端已不存在这些字段 id，请更新 src/pages.ts：${missingIds.join(', ')}`).toEqual([]);

    console.log(`[contract] 校验 ${acts.size} 个动作、${ids.size} 个字段 id，全部命中当前产物`);
  });

  test('TC-CONTRACT-02 首页脚本与样式全部可加载，控制台无 404', async ({ page }) => {
    const failed: string[] = [];
    page.on('response', r => {
      if (r.url().startsWith(APP.baseURL) && r.status() >= 400) failed.push(`${r.status()} ${r.url()}`);
    });
    const errors: string[] = [];
    page.on('pageerror', e => errors.push(e.message));

    await page.goto(APP.baseURL);
    await expect(page.locator('.nav-item').first()).toBeVisible();

    expect(failed, `静态资源或接口 4xx/5xx：\n${failed.join('\n')}`).toEqual([]);
    expect(errors, `页面脚本异常：\n${errors.join('\n')}`).toEqual([]);
  });
});
