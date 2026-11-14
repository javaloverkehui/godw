#!/usr/bin/env node
/**
 * 编排画布的规模回归：首屏渲染、拖动帧率、缩放帧率、DOM 规模。
 *
 * 为什么要有它：500 节点「卡不卡」用眼睛看只能得到「还行」这种结论，
 * 改一次布局代码就没法判断是变好还是变坏。这里把四个指标量化成可比的数，
 * 并对「视口裁剪是否真的生效」给出硬断言（DOM 节点数必须明显少于总数）。
 *
 * 前置：
 *   1. 服务已启动（默认 http://127.0.0.1:8080）
 *   2. 已发布夹具管道：python scripts/gen_dag.py 500 --out /tmp/dag500.yaml
 *                     godw-cli apply -f /tmp/dag500.yaml --publish
 * 用法：
 *   node scripts/bench_canvas.mjs --url http://127.0.0.1:8080 --pipeline dag_scale_fixture
 *   node scripts/bench_canvas.mjs --json        # 只输出机器可读结果
 * 退出码：断言全过为 0，否则为 1。
 */
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const args = process.argv.slice(2);
const opt = (name, def) => {
  const i = args.indexOf('--' + name);
  return i >= 0 && args[i + 1] && !args[i + 1].startsWith('--') ? args[i + 1] : def;
};
const flag = name => args.includes('--' + name);

const BASE = opt('url', 'http://127.0.0.1:8080').replace(/\/$/, '');
const PIPELINE = opt('pipeline', 'dag_scale_fixture');
const EXPECT_NODES = Number(opt('nodes', '500'));
const JSON_ONLY = flag('json');

const CHROME_CANDIDATES = [
  process.env.CHROME,
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].filter(Boolean);

function findChrome() {
  for (const p of CHROME_CANDIDATES) {
    try { if (fs.existsSync(p)) return p; } catch { /* ignore */ }
  }
  throw new Error('找不到 Chrome，可用 CHROME=/path/to/chrome 指定');
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function waitFor(fn, timeoutMs, label) {
  const start = Date.now();
  for (;;) {
    try {
      const v = await fn();
      if (v) return v;
    } catch { /* 还没起来 */ }
    if (Date.now() - start > timeoutMs) throw new Error('等待超时: ' + label);
    await sleep(100);
  }
}

class CDP {
  constructor(ws) { this.ws = ws; this.seq = 0; this.pending = new Map(); }

  static async connect(url) {
    const socket = new WebSocket(url);
    await new Promise((res, rej) => {
      socket.onopen = res;
      socket.onerror = () => rej(new Error('CDP 连接失败'));
    });
    const cdp = new CDP(socket);
    socket.addEventListener('message', ev => {
      const msg = JSON.parse(ev.data);
      if (!msg.id) return;
      const p = cdp.pending.get(msg.id);
      if (!p) return;
      cdp.pending.delete(msg.id);
      if (msg.error) p.reject(new Error(msg.error.message));
      else p.resolve(msg.result);
    });
    return cdp;
  }

  send(method, params) {
    const id = ++this.seq;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params: params || {} }));
    });
  }

  async eval(expression) {
    const r = await this.send('Runtime.evaluate', {
      expression, awaitPromise: true, returnByValue: true,
    });
    if (r.exceptionDetails) {
      throw new Error('页面脚本异常: ' + (r.exceptionDetails.exception?.description || r.exceptionDetails.text));
    }
    return r.result.value;
  }
}

