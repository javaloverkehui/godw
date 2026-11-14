/* GoDW 控制台 · 管道可视化编排（T0905）
 *
 * 视口模型：{tx, ty, s} 作用在 .cv-canvas 的 transform 上，节点坐标始终是世界坐标，
 * 因此缩放只影响呈现，不动数据——保存回 YAML 时不会带上缩放带来的误差。
 *
 * 交互清单：
 *   滚轮缩放（指针为锚点）/ 空格或中键平移 / 空白拖拽框选 / Shift 加选
 *   节点面板拖入或点击添加 / 右侧属性面板随选中切换 / 端口连线（客户端先拦成环）
 *   Ctrl+Z 撤销、Ctrl+Shift+Z 或 Ctrl+Y 重做、Delete 删除、Ctrl+A 全选、Ctrl+S 保存
 *   0 适应视图、+/- 缩放、Esc 取消选择
 *
 * 运行态：选中某个实例后，节点上叠加状态点、耗时与失败角标；点节点可在属性面板看到该次运行明细。
 */
'use strict';

const CV_NODE_W = 168, CV_NODE_H = 64;

const CV_TYPES = [
  { type: 'source', label: '采集', color: '#7d8590', desc: '从数据源抽数落 ODS' },
  { type: 'sql', label: '转换', color: '#4f8cff', desc: 'SELECT 写入目标模型' },
  { type: 'quality', label: '质量', color: '#d29922', desc: '规则门禁，error 断下游' },
  { type: 'branch', label: '分支', color: '#a371f7', desc: '按条件跳过节点' },
  { type: 'sink', label: '写出', color: '#39c5cf', desc: '写文件或外部接口' },
  { type: 'shell', label: '脚本', color: '#8b949e', desc: '执行本地命令' },
  { type: 'http', label: '请求', color: '#f85149', desc: '回调外部 HTTP' },
];

// 分层色：与血缘图共用同一套语义，避免同一个层在两个页面是两种颜色。
const CV_LAYER_COLOR = { ods: '#7d8590', dwd: '#4f8cff', dws: '#39c5cf', ads: '#d29922' };

const CANVAS = {
  pipe: null,
  pipes: [],
  nodes: [],
  edges: [],
  view: { tx: 32, ty: 32, s: 1 },
  sel: new Set(),
  connectFrom: null,
  run: null,          // { inst, tasks: {nodeId: task} }
  instList: [],
  layerOf: {},
  undo: [],
  redo: [],
  dirty: false,
  drag: null,
  marquee: null,
  pan: null,
  spaceDown: false,
};

Object.assign(ROUTES, {
  canvas: { title: '编排', sub: '可视化编辑管道 DAG（YAML 仍是唯一事实源）', render: pageCanvas },
});

/* ---------------- 页面 ---------------- */

async function pageCanvas(view) {
  const [pipes, models, insts] = await Promise.all([
    api('/pipelines').catch(() => []),
    api('/models').catch(() => []),
    api('/instances?limit=60').then(r => r.items || []).catch(() => []),
  ]);
  CANVAS.pipes = pipes || [];
  CANVAS.instList = (insts || []).slice().sort((a, b) =>
    String(b.createdAt || '').localeCompare(String(a.createdAt || '')));

  const layerOf = {};
  (models || []).forEach(m => {
    layerOf[m.name] = (m.spec && m.spec.layer) || cvGuessLayer(m.name);
  });
  CANVAS.layerOf = layerOf;

  const cur = S.selected.canvasPipe || (CANVAS.pipes[0] && CANVAS.pipes[0].name) || '';
  const p = CANVAS.pipes.find(x => x.name === cur) || CANVAS.pipes[0] || null;
  S.selected.canvasPipe = p ? p.name : '';

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">编排</h2>
        <p class="page-desc">拖拽移动、拖端口连线、滚轮缩放、空格平移。保存时序列化为 YAML 并校验，与手写 YAML 完全等价。</p>
      </div>
      <div class="row">
        <select id="cv-pipe" style="width:200px">
          ${CANVAS.pipes.map(x => `<option value="${esc(x.name)}" ${x.name === (p && p.name) ? 'selected' : ''}>${esc(x.name)}</option>`).join('') || '<option value="">暂无管道</option>'}
        </select>
        <button class="ghost" data-act="cv-undo" title="撤销（Ctrl+Z）">撤销</button>
        <button class="ghost" data-act="cv-redo" title="重做（Ctrl+Shift+Z）">重做</button>
        <button class="ghost" data-act="cv-dryrun" ${p ? '' : 'disabled'}>校验</button>
        <button class="ghost" data-act="cv-autolayout" ${p ? '' : 'disabled'}>自动布局</button>
        <button class="primary" data-act="cv-save" ${p ? '' : 'disabled'}>保存并发布</button>
      </div>
    </div>
    <div class="cv-wrap">
      <aside class="cv-palette">
        <div class="cv-pal-title">节点</div>
        ${CV_TYPES.map(t => `
          <div class="cv-pal-item" data-pal="${t.type}" title="拖入画布或点击添加">
            <span class="cv-pal-dot" style="background:${t.color}"></span>
            <div>
              <div class="cv-pal-label">${t.label} <span class="mono" style="font-size:10.5px;color:var(--text-mute)">${t.type}</span></div>
              <div class="cv-pal-desc">${t.desc}</div>
            </div>
          </div>`).join('')}
        <div class="cv-pal-tip">
          <kbd>空格</kbd> 拖动画布<br />
          <kbd>滚轮</kbd> 缩放<br />
          <kbd>Shift</kbd> 加选 / 框选追加<br />
          <kbd>Del</kbd> 删除　<kbd>Ctrl+Z</kbd> 撤销<br />
          <kbd>0</kbd> 适应视图
        </div>
      </aside>

      <div class="cv-main">
        <div class="cv-toolbar">
          <span class="hint" id="cv-meta">${p ? `节点 ${CANVAS.nodes.length}` : '选择管道开始编辑'}</span>
          <span class="spacer"></span>
          <span class="hint" id="cv-status"></span>
          <select id="cv-inst" title="叠加某次运行的状态"></select>
        </div>
        <div class="cv-stage" id="cv-stage">
          <div class="cv-canvas" id="cv-canvas">
            <svg class="cv-edges" id="cv-edges"></svg>
          </div>
          <div class="cv-minimap" id="cv-minimap" title="点击或拖动可快速定位">
            <svg id="cv-map-svg" width="168" height="112">
              <g id="cv-map-nodes"></g>
              <rect id="cv-map-vp" fill="rgba(79,140,255,.10)" stroke="var(--accent)" stroke-width="1" rx="2"></rect>
            </svg>
          </div>
          <div class="cv-zoom">
            <button data-act="cv-zoom-out" title="缩小">−</button>
            <span id="cv-zoom-label">100%</span>
            <button data-act="cv-zoom-in" title="放大">+</button>
            <button data-act="cv-fit" title="适应视图（0）">适应</button>
          </div>
        </div>
      </div>

      <aside class="cv-inspector" id="cv-inspector"></aside>
    </div>`;

  if (!p) {
    document.getElementById('cv-zoom') && (document.getElementById('cv-zoom').style.display = 'none');
    renderInspector();
    return;
  }

  CANVAS.pipe = p;
  loadCanvasFromSpec(p);
  bindStage();
  bindPalette();
  bindKeys();
  cvBindMinimap();
  cvRender();
  renderInspector();
  cvFit(false);
  refreshMeta();
  await syncRun('');
}

/* ---------------- 载入 / 序列化 ---------------- */

function loadCanvasFromSpec(p) {
  const spec = p.spec || {};
  const layout = readLayout(p.name);
  const raw = spec.nodes || [];

  // 没存过布局时按真实依赖分层，避免全堆在左列。
  const lvl = {};
  const byId = {};
  raw.forEach(n => byId[n.id] = n);
  const calc = (id, seen) => {
    if (lvl[id] !== undefined) return lvl[id];
    if (seen.has(id)) return 0;
    seen.add(id);
    const deps = (byId[id] && byId[id].dependsOn) || [];
    lvl[id] = deps.length ? 1 + Math.max(...deps.map(d => calc(d, seen))) : 0;
    return lvl[id];
  };
  raw.forEach(n => calc(n.id, new Set()));

  const rowCount = {};
  CANVAS.nodes = raw.map(n => {
    const l = lvl[n.id] || 0;
    const idx = rowCount[l] = (rowCount[l] || 0);
    rowCount[l] = idx + 1;
    const pos = layout[n.id] || { x: 56 + l * 192, y: 48 + idx * 112 };
    return {
      id: n.id, type: n.type, model: n.model || '', datasource: n.datasource || '',
      sql: n.sql || '', writeMode: n.writeMode || '', dependsOn: (n.dependsOn || []).slice(),
      x: pos.x, y: pos.y,
    };
  });
  CANVAS.edges = [];
  CANVAS.nodes.forEach(n => n.dependsOn.forEach(d => CANVAS.edges.push({ from: d, to: n.id })));
  CANVAS.sel.clear();
  CANVAS.connectFrom = null;
  CANVAS.undo = [];
  CANVAS.redo = [];
  CANVAS.dirty = false;
}

function specFromCanvas() {
  const p = CANVAS.pipe;
  const base = JSON.parse(JSON.stringify(p.spec || {}));
  base.kind = 'Pipeline';
  base.name = p.name;
  base.nodes = CANVAS.nodes.map(n => {
    const spec = { id: n.id, type: n.type };
    if (n.dependsOn && n.dependsOn.length) spec.depends_on = n.dependsOn;
    if (n.datasource) spec.datasource = n.datasource;
    if (n.model) spec.model = n.model;
    if (n.writeMode) spec.write_mode = n.writeMode;
    if (n.sql) spec.sql = n.sql;
    return spec;
  });
  return base;
}

function yamlOf(spec) {
  // 手写序列化：只覆盖管道 DSL 的字段，避免为前端引入 YAML 库。
  const lines = ['kind: Pipeline', 'name: ' + spec.name];
  if (spec.description) lines.push('description: ' + q(spec.description));
  if (spec.schedule) lines.push('schedule: ' + JSON.stringify(spec.schedule));
  if (spec.timezone) lines.push('timezone: ' + spec.timezone);
  if (spec.concurrency) lines.push('concurrency: ' + spec.concurrency);
  if (spec.params && spec.params.length) {
    lines.push('params:');
    spec.params.forEach(pm => {
      lines.push('  - name: ' + pm.name);
      if (pm.type) lines.push('    type: ' + pm.type);
      if (pm.required) lines.push('    required: true');
      if (pm.default !== undefined) lines.push('    default: ' + JSON.stringify(pm.default));
      if (pm.description) lines.push('    description: ' + q(pm.description));
    });
  }
  lines.push('nodes:');
  spec.nodes.forEach(n => {
    lines.push('  - id: ' + n.id);
    lines.push('    type: ' + n.type);
    if (n.depends_on) lines.push('    depends_on: [' + n.depends_on.join(', ') + ']');
    if (n.datasource) lines.push('    datasource: ' + n.datasource);
    if (n.model) lines.push('    model: ' + n.model);
    if (n.write_mode) lines.push('    write_mode: ' + n.write_mode);
    if (n.sql) {
      lines.push('    sql: |');
      n.sql.replace(/\s+$/, '').split('\n').forEach(l => lines.push('      ' + l));
    }
  });
  return lines.join('\n') + '\n';
}

function q(s) {
  return /[:#\[\]{}&*!|>'"%@`]/.test(s) ? JSON.stringify(s) : s;
}

/* ---------------- 渲染 ---------------- */

function cvLayerOf(n) {
  const l = CANVAS.layerOf[n.model];
  if (l) return String(l).toLowerCase();
  return cvGuessLayer(n.model || n.id);
}

function cvGuessLayer(name) {
  const m = String(name || '').toLowerCase();
  if (m.startsWith('ods')) return 'ods';
  if (m.startsWith('dwd')) return 'dwd';
  if (m.startsWith('dws')) return 'dws';
  if (m.startsWith('ads') || m.startsWith('dash')) return 'ads';
  return '';
}

function cvTaskOf(id) {
  return CANVAS.run && CANVAS.run.tasks ? CANVAS.run.tasks[id] : null;
}

function cvEdgePath(a, b) {
  const x1 = a.x + CV_NODE_W, y1 = a.y + CV_NODE_H / 2;
  const x2 = b.x, y2 = b.y + CV_NODE_H / 2;
  if (x2 - x1 >= 20) {
    const mx = x1 + (x2 - x1) / 2;
    return `M ${x1} ${y1} H ${mx} V ${y2} H ${x2}`;
  }
  // 目标在左侧或贴太近：从下方绕行，避免线压住节点正文
  const y = Math.max(a.y, b.y) + CV_NODE_H + 18;
  return `M ${x1} ${y1} H ${x1 + 24} V ${y} H ${x2 - 24} V ${y2} H ${x2}`;
}

// 大图阈值：低于它全量渲染，高于它才启用裁剪与边合并。
// 小图开裁剪只有代价没有收益（每次平移都要重算可见集），而 300 以下全量画一点也不卡。
const CV_CULL_MIN = 300;

// cvViewport 返回当前可视区域的世界坐标（含一个节点宽度的余量）。
// 屏幕坐标 sx = x * s + tx，所以 x = (sx - tx) / s。
function cvViewport() {
  const stage = document.getElementById('cv-stage');
  if (!stage) return null;
  const { tx, ty, s } = CANVAS.view;
  const r = stage.getBoundingClientRect();
  const pad = CV_NODE_W;
  const x1 = -tx / s - pad, y1 = -ty / s - pad;
  return { x1, y1, x2: x1 + r.width / s + pad * 2, y2: y1 + r.height / s + pad * 2 };
}

// 视口键：把位置量化成格子，避免每移动 1px 就整图重绘。
function cvCullKey(vp) {
  const cell = 320;
  return `${Math.floor(vp.x1 / cell)}:${Math.floor(vp.y1 / cell)}:${CANVAS.view.s.toFixed(2)}`;
}

// cvEdgeGeometry 把可见边拼成两条路径（普通 + 高亮）。
// 一条 <path> 可以带多个子路径，marker-end 会画在每个子路径终点，
// 所以箭头不会丢；627 条边从 627 个 DOM 节点变成 2 个。
function cvEdgeGeometry() {
  const byId = {};
  CANVAS.nodes.forEach(n => byId[n.id] = n);
  const vp = CANVAS.cullOn ? cvViewport() : null;
  const visible = n => !vp ||
    (n.x < vp.x2 && n.x + CV_NODE_W > vp.x1 && n.y < vp.y2 && n.y + CV_NODE_H > vp.y1);
  const hot = CANVAS.sel;
  const normal = [], hl = [];
  CANVAS.edges.forEach(e => {
    const a = byId[e.from], b = byId[e.to];
    if (!a || !b) return;
    // 两端都在视口外才丢：只要一端可见，这条线就可能是唯一线索。
    if (vp && !visible(a) && !visible(b)) return;
    const d = cvEdgePath(a, b);
    if (hot.size && (hot.has(e.from) || hot.has(e.to))) hl.push(d);
    else normal.push(d);
  });
  return { normal: normal.join(' '), hot: hl.join(' ') };
}

function cvPaintEdges(svg, g) {
  const paths = [];
  if (g.normal) paths.push(`<path class="cv-edge" d="${g.normal}" marker-end="url(#cv-arrow)"/>`);
  if (g.hot) paths.push(`<path class="cv-edge hl" d="${g.hot}" marker-end="url(#cv-arrow)"/>`);
  svg.innerHTML = `<defs><marker id="cv-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
    <path d="M0,0 L8,4 L0,8 z" fill="var(--accent-dim)"/></marker></defs>${paths.join('')}`;
}

function cvRender() {
  const canvasEl = document.getElementById('cv-canvas');
  if (!canvasEl) return;

  // 大图只渲染视口内的节点：500 个节点全塞进 DOM，拖动时浏览器每帧都要
  // 重新计算所有元素的层与布局，帧率会掉到 10 以下。裁剪之后可视区通常只有几十个。
  CANVAS.cullOn = CANVAS.nodes.length > CV_CULL_MIN;
  let vp = null;
  if (CANVAS.cullOn) {
    vp = cvViewport();
    CANVAS.cullKey = vp ? cvCullKey(vp) : '';
  }
  const visible = n => !vp ||
    (n.x < vp.x2 && n.x + CV_NODE_W > vp.x1 && n.y < vp.y2 && n.y + CV_NODE_H > vp.y1);
  const geometry = cvEdgeGeometry();

  let maxX = 600, maxY = 360;
  CANVAS.nodes.forEach(n => {
    maxX = Math.max(maxX, n.x + CV_NODE_W + 80);
    maxY = Math.max(maxY, n.y + CV_NODE_H + 80);
  });

  const nodeHTML = CANVAS.nodes.filter(visible).map(cvNodeHTML).join('');

  // 一次 innerHTML 重建：SVG 必须和节点在同一次写入里生成，
  // 分两步写会让浏览器先按空画布排版一遍，500 节点时能感觉到闪一下。
  canvasEl.innerHTML =
    `<svg class="cv-edges" id="cv-edges" width="${maxX}" height="${maxY}" viewBox="0 0 ${maxX} ${maxY}"></svg>` +
    nodeHTML;
  const fresh = document.getElementById('cv-edges');
  if (fresh) cvPaintEdges(fresh, geometry);
  // 记住「这一批元素属于哪个画布容器」，视口补画时才知道能不能增量复用。
  CANVAS.nodeEls = new Map();
  canvasEl.querySelectorAll('.cv-node').forEach(el => CANVAS.nodeEls.set(el.dataset.node, el));
  CANVAS.renderedEl = canvasEl;
  cvRenderMinimap();
  applyView();
}