// 页面内测量。整段在浏览器里跑，才能拿到真实的帧间隔与单帧耗时。
//
// 两个数缺一不可：
//   work  —— 事件处理本身花了多久（我们的代码成本），这是「能不能跑满 60fps」的判据；
//   frame —— 相邻两帧的间隔（浏览器节奏），它包含 vsync 与合成，天然有 1~2ms 抖动，
//            只能用来发现「持续掉帧」，不能拿 16.7ms 当硬阈值。
const measureScript = (pipeline, expectNodes) => `(async () => {
  const frame = () => new Promise(r => requestAnimationFrame(r));
  const stats = arr => {
    const s = arr.slice().sort((a, b) => a - b);
    const at = q => s[Math.min(s.length - 1, Math.floor(q * s.length))];
    return { p50: at(0.5), p95: at(0.95), max: s[s.length - 1] };
  };
  const round = o => ({ p50: +o.p50.toFixed(2), p95: +o.p95.toFixed(2), max: +o.max.toFixed(2) });

  // 1) 首屏：切到夹具管道，等两帧确保布局稳定
  const t0 = performance.now();
  S.selected.canvasPipe = ${JSON.stringify(pipeline)};
  await render();
  await frame(); await frame();
  const renderMs = performance.now() - t0;

  const domNodes = document.querySelectorAll('#cv-canvas .cv-node').length;
  const edgePaths = document.querySelectorAll('#cv-edges path.cv-edge').length;
  const totalNodes = (typeof CANVAS !== 'undefined' && CANVAS.nodes) ? CANVAS.nodes.length : 0;

  const nodeEl = document.querySelector('#cv-canvas .cv-node');
  if (!nodeEl) return { error: '画布没有节点，夹具管道可能未发布' };
  const box = nodeEl.getBoundingClientRect();
  const startX = box.left + box.width / 2, startY = box.top + box.height / 2;

  // 2) 拖动：真实 mousedown → 60 次 mousemove → mouseup
  nodeEl.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, clientX: startX, clientY: startY, button: 0, buttons: 1 }));
  const dragWork = [], dragFrame = [];
  let last = performance.now();
  for (let i = 1; i <= 60; i++) {
    const t = performance.now();
    window.dispatchEvent(new MouseEvent('mousemove', {
      bubbles: true, clientX: startX + i * 4, clientY: startY + i * 2, button: 0, buttons: 1,
    }));
    dragWork.push(performance.now() - t);
    await frame();
    const now = performance.now();
    dragFrame.push(now - last);
    last = now;
  }
  window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, clientX: startX + 240, clientY: startY + 120, button: 0 }));
  await frame();

  // 3) 缩放：30 次滚轮（先放大再缩小）
  const stage = document.getElementById('cv-stage');
  const stageBox = stage.getBoundingClientRect();
  const zoomWork = [], zoomFrame = [];
  last = performance.now();
  for (let i = 0; i < 30; i++) {
    const t = performance.now();
    stage.dispatchEvent(new WheelEvent('wheel', {
      bubbles: true, cancelable: true, deltaY: i < 15 ? -120 : 120,
      clientX: stageBox.left + 400, clientY: stageBox.top + 300,
    }));
    zoomWork.push(performance.now() - t);
    await frame();
    const now = performance.now();
    zoomFrame.push(now - last);
    last = now;
  }
  await new Promise(r => setTimeout(r, 200)); // 等尾帧补画
  await frame();
  const zoomNodes = document.querySelectorAll('#cv-canvas .cv-node').length;

  // 4) 平移：空格 + 拖拽画布（空格键必须带 code，处理函数按 code 判断）
  document.dispatchEvent(new KeyboardEvent('keydown', { code: 'Space', key: ' ', bubbles: true }));
  const panWork = [], panFrame = [];
  stage.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, clientX: 700, clientY: 450, button: 0, buttons: 1 }));
  last = performance.now();
  for (let i = 1; i <= 40; i++) {
    const t = performance.now();
    window.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, clientX: 700 - i * 8, clientY: 450, button: 0, buttons: 1 }));
    panWork.push(performance.now() - t);
    await frame();
    const now = performance.now();
    panFrame.push(now - last);
    last = now;
  }
  window.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, clientX: 380, clientY: 450, button: 0 }));
  document.dispatchEvent(new KeyboardEvent('keyup', { code: 'Space', key: ' ', bubbles: true }));
  await new Promise(r => setTimeout(r, 200));
  await frame();
  const panNodes = document.querySelectorAll('#cv-canvas .cv-node').length;
  const panScale = CANVAS.view.s;

  // 5) 反复进入画布 8 次：历史上 window 监听器每进一次就多挂一套，
  //    第 8 次拖动从 159fps 掉到 23fps。这里用「每次重建页面的耗时」当代理指标。
  const enterMs = [];
  for (let i = 0; i < 8; i++) {
    const t = performance.now();
    S.selected.canvasPipe = ${JSON.stringify(pipeline)};
    await render();
    await frame();
    enterMs.push(+(performance.now() - t).toFixed(1));
  }

  const trim = a => a.slice(5); // 丢掉前 5 帧的冷启动
  return {
    totalNodes, expectNodes: ${expectNodes},
    renderMs: Math.round(renderMs * 10) / 10,
    domNodes, edgePaths, zoomNodes, panNodes, panScale: +panScale.toFixed(2),
    enterMs, enterFirst: enterMs[0], enterLast: enterMs[enterMs.length - 1],
    dragWork: round(stats(trim(dragWork))), dragFrame: round(stats(trim(dragFrame))),
    zoomWork: round(stats(trim(zoomWork))), zoomFrame: round(stats(trim(zoomFrame))),
    panWork: round(stats(trim(panWork))), panFrame: round(stats(trim(panFrame))),
  };
})()`;