function cvNodeHTML(n) {
  const layer = cvLayerOf(n);
  const t = cvTaskOf(n.id);
  const state = t ? t.status : '';
  const dot = state === 'success' ? 'ok' : state === 'failed' ? 'err'
    : state === 'running' ? 'running' : 'queued';
  const typeMeta = CV_TYPES.find(x => x.type === n.type);
  return `
      <div class="cv-node ${CANVAS.sel.has(n.id) ? 'sel' : ''} ${CANVAS.connectFrom === n.id ? 'connecting' : ''}"
           data-node="${esc(n.id)}" data-state="${esc(state)}"
           style="left:${n.x}px;top:${n.y}px;width:${CV_NODE_W}px;height:${CV_NODE_H}px">
        <span class="cv-node-bar" style="background:${CV_LAYER_COLOR[layer] || (typeMeta ? typeMeta.color : 'var(--line)')}"></span>
        <div class="cv-node-head">
          <span class="cv-node-title">${esc(n.id)}</span>
          ${t ? `<span class="cv-node-dot ${dot}" title="${esc(state)}"></span>` : ''}
        </div>
        <div class="cv-node-meta">
          <span class="pill info" style="font-size:10px">${esc(n.type)}</span>
          ${n.model ? `<span class="mono">${esc(n.model)}</span>` : ''}
        </div>
        ${t ? `<div class="cv-node-foot">
          <span>${t.durationMs != null ? fmtDur(t.durationMs) : '—'}</span>
          <span>${t.rowsAffected != null ? fmtInt(t.rowsAffected) + ' 行' : ''}</span>
          ${t.attempt > 1 ? `<span>第 ${t.attempt} 次</span>` : ''}
        </div>` : ''}
        ${state === 'failed' ? '<span class="cv-node-badge" title="本次运行失败">!</span>' : ''}
        <div class="cv-port in" data-port="in" data-node="${esc(n.id)}" title="输入"></div>
        <div class="cv-port out ${CANVAS.connectFrom === n.id ? 'active' : ''}" data-port="out" data-node="${esc(n.id)}" title="从此连线"></div>
      </div>`;
}

// cvSyncViewport 只增删「进出视口」的节点，不重建已有元素。
// 平移时视口每移动一格就要补画一次，若每次都 innerHTML 重建 400 个节点，
// 每 90ms 就会出现一次 30–50ms 的卡顿；增量同步只创建新进场的几十个。
function cvSyncViewport() {
  const canvasEl = document.getElementById('cv-canvas');
  if (!canvasEl || canvasEl !== CANVAS.renderedEl || !CANVAS.nodeEls) { cvRender(); return; }
  const vp = cvViewport();
  if (!vp) return;
  CANVAS.cullKey = cvCullKey(vp);
  const visible = n => n.x < vp.x2 && n.x + CV_NODE_W > vp.x1 && n.y < vp.y2 && n.y + CV_NODE_H > vp.y1;
  const keep = new Set();
  const entering = [];
  CANVAS.nodes.forEach(n => {
    if (!visible(n)) return;
    keep.add(n.id);
    if (!CANVAS.nodeEls.has(n.id)) entering.push(n);
  });
  CANVAS.nodeEls.forEach((el, id) => {
    if (keep.has(id)) return;
    el.remove();
    CANVAS.nodeEls.delete(id);
  });
  if (entering.length) {
    const tpl = document.createElement('template');
    tpl.innerHTML = entering.map(cvNodeHTML).join('');
    entering.forEach(n => {
      const el = tpl.content.querySelector(`.cv-node[data-node="${CSS.escape(n.id)}"]`);
      if (el) CANVAS.nodeEls.set(n.id, el);
    });
    canvasEl.appendChild(tpl.content);
  }
  cvRedrawEdges();
  cvRenderMinimap();
}

function fmtInt(n) {
  if (n == null) return '—';
  return Number(n) >= 10000 ? (Number(n) / 10000).toFixed(1) + '万' : String(n);
}

function applyView() {
  const el = document.getElementById('cv-canvas');
  if (!el) return;
  const { tx, ty, s } = CANVAS.view;
  el.style.transform = `translate(${tx}px, ${ty}px) scale(${s})`;
  const label = document.getElementById('cv-zoom-label');
  if (label) label.textContent = Math.round(s * 100) + '%';
  cvUpdateMapViewport();
  cvMaybeRecull();
}

// 裁剪模式下平移/缩放会露出新区域，需要补画。
//
// 两个约束一起满足才不卡：滚动时最多每 90ms 重建一次（重建 500 个节点的筛选
// 与 innerHTML 大约 8–15ms，每个滚轮事件都做会把缩放拖到 20fps），
// 停下来 120ms 后再补一次精确重建（否则快速缩放后边缘会留一块空白）。
const CV_RECULL_GAP_MS = 90;
const CV_RECULL_IDLE_MS = 120;

function cvMaybeRecull() {
  if (!CANVAS.cullOn) return;
  const vp = cvViewport();
  if (!vp) return;
  if (cvCullKey(vp) === CANVAS.cullKey) return;

  const now = performance.now();
  if (!CANVAS._cullRaf && now - (CANVAS._lastRecull || 0) >= CV_RECULL_GAP_MS) {
    CANVAS._cullRaf = requestAnimationFrame(() => {
      CANVAS._cullRaf = 0;
      CANVAS._lastRecull = performance.now();
      cvSyncViewport();
    });
  }
  clearTimeout(CANVAS._cullTimer);
  CANVAS._cullTimer = setTimeout(() => {
    const v = cvViewport();
    if (!v || cvCullKey(v) === CANVAS.cullKey) return;
    CANVAS._lastRecull = performance.now();
    cvSyncViewport();
  }, CV_RECULL_IDLE_MS);
}

/* ---------------- 小地图 ---------------- */
// 只在小地图里画缩略方块 + 当前视口框：节点多时靠它判断「我在这张图的哪个位置」。
function cvScheduleMinimap() {
  if (CANVAS._mapRaf) return;
  CANVAS._mapRaf = requestAnimationFrame(() => {
    CANVAS._mapRaf = 0;
    cvRenderMinimap();
  });
}

function cvRenderMinimap() {
  const g = document.getElementById('cv-map-nodes');
  if (!g) return;
  const nodes = CANVAS.nodes;
  if (!nodes.length) { g.innerHTML = ''; CANVAS.mapGeom = null; return; }

  const W = 168, H = 112, pad = 8;
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
  nodes.forEach(n => {
    minX = Math.min(minX, n.x); minY = Math.min(minY, n.y);
    maxX = Math.max(maxX, n.x + CV_NODE_W); maxY = Math.max(maxY, n.y + CV_NODE_H);
  });
  const bw = Math.max(1, maxX - minX), bh = Math.max(1, maxY - minY);
  const k = Math.min((W - pad * 2) / bw, (H - pad * 2) / bh);
  const ox = pad + ((W - pad * 2) - bw * k) / 2 - minX * k;
  const oy = pad + ((H - pad * 2) - bh * k) / 2 - minY * k;
  CANVAS.mapGeom = { k, ox, oy };

  g.innerHTML = nodes.map(n => {
    const layer = cvLayerOf(n);
    const t = cvTaskOf(n.id);
    const fill = t && t.status === 'failed' ? 'var(--err)'
      : t && t.status === 'success' ? (CV_LAYER_COLOR[layer] || '#3fb950')
      : (CV_LAYER_COLOR[layer] || '#4a5568');
    return `<rect x="${(n.x * k + ox).toFixed(1)}" y="${(n.y * k + oy).toFixed(1)}"
      width="${Math.max(2, CV_NODE_W * k).toFixed(1)}" height="${Math.max(1.5, CV_NODE_H * k).toFixed(1)}"
      rx="1.2" fill="${fill}" opacity="0.9"></rect>`;
  }).join('');
  cvUpdateMapViewport();
}

function cvUpdateMapViewport() {
  const vp = document.getElementById('cv-map-vp');
  const geo = CANVAS.mapGeom;
  const stage = document.getElementById('cv-stage');
  if (!vp || !geo || !stage) return;
  const sr = stage.getBoundingClientRect();
  const s = CANVAS.view.s;
  const vx = -CANVAS.view.tx / s, vy = -CANVAS.view.ty / s;
  const vw = sr.width / s, vh = sr.height / s;
  // 视口可能大于整张图（缩放很小时），越界部分裁掉，否则整个小地图被一块蓝盖住
  const x0 = Math.max(1, vx * geo.k + geo.ox);
  const y0 = Math.max(1, vy * geo.k + geo.oy);
  const x1 = Math.min(167, (vx + vw) * geo.k + geo.ox);
  const y1 = Math.min(111, (vy + vh) * geo.k + geo.oy);
  vp.setAttribute('x', x0.toFixed(1));
  vp.setAttribute('y', y0.toFixed(1));
  vp.setAttribute('width', Math.max(4, x1 - x0).toFixed(1));
  vp.setAttribute('height', Math.max(4, y1 - y0).toFixed(1));
}

function cvBindMinimap() {
  const wrap = document.getElementById('cv-minimap');
  if (!wrap || wrap.dataset.bound) return;
  wrap.dataset.bound = '1';
  const jump = cvMapJump;
  wrap.addEventListener('mousedown', (ev) => {
    ev.preventDefault();
    ev.stopPropagation();
    CANVAS.mapDrag = true;
    jump(ev);
  });
  // window 上的监听只挂一次：页面每次重挂都会换掉 DOM，
  // 挂在 window 上的闭包会累积成泄漏。
  if (!CANVAS._mapDragBound) {
    CANVAS._mapDragBound = true;
    window.addEventListener('mousemove', (ev) => { if (CANVAS.mapDrag) cvMapJump(ev); });
    window.addEventListener('mouseup', () => { CANVAS.mapDrag = false; });
  }
}

function cvMapJump(ev) {
  const geo = CANVAS.mapGeom;
  const stage = document.getElementById('cv-stage');
  if (!geo || !stage) return;
  const svg = document.getElementById('cv-map-svg');
  if (!svg) return;
  const r = svg.getBoundingClientRect();
  const px = (ev.clientX - r.left) * (168 / r.width);
  const py = (ev.clientY - r.top) * (112 / r.height);
  const worldX = (px - geo.ox) / geo.k;
  const worldY = (py - geo.oy) / geo.k;
  const sr = stage.getBoundingClientRect();
  CANVAS.view.tx = sr.width / 2 - worldX * CANVAS.view.s;
  CANVAS.view.ty = sr.height / 2 - worldY * CANVAS.view.s;
  applyView();
}

function refreshMeta() {
  const el = document.getElementById('cv-meta');
  if (!el || !CANVAS.pipe) return;
  const inst = CANVAS.run && CANVAS.run.inst;
  const bits = [`节点 ${CANVAS.nodes.length}`, `v${CANVAS.pipe.activeVersion || 0}`];
  if (CANVAS.dirty) bits.push('未保存');
  if (inst) bits.push(`${inst.bizDate} · ${inst.status === 'success' ? '成功' : inst.status === 'failed' ? '失败' : inst.status}`);
  el.textContent = bits.join(' · ');
  fillInstancePicker();
}

function fillInstancePicker() {
  const sel = document.getElementById('cv-inst');
  if (!sel || !CANVAS.pipe) return;
  const list = CANVAS.instList.filter(i => i.pipelineName === CANVAS.pipe.name).slice(0, 20);
  const curId = CANVAS.run && CANVAS.run.inst ? CANVAS.run.inst.id : '';
  sel.innerHTML = `<option value="">不叠加运行状态</option>` + list.map(i => `
    <option value="${esc(i.id)}" ${i.id === curId ? 'selected' : ''}>
      ${esc(i.bizDate)} · ${esc(cvStatusLabel(i.status))} · ${esc(i.id.slice(-6))}
    </option>`).join('');
}

function cvStatusLabel(s) {
  return ({ success: '成功', failed: '失败', running: '运行中', queued: '排队', pending: '待执行', canceled: '取消' })[s] || s || '—';
}

/* ---------------- 运行态 ---------------- */

async function syncRun(instId) {
  if (!CANVAS.pipe) return;
  try {
    let target = instId;
    if (!target) {
      const list = CANVAS.instList.filter(i => i.pipelineName === CANVAS.pipe.name);
      target = list.length ? list[0].id : '';
    }
    if (!target) { CANVAS.run = null; cvRender(); refreshMeta(); return; }
    const d = await api('/instances/' + encodeURIComponent(target));
    const tasks = {};
    ((d.instance && d.instance.tasks) || []).forEach(t => tasks[t.nodeId] = t);
    CANVAS.run = { inst: d.instance, tasks };
  } catch (e) {
    CANVAS.run = null;
  }
  cvRender();
  renderInspector();
  refreshMeta();
}

/* ---------------- 视口 ---------------- */

function cvClamp(v, lo, hi) { return Math.min(hi, Math.max(lo, v)); }

function cvZoomAt(factor, cx, cy) {
  const stage = document.getElementById('cv-stage');
  if (!stage) return;
  const rect = stage.getBoundingClientRect();
  const px = cx == null ? rect.width / 2 : cx - rect.left;
  const py = cy == null ? rect.height / 2 : cy - rect.top;
  const s0 = CANVAS.view.s;
  const s1 = cvClamp(s0 * factor, 0.25, 2.5);
  CANVAS.view.tx = px - (px - CANVAS.view.tx) * (s1 / s0);
  CANVAS.view.ty = py - (py - CANVAS.view.ty) * (s1 / s0);
  CANVAS.view.s = s1;
  applyView();
}

function cvFit(animate) {
  const stage = document.getElementById('cv-stage');
  if (!stage || !CANVAS.nodes.length) return;
  const rect = stage.getBoundingClientRect();
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
  CANVAS.nodes.forEach(n => {
    minX = Math.min(minX, n.x); minY = Math.min(minY, n.y);
    maxX = Math.max(maxX, n.x + CV_NODE_W); maxY = Math.max(maxY, n.y + CV_NODE_H);
  });
  const pad = 56;
  const s = cvClamp(Math.min((rect.width - pad * 2) / (maxX - minX), (rect.height - pad * 2) / (maxY - minY), 1.1), 0.7, 1.1);
  CANVAS.view.s = s;
  CANVAS.view.tx = (rect.width - (maxX - minX) * s) / 2 - minX * s;
  CANVAS.view.ty = (rect.height - (maxY - minY) * s) / 2 - minY * s;
  const el = document.getElementById('cv-canvas');
  if (animate && el) {
    el.style.transition = 'transform .18s ease-out';
    setTimeout(() => { el.style.transition = ''; }, 200);
  }
  applyView();
}

/* ---------------- 撤销 / 重做 ---------------- */

function cvSnap() {
  return {
    nodes: CANVAS.nodes.map(n => ({ ...n, dependsOn: n.dependsOn.slice() })),
    edges: CANVAS.edges.map(e => ({ ...e })),
    sel: Array.from(CANVAS.sel),
  };
}

function cvPush() {
  cvPushSnapshot(cvSnap());
}

/**
 * 压入一个「操作前」的快照。
 * 拖动是「先改变、后提交」的动作：等到 mouseup 再 cvSnap()，拿到的已经是拖完的坐标，
 * Ctrl+Z 等于什么都不做。所以 mousedown 时先把快照存起来，抬手时才入栈。
 */
function cvPushSnapshot(snap) {
  CANVAS.undo.push(snap || cvSnap());
  if (CANVAS.undo.length > 60) CANVAS.undo.shift();
  CANVAS.redo.length = 0;
  CANVAS.dirty = true;
  refreshMeta();
}

function cvRestore(snap) {
  CANVAS.nodes = snap.nodes.map(n => ({ ...n, dependsOn: n.dependsOn.slice() }));
  CANVAS.edges = snap.edges.map(e => ({ ...e }));
  CANVAS.sel = new Set(snap.sel || []);
  CANVAS.connectFrom = null;
  cvRender();
  renderInspector();
  refreshMeta();
}

function cvUndo() {
  if (!CANVAS.undo.length) return cvSetStatus('没有可撤销的操作');
  CANVAS.redo.push(cvSnap());
  cvRestore(CANVAS.undo.pop());
  cvSetStatus('已撤销');
}

function cvRedo() {
  if (!CANVAS.redo.length) return cvSetStatus('没有可重做的操作');
  CANVAS.undo.push(cvSnap());
  cvRestore(CANVAS.redo.pop());
  cvSetStatus('已重做');
}

function cvSetStatus(msg) {
  const el = document.getElementById('cv-status');
  if (el) el.textContent = msg;
}

function saveLayout() {
  if (!CANVAS.pipe) return;
  try {
    const all = JSON.parse(localStorage.getItem('godw.canvas.layout') || '{}');
    all[CANVAS.pipe.name] = {};
    CANVAS.nodes.forEach(n => { all[CANVAS.pipe.name][n.id] = { x: n.x, y: n.y }; });
    localStorage.setItem('godw.canvas.layout', JSON.stringify(all));
  } catch (e) { /* 隐私模式下写不进去，不影响编辑 */ }
}

function readLayout(name) {
  try {
    const all = JSON.parse(localStorage.getItem('godw.canvas.layout') || '{}');
    return all[name] || {};
  } catch (e) { return {}; }
}

/* ---------------- 交互 ---------------- */

function cvWorld(ev) {
  const stage = document.getElementById('cv-stage');
  const rect = stage.getBoundingClientRect();
  return {
    x: (ev.clientX - rect.left - CANVAS.view.tx) / CANVAS.view.s,
    y: (ev.clientY - rect.top - CANVAS.view.ty) / CANVAS.view.s,
  };
}

/* 拖动时的对齐辅助线 + 吸附。
 * 取被拖节点包围盒的左/中/右与上/中/下，跟静止节点的同类线比距离；
 * 阈值按当前缩放折算成 6 个屏幕像素，贴上去后画一条贯通的细线提示「跟谁对齐了」。 */
function cvSnapDrag() {
  const moving = CANVAS.drag && CANVAS.drag.moving;
  if (!moving || !moving.length) return;
  const ids = new Set(moving.map(it => it.n.id));
  const others = CANVAS.nodes.filter(n => !ids.has(n.id));
  const mb = cvBoxOf(moving.map(it => it.n));
  if (!others.length) { cvClearGuides(); return; }

  const th = 6 / CANVAS.view.s;
  const best = { x: null, y: null };
  others.forEach(o => {
    [o.x, o.x + CV_NODE_W / 2, o.x + CV_NODE_W].forEach(line => {
      [mb.x1, (mb.x1 + mb.x2) / 2, mb.x2].forEach(mv => {
        const d = line - mv;
        if (Math.abs(d) <= th && (!best.x || Math.abs(d) < Math.abs(best.x.d))) best.x = { d, line, o };
      });
    });
    [o.y, o.y + CV_NODE_H / 2, o.y + CV_NODE_H].forEach(line => {
      [mb.y1, (mb.y1 + mb.y2) / 2, mb.y2].forEach(mv => {
        const d = line - mv;
        if (Math.abs(d) <= th && (!best.y || Math.abs(d) < Math.abs(best.y.d))) best.y = { d, line, o };
      });
    });
  });

  if (best.x) moving.forEach(it => { it.n.x += best.x.d; });
  if (best.y) moving.forEach(it => { it.n.y += best.y.d; });

  const nb = cvBoxOf(moving.map(it => it.n));
  cvDrawGuides(
    best.x ? { x: best.x.line, y1: Math.min(nb.y1, best.x.o.y) - 10, y2: Math.max(nb.y2, best.x.o.y + CV_NODE_H) + 10 } : null,
    best.y ? { y: best.y.line, x1: Math.min(nb.x1, best.y.o.x) - 10, x2: Math.max(nb.x2, best.y.o.x + CV_NODE_W) + 10 } : null,
  );
}