async function main() {
  const chromePath = findChrome();
  const port = 9400 + Math.floor(Math.random() * 300);
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'godw-bench-'));
  const chrome = spawn(chromePath, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`,
    '--window-size=1440,900', 'about:blank',
  ], { stdio: 'ignore' });

  const cleanup = () => {
    try { chrome.kill(); } catch { /* ignore */ }
    try { fs.rmSync(profile, { recursive: true, force: true }); } catch { /* ignore */ }
  };
  process.on('exit', cleanup);

  try {
    await waitFor(async () => (await fetch(`http://127.0.0.1:${port}/json/version`)).ok, 15000, 'Chrome 调试端口');
    const tab = await (await fetch(`http://127.0.0.1:${port}/json/new?${encodeURIComponent(BASE + '/#/canvas')}`, { method: 'PUT' })).json();
    const cdp = await CDP.connect(tab.webSocketDebuggerUrl);
    await cdp.send('Page.enable');
    await cdp.send('Runtime.enable');
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
    await sleep(1500);

    const res = await cdp.eval(measureScript(PIPELINE, EXPECT_NODES));
    if (res.error) throw new Error(res.error);

    // 断言：这些数字就是「优化真的生效」的判据，不是参考值。
    const checks = [
      { name: '拖动单帧耗时 p95 < 16.7ms', ok: res.dragWork.p95 < 16.7, got: res.dragWork.p95 + 'ms' },
      { name: '缩放单帧耗时 p95 < 16.7ms', ok: res.zoomWork.p95 < 16.7, got: res.zoomWork.p95 + 'ms' },
      { name: '平移单帧耗时 p95 < 16.7ms', ok: res.panWork.p95 < 16.7, got: res.panWork.p95 + 'ms' },
      { name: '拖动帧间隔 p95 < 33ms（无持续掉帧）', ok: res.dragFrame.p95 < 33, got: res.dragFrame.p95 + 'ms' },
      { name: '缩放帧间隔 p95 < 33ms', ok: res.zoomFrame.p95 < 33, got: res.zoomFrame.p95 + 'ms' },
      { name: 'DOM 节点 < 总数 40%', ok: res.domNodes < res.totalNodes * 0.4, got: `${res.domNodes}/${res.totalNodes}` },
      { name: '边 path ≤ 2 条（已合并）', ok: res.edgePaths <= 2, got: String(res.edgePaths) },
      { name: '首屏 < 1500ms', ok: res.renderMs < 1500, got: res.renderMs + 'ms' },
      {
        name: '进出画布 8 次首帧不劣化 > 20%',
        ok: res.enterLast <= Math.max(res.enterFirst * 1.2, 80),
        got: `${res.enterFirst}ms → ${res.enterLast}ms`,
      },
    ];
    const failed = checks.filter(c => !c.ok);

    if (JSON_ONLY) {
      console.log(JSON.stringify({ ...res, checks, failed: failed.map(f => f.name) }, null, 2));
    } else {
      console.log(`\n编排画布 · ${PIPELINE}（${res.totalNodes} 节点）`);
      console.log(`  首屏渲染      ${res.renderMs} ms`);
      console.log(`  DOM 节点      ${res.domNodes} / ${res.totalNodes}（${(res.domNodes / res.totalNodes * 100).toFixed(0)}%）`);
      console.log(`  边 path       ${res.edgePaths} 条`);
      console.log(`  拖动          work p50 ${res.dragWork.p50}ms p95 ${res.dragWork.p95}ms | frame p95 ${res.dragFrame.p95}ms`);
      console.log(`  缩放          work p50 ${res.zoomWork.p50}ms p95 ${res.zoomWork.p95}ms | frame p95 ${res.zoomFrame.p95}ms · 缩放后 DOM ${res.zoomNodes}`);
      console.log(`  平移          work p50 ${res.panWork.p50}ms p95 ${res.panWork.p95}ms | frame p95 ${res.panFrame.p95}ms · 平移后 DOM ${res.panNodes} · 比例 ${res.panScale}`);
      console.log(`  进出 8 次     首 ${res.enterFirst}ms → 末 ${res.enterLast}ms  [${res.enterMs.join(', ')}]`);
      console.log('');
      for (const c of checks) console.log(`  ${c.ok ? '✓' : '✗'} ${c.name}（${c.got}）`);
    }
    process.exitCode = failed.length ? 1 : 0;
  } finally {
    cleanup();
  }
}

main().catch(e => {
  console.error('基准失败:', e.message);
  process.exitCode = 1;
});