function cvBoxOf(nodes) {
  let x1 = Infinity, y1 = Infinity, x2 = -Infinity, y2 = -Infinity;
  nodes.forEach(n => {
    x1 = Math.min(x1, n.x); y1 = Math.min(y1, n.y);
    x2 = Math.max(x2, n.x + CV_NODE_W); y2 = Math.max(y2, n.y + CV_NODE_H);
  });
  if (!isFinite(x1)) return { x1: 0, y1: 0, x2: 0, y2: 0 };
  return { x1, y1, x2, y2 };
}

// 辅助线挂在 .cv-canvas 里，跟节点同一套世界坐标，缩放平移自动跟随
function cvDrawGuides(v, h) {
  const host = document.getElementById('cv-canvas');
  if (!host) return;
  let gv = document.getElementById('cv-guide-v');
  let gh = document.getElementById('cv-guide-h');
  if (v) {
    if (!gv) { gv = document.createElement('div'); gv.id = 'cv-guide-v'; gv.className = 'cv-guide-v'; host.appendChild(gv); }
    gv.style.display = 'block';
    gv.style.left = v.x + 'px';
    gv.style.top = v.y1 + 'px';
    gv.style.height = Math.max(1, v.y2 - v.y1) + 'px';
  } else if (gv) gv.style.display = 'none';
  if (h) {
    if (!gh) { gh = document.createElement('div'); gh.id = 'cv-guide-h'; gh.className = 'cv-guide-h'; host.appendChild(gh); }
    gh.style.display = 'block';
    gh.style.top = h.y + 'px';
    gh.style.left = h.x1 + 'px';
    gh.style.width = Math.max(1, h.x2 - h.x1) + 'px';
  } else if (gh) gh.style.display = 'none';
}

function cvClearGuides() {
  ['cv-guide-v', 'cv-guide-h'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.remove();
  });
}

/* 指针在 window 上移动：拖节点 / 框选 / 平移三种手势共用一套。
 * 挂在 window 而不是 stage：鼠标拖出画布边界时仍要跟手，否则一松手位置就断在半路。 */
function cvOnWindowMove(ev) {
  if (CANVAS.pan) {
    CANVAS.view.tx = CANVAS.pan.tx + (ev.clientX - CANVAS.pan.x);
    CANVAS.view.ty = CANVAS.pan.ty + (ev.clientY - CANVAS.pan.y);
    applyView();
    return;
  }
  if (CANVAS.drag) {
    const w = cvWorld(ev);
    CANVAS.drag.moved = true;
    CANVAS.drag.moving.forEach(it => {
      it.n.x = Math.max(0, w.x + it.ox);
      it.n.y = Math.max(0, w.y + it.oy);
    });
    // 吸附：被拖节点的左/中/右、上/中/下，与静止节点的同类线比距离，
    // 6 个屏幕像素内就贴上去并画辅助线。不做这一步，手工拖出来的图永远差几像素。
    cvSnapDrag();
    // 只改被拖元素的 left/top，不整图重排：这是拖动时不掉帧的关键
    CANVAS.drag.moving.forEach(it => {
      const el = document.querySelector(`.cv-node[data-node="${CSS.escape(it.n.id)}"]`);
      if (el) { el.style.left = it.n.x + 'px'; el.style.top = it.n.y + 'px'; }
    });
    cvRedrawEdges();
    // 小地图是 500 个小方块，每帧重建等于白送一次全量布局；
    // 按帧节流后它仍然跟着动，只是不再拖慢主画布。
    cvScheduleMinimap();
    return;
  }
  if (CANVAS.marquee) {
    const now = cvWorld(ev);
    const x1 = Math.min(CANVAS.marquee.start.x, now.x), x2 = Math.max(CANVAS.marquee.start.x, now.x);
    const y1 = Math.min(CANVAS.marquee.start.y, now.y), y2 = Math.max(CANVAS.marquee.start.y, now.y);
    CANVAS.marquee.rect = { x1, y1, x2, y2 };
    const div = document.getElementById('cv-marquee');
    if (div) {
      div.style.left = (x1 * CANVAS.view.s + CANVAS.view.tx) + 'px';
      div.style.top = (y1 * CANVAS.view.s + CANVAS.view.ty) + 'px';
      div.style.width = ((x2 - x1) * CANVAS.view.s) + 'px';
      div.style.height = ((y2 - y1) * CANVAS.view.s) + 'px';
    }
  }
}

function cvOnWindowUp() {
  if (CANVAS.pan) {
    CANVAS.pan = null;
    const stage = document.getElementById('cv-stage');
    if (stage) stage.classList.remove('panning');
  }
  if (CANVAS.drag) {
    cvClearGuides();
    if (CANVAS.drag.moved) {
      cvPushSnapshot(CANVAS.drag.snap);
      saveLayout();
      cvSetStatus('位置已调整（布局本地保存）');
    }
    CANVAS.drag = null;
  }
  if (CANVAS.marquee) {
    const m = CANVAS.marquee;
    CANVAS.marquee = null;
    const div = document.getElementById('cv-marquee');
    if (div) div.remove();
    if (m.rect) {
      const hit = CANVAS.nodes.filter(n => {
        const nx2 = n.x + CV_NODE_W, ny2 = n.y + CV_NODE_H;
        return nx2 > m.rect.x1 && n.x < m.rect.x2 && ny2 > m.rect.y1 && n.y < m.rect.y2;
      });
      if (!m.add) CANVAS.sel.clear();
      hit.forEach(n => CANVAS.sel.add(n.id));
      cvRender();
      renderInspector();
    }
  }
}

function bindStage() {
  const stage = document.getElementById('cv-stage');
  if (!stage) return;

  stage.addEventListener('wheel', (ev) => {
    if (ev.shiftKey) {
      // Shift+滚轮：横向平移（触控板用户习惯）
      ev.preventDefault();
      CANVAS.view.tx -= ev.deltaY;
      applyView();
      return;
    }
    ev.preventDefault();
    const factor = Math.exp(-ev.deltaY * 0.0016);
    cvZoomAt(factor, ev.clientX, ev.clientY);
  }, { passive: false });

  stage.addEventListener('mousedown', (ev) => {
    const port = ev.target.closest('.cv-port');
    if (port) {
      ev.preventDefault();
      cvClickPort(port);
      return;
    }

    // 空格 / 中键 / 右键：平移
    if (CANVAS.spaceDown || ev.button === 1) {
      ev.preventDefault();
      CANVAS.pan = { x: ev.clientX, y: ev.clientY, tx: CANVAS.view.tx, ty: CANVAS.view.ty };
      stage.classList.add('panning');
      return;
    }

    const nodeEl = ev.target.closest('.cv-node');
    if (nodeEl) {
      ev.preventDefault();
      const id = nodeEl.dataset.node;
      if (ev.shiftKey) {
        CANVAS.sel.has(id) ? CANVAS.sel.delete(id) : CANVAS.sel.add(id);
      } else if (!CANVAS.sel.has(id)) {
        CANVAS.sel = new Set([id]);
      }
      CANVAS.connectFrom = null;
      cvRender();
      renderInspector();

      const start = cvWorld(ev);
      const moving = CANVAS.nodes.filter(n => CANVAS.sel.has(n.id))
        .map(n => ({ n, ox: n.x - start.x, oy: n.y - start.y }));
      // 快照在改动之前取，撤销栈里才是「拖之前」的坐标
      CANVAS.drag = { moving, moved: false, snap: cvSnap() };
      return;
    }

    // 空白：框选
    if (ev.button === 0) {
      const start = cvWorld(ev);
      CANVAS.marquee = { start, add: ev.shiftKey, rect: null };
      if (!ev.shiftKey) { CANVAS.sel.clear(); cvRender(); renderInspector(); }
      const div = document.createElement('div');
      div.className = 'cv-marquee';
      div.id = 'cv-marquee';
      stage.appendChild(div);
    }
  });

  // window 上的监听只挂一次。每次进入本页都会重建 stage，但 window 不会回收旧闭包；
  // 之前每进一次画布就多挂一套，拖动时同一帧被处理 N 次（实测第 8 次进入后 500 节点从 159fps 掉到 23fps）。
  if (!CANVAS._winBound) {
    CANVAS._winBound = true;
    window.addEventListener('mousemove', cvOnWindowMove);
    window.addEventListener('mouseup', cvOnWindowUp);
  }

  stage.addEventListener('dblclick', (ev) => {
    const nodeEl = ev.target.closest('.cv-node');
    if (!nodeEl) return;
    CANVAS.sel = new Set([nodeEl.dataset.node]);
    cvRender();
    renderInspector();
    const box = document.getElementById('ins-sql');
    if (box) box.focus();
  });

}

// 拖动时每帧只改这两条 path 的 d 属性。
// 早先是「每条边一个 path」并逐个 setAttribute，500 节点 627 条边等于每帧 627 次属性写，
// 再加上 500 个节点元素的重排，帧率直接掉到个位数。
function cvRedrawEdges() {
  const svg = document.getElementById('cv-edges');
  if (!svg) return;
  const g = cvEdgeGeometry();
  const paths = svg.querySelectorAll('path.cv-edge');
  const expect = (g.normal ? 1 : 0) + (g.hot ? 1 : 0);
  // 结构对不上（可视集变了 / 高亮变了）就整层重画；对得上只改几何。
  if (paths.length !== expect || !svg.querySelector('defs')) {
    cvPaintEdges(svg, g);
    return;
  }
  let i = 0;
  if (g.normal) paths[i++].setAttribute('d', g.normal);
  if (g.hot) paths[i].setAttribute('d', g.hot);
}

function cvClickPort(port) {
  const id = port.dataset.node;
  if (port.dataset.port === 'out') {
    CANVAS.connectFrom = CANVAS.connectFrom === id ? null : id;
    cvRender();
    cvSetStatus(CANVAS.connectFrom ? `从 ${id} 连线：点击目标节点左侧圆点` : '已取消连线');
    return;
  }
  // 点入端口：完成连线
  const from = CANVAS.connectFrom;
  if (!from || from === id) { CANVAS.connectFrom = null; cvRender(); return; }
  if (cvWouldCycle(from, id)) {
    toast(`不能连线：${id} 已经在 ${from} 的上游，会成环`, 'err');
    CANVAS.connectFrom = null;
    cvRender();
    return;
  }
  const node = CANVAS.nodes.find(n => n.id === id);
  if (node && !node.dependsOn.includes(from)) {
    cvPush();
    node.dependsOn.push(from);
    CANVAS.edges.push({ from, to: id });
    cvSetStatus(`已连接 ${from} → ${id}`);
  }
  CANVAS.connectFrom = null;
  cvRender();
  renderInspector();
}

/** 判断把 from → to 连上后是否成环：to 是否已经是 from 的祖先。 */
function cvWouldCycle(from, to) {
  if (from === to) return true;
  const byId = {};
  CANVAS.nodes.forEach(n => byId[n.id] = n);
  const seen = new Set();
  const walk = (id) => {
    if (id === from) return true;
    if (seen.has(id)) return false;
    seen.add(id);
    const n = byId[id];
    return !!(n && n.dependsOn.some(walk));
  };
  return walk(to);
}

function bindPalette() {
  document.querySelectorAll('.cv-pal-item').forEach(el => {
    // 用指针拖拽而不是 HTML5 drag：dragover/drop 在部分环境（含某些浏览器缩放、
    // 触控板手势）下不触发，而这里只需要「从面板拖到画布落点」这一种手势。
    el.addEventListener('mousedown', (ev) => {
      if (ev.button !== 0) return;
      ev.preventDefault();
      const type = el.dataset.pal;
      const ghost = document.createElement('div');
      ghost.className = 'cv-pal-ghost';
      const meta = CV_TYPES.find(t => t.type === type) || CV_TYPES[1];
      ghost.innerHTML = `<span class="cv-pal-dot" style="background:${meta.color}"></span>${meta.label} · ${type}`;
      document.body.appendChild(ghost);
      const move = (e) => {
        ghost.style.left = (e.clientX + 12) + 'px';
        ghost.style.top = (e.clientY + 10) + 'px';
      };
      move(ev);
      const up = (e) => {
        document.removeEventListener('mousemove', move);
        document.removeEventListener('mouseup', up);
        ghost.remove();
        const stage = document.getElementById('cv-stage');
        if (!stage) return;
        const r = stage.getBoundingClientRect();
        if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) return;
        const w = cvWorld(e);
        cvAddNode(type, w.x - CV_NODE_W / 2, w.y - CV_NODE_H / 2);
      };
      document.addEventListener('mousemove', move);
      document.addEventListener('mouseup', up);
    });
    el.addEventListener('click', () => {
      // 点击添加时落在视口中心，比随机坐标可预期
      const stage = document.getElementById('cv-stage');
      const rect = stage.getBoundingClientRect();
      const cx = (rect.width / 2 - CANVAS.view.tx) / CANVAS.view.s - CV_NODE_W / 2;
      const cy = (rect.height / 2 - CANVAS.view.ty) / CANVAS.view.s - CV_NODE_H / 2;
      cvAddNode(el.dataset.pal, cx, cy);
    });
  });
}

function cvAddNode(type, x, y) {
  if (!CANVAS.pipe) return;
  const meta = CV_TYPES.find(t => t.type === type) || CV_TYPES[1];
  let i = 1, id = `${type}_1`;
  while (CANVAS.nodes.some(n => n.id === id)) { i++; id = `${type}_${i}`; }
  cvPush();
  CANVAS.nodes.push({
    id, type, model: '', datasource: '', sql: '', writeMode: '', dependsOn: [],
    x: Math.max(0, Math.round(x)), y: Math.max(0, Math.round(y)),
  });
  CANVAS.sel = new Set([id]);
  cvRender();
  renderInspector();
  cvSetStatus(`已添加 ${meta.label}节点 ${id}，在右侧填字段`);
  const box = document.getElementById('ins-id');
  if (box) { box.focus(); box.select(); }
}

function cvDeleteSelected() {
  if (!CANVAS.sel.size) return;
  const ids = Array.from(CANVAS.sel);
  cvPush();
  CANVAS.nodes = CANVAS.nodes.filter(n => !CANVAS.sel.has(n.id));
  CANVAS.nodes.forEach(n => { n.dependsOn = n.dependsOn.filter(d => !CANVAS.sel.has(d)); });
  CANVAS.edges = CANVAS.edges.filter(e => !CANVAS.sel.has(e.from) && !CANVAS.sel.has(e.to));
  CANVAS.sel.clear();
  cvRender();
  renderInspector();
  cvSetStatus(`已删除 ${ids.join(', ')}`);
}

/**
 * 复制 / 粘贴选中节点。
 * 组内依赖要一起重映射（否则粘贴出来的两个节点仍指向原节点，一看就知道是「假的」）；
 * 指向组外的依赖保持不变——那才是真正的跨链路依赖。
 */
function cvCopy() {
  if (!CANVAS.sel.size) return;
  CANVAS.clip = CANVAS.nodes.filter(n => CANVAS.sel.has(n.id))
    .map(n => ({ ...n, dependsOn: n.dependsOn.slice() }));
  cvSetStatus(`已复制 ${CANVAS.clip.length} 个节点（Ctrl+V 粘贴）`);
}

function cvPaste() {
  if (!CANVAS.clip || !CANVAS.clip.length) return;
  cvPush();
  // 先给每个副本定好新 id（同一批里互相引用要靠这张表），再统一改 dependsOn
  const map = {};
  const made = CANVAS.clip.map(src => {
    let base = src.id + '_copy';
    let id = base, i = 1;
    while (CANVAS.nodes.some(n => n.id === id)) { i++; id = base + i; }
    map[src.id] = id;
    return { ...src, id, dependsOn: [], x: src.x + 40, y: src.y + 40 };
  });
  made.forEach((n, k) => {
    n.dependsOn = (CANVAS.clip[k].dependsOn || []).map(d => map[d] || d);
  });
  made.forEach(n => {
    CANVAS.nodes.push(n);
    n.dependsOn.forEach(d => CANVAS.edges.push({ from: d, to: n.id }));
  });
  CANVAS.sel = new Set(made.map(n => n.id));
  CANVAS.clip = made.map(n => ({ ...n, dependsOn: n.dependsOn.slice() }));
  cvRender();
  renderInspector();
  cvSetStatus(`已粘贴 ${made.length} 个节点`);
}

function bindKeys() {
  if (CANVAS._keysBound) return;
  CANVAS._keysBound = true;

  document.addEventListener('keydown', (ev) => {
    if (S.route !== 'canvas') return;
    const tag = (ev.target.tagName || '').toLowerCase();
    const typing = tag === 'input' || tag === 'textarea' || tag === 'select' || ev.target.isContentEditable;
    const modalOpen = !!document.querySelector('#modal-root .modal');

    if (ev.code === 'Space' && !typing && !modalOpen) {
      CANVAS.spaceDown = true;
      ev.preventDefault();
      document.getElementById('cv-stage')?.classList.add('panning');
      return;
    }
    if (typing || modalOpen) return;

    const mod = ev.ctrlKey || ev.metaKey;
    if (mod && ev.key.toLowerCase() === 'z') { ev.preventDefault(); ev.shiftKey ? cvRedo() : cvUndo(); return; }
    if (mod && ev.key.toLowerCase() === 'y') { ev.preventDefault(); cvRedo(); return; }
    if (mod && ev.key.toLowerCase() === 's') { ev.preventDefault(); cvSave(); return; }
    if (mod && ev.key.toLowerCase() === 'a') { ev.preventDefault(); CANVAS.sel = new Set(CANVAS.nodes.map(n => n.id)); cvRender(); renderInspector(); return; }
    if (mod && ev.key.toLowerCase() === 'c') { ev.preventDefault(); cvCopy(); return; }
    if (mod && ev.key.toLowerCase() === 'v') { ev.preventDefault(); cvPaste(); return; }
    if (ev.key === 'Delete' || ev.key === 'Backspace') { ev.preventDefault(); cvDeleteSelected(); return; }
    if (ev.key === 'Escape') {
      CANVAS.connectFrom = null; CANVAS.sel.clear(); cvRender(); renderInspector();
      return;
    }
    if (ev.key === '0') { ev.preventDefault(); cvFit(true); return; }
    if (ev.key === '+' || ev.key === '=') { ev.preventDefault(); cvZoomAt(1.2); return; }
    if (ev.key === '-' || ev.key === '_') { ev.preventDefault(); cvZoomAt(1 / 1.2); return; }
    if (['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(ev.key) && CANVAS.sel.size) {
      ev.preventDefault();
      const d = ev.shiftKey ? 24 : 4;
      const dx = ev.key === 'ArrowLeft' ? -d : ev.key === 'ArrowRight' ? d : 0;
      const dy = ev.key === 'ArrowUp' ? -d : ev.key === 'ArrowDown' ? d : 0;
      // 连续按方向键算一步：第一次按下时压快照，停手 500ms 再落盘。
      // 否则连按 20 下会吃掉 20 步撤销额度，而布局也不会保存（刷新即丢）。
      if (!CANVAS.nudgePushed) { cvPush(); CANVAS.nudgePushed = true; }
      clearTimeout(CANVAS.nudgeTimer);
      CANVAS.nudgeTimer = setTimeout(() => {
        CANVAS.nudgePushed = false;
        saveLayout();
        refreshMeta();
      }, 500);
      CANVAS.nodes.forEach(n => { if (CANVAS.sel.has(n.id)) { n.x += dx; n.y += dy; } });
      cvRender();
    }
  });

  document.addEventListener('keyup', (ev) => {
    if (ev.code === 'Space') {
      CANVAS.spaceDown = false;
      if (!CANVAS.pan) document.getElementById('cv-stage')?.classList.remove('panning');
    }
  });
}

/* ---------------- 属性面板 ---------------- */

function renderInspector() {
  const host = document.getElementById('cv-inspector');
  if (!host) return;

  if (!CANVAS.pipe) {
    host.innerHTML = `<div class="cv-ins-empty">选择一条管道后开始编辑。</div>`;
    return;
  }
  if (!CANVAS.sel.size) {
    host.innerHTML = `
      <div class="cv-ins-title">属性</div>
      <div class="cv-ins-empty">
        选中节点后可编辑类型、目标模型、写入模式与 SQL。<br /><br />
        从左侧拖入节点，或点击节点右侧圆点、再点目标节点左侧圆点建立依赖。
      </div>
      <div class="cv-ins-run">
        <div class="row"><span>管道</span><b class="mono">${esc(CANVAS.pipe.name)}</b></div>
        <div class="row"><span>cron</span><b class="mono">${esc(CANVAS.pipe.schedule || '手动')}</b></div>
        <div class="row"><span>节点 / 依赖</span><b>${CANVAS.nodes.length} / ${CANVAS.edges.length}</b></div>
        <div class="row"><span>版本</span><b>v${CANVAS.pipe.activeVersion || 0}</b></div>
      </div>`;
    return;
  }

  if (CANVAS.sel.size > 1) {
    const ids = Array.from(CANVAS.sel);
    host.innerHTML = `
      <div class="cv-ins-title">已选 ${ids.length} 个节点</div>
      <div class="cv-ins-empty mono" style="font-size:11px;word-break:break-all">${esc(ids.join('、'))}</div>
      <div class="cv-ins-field">
        <label>对齐</label>
        <div class="row">
          <button class="sm ghost" data-act="cv-align-left">左对齐</button>
          <button class="sm ghost" data-act="cv-align-top">顶对齐</button>
          <button class="sm ghost" data-act="cv-distribute">等距</button>
        </div>
      </div>
      <div class="cv-ins-field">
        <label>批量设置数据源</label>
        <input id="ins-multi-ds" placeholder="pg_dw" class="mono" />
      </div>
      <button class="sm ghost" data-act="cv-apply-ds">应用到所选</button>
      <button class="danger sm" style="margin-top:8px" data-act="cv-del-selected">删除所选</button>`;
    return;
  }

  const node = CANVAS.nodes.find(n => CANVAS.sel.has(n.id));
  if (!node) { host.innerHTML = ''; return; }
  const t = cvTaskOf(node.id);
  const layer = cvLayerOf(node);

  host.innerHTML = `
    <div class="cv-ins-title" style="justify-content:space-between">
      <span>节点属性 ${layer ? `<span class="pill mute" style="font-size:10px">${esc(layer.toUpperCase())}</span>` : ''}</span>
      <button class="sm ghost" data-act="cv-del-selected" title="删除（Del）">删除</button>
    </div>
    <div class="cv-ins-field"><label>节点 ID</label><input id="ins-id" class="mono" value="${esc(node.id)}" /></div>
    <div class="cv-ins-field"><label>类型</label><select id="ins-type">
      ${CV_TYPES.map(x => `<option value="${x.type}" ${x.type === node.type ? 'selected' : ''}>${x.label} · ${x.type}</option>`).join('')}
    </select></div>
    <div class="cv-ins-field"><label>目标模型</label><input id="ins-model" class="mono" value="${esc(node.model)}" placeholder="dwd_order" /></div>
    <div class="cv-ins-field"><label>数据源</label><input id="ins-ds" class="mono" value="${esc(node.datasource)}" placeholder="pg_dw" /></div>
    <div class="cv-ins-field"><label>写入模式</label><select id="ins-wm">
      ${['', 'append', 'overwrite', 'merge', 'scd2', 'full_refresh', 'delete_insert', 'overwrite_partition']
        .map(m => `<option value="${m}" ${m === (node.writeMode || '') ? 'selected' : ''}>${m || '（按模型默认）'}</option>`).join('')}
    </select></div>
    <div class="cv-ins-field"><label>依赖（上游节点，逗号分隔）</label>
      <input id="ins-deps" class="mono" value="${esc(node.dependsOn.join(', '))}" placeholder="src_order" /></div>
    <div class="cv-ins-field"><label>SQL</label>
      <textarea id="ins-sql" rows="9" class="mono" placeholder="SELECT ...">${esc(node.sql)}</textarea></div>

    ${t ? `<div class="cv-ins-run">
      <div class="row"><span>本次运行</span><b>${statusPill(t.status)}</b></div>
      <div class="row"><span>耗时</span><b class="mono">${t.durationMs != null ? fmtDur(t.durationMs) : '—'}</b></div>
      <div class="row"><span>影响行数</span><b class="mono">${t.rowsAffected != null ? t.rowsAffected : '—'}</b></div>
      <div class="row"><span>尝试次数</span><b>第 ${t.attempt || 1} 次</b></div>
      <div class="row" style="gap:6px;margin-top:6px">
        <button class="sm ghost" style="flex:1" data-act="cv-open-run" data-id="${esc(CANVAS.run.inst.id)}">查看日志</button>
        ${['failed', 'blocked'].includes(t.status)
          ? `<button class="sm ghost" style="flex:1" data-act="cv-retry-node" data-id="${esc(CANVAS.run.inst.id)}" data-node="${esc(node.id)}">重跑此节点</button>`
          : ''}
      </div>
    </div>` : `<div class="cv-ins-run">该节点在当前选中实例里没有运行记录。</div>`}`;

  cvBindInspector(node);
}

function cvBindInspector(node) {
  const commit = () => { cvPush(); refreshMeta(); };

  const idEl = document.getElementById('ins-id');
  if (idEl) idEl.addEventListener('change', () => {
    const v = idEl.value.trim();
    if (!v) { idEl.value = node.id; return toast('节点 ID 不能为空', 'err'); }
    if (v !== node.id && CANVAS.nodes.some(n => n.id === v)) { idEl.value = node.id; return toast('节点 ID 已存在', 'err'); }
    commit();
    const old = node.id;
    node.id = v;
    CANVAS.nodes.forEach(n => { n.dependsOn = n.dependsOn.map(d => d === old ? v : d); });
    CANVAS.edges = CANVAS.edges.map(e => ({ from: e.from === old ? v : e.from, to: e.to === old ? v : e.to }));
    CANVAS.sel = new Set([v]);
    cvRender();
    renderInspector();
  });

  const bindSimple = (id, apply) => {
    const el = document.getElementById(id);
    if (!el) return;
    el.addEventListener('change', () => { commit(); apply(el.value); cvRender(); refreshMeta(); });
  };
  bindSimple('ins-type', v => { node.type = v; });
  bindSimple('ins-model', v => { node.model = v.trim(); });
  bindSimple('ins-ds', v => { node.datasource = v.trim(); });
  bindSimple('ins-wm', v => { node.writeMode = v; });

  const sqlEl = document.getElementById('ins-sql');
  if (sqlEl) {
    // 第一次敲键时压一次快照：之后连续输入都算同一步编辑，撤销能整段回退。
    let pushed = false, timer = null;
    sqlEl.addEventListener('input', () => {
      if (!pushed) { cvPush(); pushed = true; }
      clearTimeout(timer);
      timer = setTimeout(() => { node.sql = sqlEl.value; refreshMeta(); }, 400);
    });
    sqlEl.addEventListener('blur', () => { node.sql = sqlEl.value; pushed = false; });
  }

  const depsEl = document.getElementById('ins-deps');
  if (depsEl) depsEl.addEventListener('change', () => {
    const deps = depsEl.value.split(',').map(s => s.trim()).filter(Boolean)
      .filter(d => d !== node.id && CANVAS.nodes.some(n => n.id === d));
    const bad = cvWouldCycleWith(node.id, deps);
    if (bad) { depsEl.value = node.dependsOn.join(', '); return toast(`依赖会成环：${bad}`, 'err'); }
    commit();
    node.dependsOn = deps;
    CANVAS.edges = [];
    CANVAS.nodes.forEach(n => n.dependsOn.forEach(d => CANVAS.edges.push({ from: d, to: n.id })));
    cvRender();
  });
}

function cvWouldCycleWith(id, deps) {
  const byId = {};
  CANVAS.nodes.forEach(n => byId[n.id] = n);
  const bad = deps.find(d => {
    // 若 id 是 d 的祖先，d 反过来依赖 id 就成环
    const seen = new Set();
    const walk = (x) => {
      if (x === id) return true;
      if (seen.has(x)) return false;
      seen.add(x);
      const n = byId[x];
      return !!(n && n.dependsOn.some(walk));
    };
    return walk(d);
  });
  return bad || '';
}

/* ---------------- 布局 ---------------- */

function cvAutoLayout() {
  if (!CANVAS.nodes.length) return;
  cvPush();
  const byId = {};
  CANVAS.nodes.forEach(n => byId[n.id] = n);
  const level = {};
  const visit = (id, d) => {
    if (level[id] !== undefined && level[id] >= d) return;
    level[id] = d;
    if (d > 12) return;
    (byId[id] ? byId[id].dependsOn : []).forEach(u => visit(u, d + 1));
  };
  CANVAS.nodes.forEach(n => visit(n.id, 0));
  const groups = {};
  CANVAS.nodes.forEach(n => {
    const l = level[n.id] || 0;
    (groups[l] = groups[l] || []).push(n);
  });
  Object.keys(groups).forEach(k => {
    // 同层内按上游平均位置排序，减少交叉线
    groups[k].sort((a, b) => {
      const ay = cvAvgUpY(a, byId), by = cvAvgUpY(b, byId);
      return ay - by;
    });
    groups[k].forEach((n, i) => {
      n.x = 56 + Number(k) * 192;
      n.y = 48 + i * 112;
    });
  });
  saveLayout();
  cvRender();
  cvFit(true);
  cvSetStatus('已自动布局');
}

function cvAvgUpY(n, byId) {
  if (!n.dependsOn.length) return n.y;
  const ys = n.dependsOn.map(d => byId[d] ? byId[d].y : 0).filter(y => y);
  return ys.length ? ys.reduce((a, b) => a + b, 0) / ys.length : n.y;
}

function cvAlign(mode) {
  const sel = CANVAS.nodes.filter(n => CANVAS.sel.has(n.id));
  if (sel.length < 2) return;
  cvPush();
  if (mode === 'left') {
    const x = Math.min(...sel.map(n => n.x));
    sel.forEach(n => n.x = x);
  } else if (mode === 'top') {
    const y = Math.min(...sel.map(n => n.y));
    sel.forEach(n => n.y = y);
  } else {
    const sorted = sel.slice().sort((a, b) => a.y - b.y);
    const y0 = sorted[0].y, step = 112;
    sorted.forEach((n, i) => n.y = y0 + i * step);
  }
  saveLayout();
  cvRender();
}

/* ---------------- 校验 / 保存 ---------------- */

async function cvDryRun() {
  if (!CANVAS.pipe) return;
  let res;
  try {
    res = await api('/pipelines/dry-run', { method: 'POST', body: { yaml: yamlOf(specFromCanvas()) } });
  } catch (e) {
    modal('校验未通过', `<div class="err-box">${esc(e.message)}</div>`);
    return;
  }
  const nodes = res.nodes || [];
  const bad = nodes.filter(n => n.error);
  modal('校验结果 · ' + res.pipeline, `
    <div class="row" style="margin-bottom:12px;gap:8px;flex-wrap:wrap">
      <span class="pill ${bad.length ? 'err' : 'ok'}">${bad.length ? bad.length + ' 个节点有问题' : '全部通过'}</span>
      <span class="pill mute">业务日期 ${esc(res.bizDate || '')}</span>
      <span class="pill mute">${(res.stages || []).length} 层</span>
      ${(res.wouldWrite || []).length ? `<span class="pill warn">将写 ${res.wouldWrite.length} 张表</span>` : ''}
    </div>
    ${(res.warnings || []).map(w => `<div class="hint" style="margin-bottom:6px">⚠ ${esc(w)}</div>`).join('')}
    <div class="table-wrap"><table>
      <thead><tr><th>层</th><th>节点</th><th>类型</th><th>目标</th><th>写入</th><th>说明</th></tr></thead>
      <tbody>${nodes.map((n, i) => `
        <tr>
          <td class="dim mono">${(res.stages || []).findIndex(st => st.includes(n.id)) + 1 || '—'}</td>
          <td class="mono">${esc(n.id)}</td>
          <td><span class="pill mute">${esc(n.type)}</span></td>
          <td class="mono">${esc(n.model || '—')}</td>
          <td class="mono">${esc(n.writeMode || '—')}</td>
          <td>${n.error ? `<span style="color:var(--err)">${esc(n.error)}</span>` : (n.managed ? '<span class="dim">平台托管</span>' : '<span class="dim">—</span>')}</td>
        </tr>`).join('')}</tbody>
    </table></div>
    ${nodes.some(n => n.sql) ? `<details style="margin-top:12px"><summary class="hint" style="cursor:pointer">查看渲染后的 SQL</summary>
      <div class="stack" style="margin-top:8px">
        ${nodes.filter(n => n.sql).map(n => `<div>
          <div class="hint mono" style="margin-bottom:4px">${esc(n.id)}</div>
          <pre class="mono" style="background:var(--bg);border:1px solid var(--line);border-radius:7px;padding:10px;font-size:11.5px;overflow:auto;max-height:180px">${esc(n.sql)}</pre>
        </div>`).join('')}
      </div></details>` : ''}`);
  void cvSetStatus(`${nodes.length} 个节点已渲染`);
}

async function cvSave() {
  if (!CANVAS.pipe) return;
  const btn = document.querySelector('[data-act="cv-save"]');
  if (btn) btn.disabled = true;
  const spec = specFromCanvas();
  const yaml = yamlOf(spec);
  try {
    await api('/pipelines', { method: 'POST', body: { yaml, publish: true } });
    CANVAS.dirty = false;
    toast('已保存并发布', 'ok');
    S.selected.canvasPipe = CANVAS.pipe.name;
    await render();
  } catch (err) {
    modal('校验失败', `<div class="err-box">${esc(err.message)}</div>
      <div class="hint" style="margin-top:10px">以下是生成的 YAML，可直接修改后保存：</div>
      <textarea rows="16" style="margin-top:8px">${esc(yaml)}</textarea>`);
  } finally {
    if (btn) btn.disabled = false;
  }
}

/* ---------------- 动作 ---------------- */

document.addEventListener('click', async (ev) => {
  const el = ev.target.closest('[data-act]');
  if (!el) return;
  const act = el.dataset.act;
  if (!act.startsWith('cv-')) return;

  try {
    switch (act) {
      case 'cv-undo': cvUndo(); break;
      case 'cv-redo': cvRedo(); break;
      case 'cv-dryrun': await cvDryRun(); break;
      case 'cv-save': await cvSave(); break;
      case 'cv-autolayout': cvAutoLayout(); break;
      case 'cv-zoom-in': cvZoomAt(1.2); break;
      case 'cv-zoom-out': cvZoomAt(1 / 1.2); break;
      case 'cv-fit': cvFit(true); break;
      case 'cv-del-selected': cvDeleteSelected(); break;
      case 'cv-align-left': cvAlign('left'); break;
      case 'cv-align-top': cvAlign('top'); break;
      case 'cv-distribute': cvAlign('dist'); break;
      case 'cv-apply-ds': {
        const v = val('ins-multi-ds');
        if (!v) return toast('先填数据源', 'err');
        cvPush();
        CANVAS.nodes.forEach(n => { if (CANVAS.sel.has(n.id)) n.datasource = v; });
        cvRender(); renderInspector();
        cvSetStatus(`已把 ${CANVAS.sel.size} 个节点的数据源设为 ${v}`);
        break;
      }
      case 'cv-open-run': {
        if (typeof openRunDrawer === 'function') await openRunDrawer(el.dataset.id);
        break;
      }
      case 'cv-retry-node': {
        const nodeID = el.dataset.node;
        el.disabled = true;
        try {
          await api('/instances/' + encodeURIComponent(el.dataset.id) + '/retry', {
            method: 'POST', body: { node: nodeID },
          });
          toast(`已从 ${nodeID} 重跑（含下游）`, 'ok');
          await syncRun(el.dataset.id);
        } catch (err) {
          toast(err.message, 'err');
        } finally {
          el.disabled = false;
        }
        break;
      }
    }
  } catch (err) {
    toast(err.message, 'err');
  }
});

// 管道切换 / 实例切换
document.addEventListener('change', (ev) => {
  if (ev.target.id === 'cv-pipe') {
    S.selected.canvasPipe = ev.target.value;
    render();
  }
  if (ev.target.id === 'cv-inst') {
    syncRun(ev.target.value);
  }
});
