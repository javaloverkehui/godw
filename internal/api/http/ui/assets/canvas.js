/* GoDW 控制台 · 管道可视化编排：拖拽节点、连线、编辑属性，保存即写回 YAML 规范。 */
'use strict';

const CANVAS = {
  nodes: [],          // {id, type, model, datasource, sql, writeMode, dependsOn, x, y}
  edges: [],
  pipe: null,
  mode: 'select',     // select | connect
  connectFrom: null,
  dirty: false,
  layoutKey: 'godw.canvas.layout',
};

Object.assign(ROUTES, {
  canvas: { title: '编排', sub: '可视化编辑管道 DAG（YAML 仍是唯一事实源）', render: pageCanvas },
});

const NODE_W = 176, NODE_H = 56;

async function pageCanvas(view) {
  const pipes = await api('/pipelines');
  const cur = S.selected.canvasPipe || (pipes[0] && pipes[0].name) || '';
  const p = pipes.find(x => x.name === cur);

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">编排</h2>
        <p class="page-desc">拖拽移动、拖端口连线、双击编辑节点。保存后生成 YAML 并校验，与手写 YAML 完全等价。</p>
      </div>
      <div class="row">
        <select id="cv-pipe" style="width:220px">
          ${pipes.map(x => `<option value="${esc(x.name)}" ${x.name === cur ? 'selected' : ''}>${esc(x.name)}</option>`).join('') || '<option value="">暂无管道</option>'}
        </select>
        <button class="ghost" data-act="cv-add-node" ${p ? '' : 'disabled'}>加节点</button>
        <button class="ghost" data-act="cv-autolayout" ${p ? '' : 'disabled'}>自动布局</button>
        <button class="primary" data-act="cv-save" ${p ? '' : 'disabled'}>保存并发布</button>
      </div>
    </div>
    <div class="card" style="padding:0;overflow:hidden">
      <div class="cv-toolbar">
        <span class="hint">${p ? `节点 ${(p.spec.nodes || []).length} 个 · v${p.activeVersion || 0}` : '选择管道开始编辑'}</span>
        <span class="spacer"></span>
        <span class="hint" id="cv-status"></span>
      </div>
      <div id="cv-stage" class="cv-stage">
        ${p ? '' : '<div class="empty" style="margin:auto"><b>还没有管道</b>先在「管道」页新建一条</div>'}
      </div>
    </div>`;

  if (!p) return;
  CANVAS.pipe = p;
  loadCanvasFromSpec(p);
  bindCanvasStage();
  drawCanvas();
  const sel = document.getElementById('cv-pipe');
  if (sel) sel.onchange = () => { S.selected.canvasPipe = sel.value; render(); };
}

function loadCanvasFromSpec(p) {
  const spec = p.spec || {};
  const layout = readLayout(p.name);
  const raw = spec.nodes || [];

  // 没有保存过布局时，按真实依赖算层级，避免所有节点堆在左列。
  const lvl = {};
  const byId = {};
  raw.forEach(n => byId[n.id] = n);
  const calc = (id, seen) => {
    if (lvl[id] !== undefined) return lvl[id];
    if (seen.has(id)) return 0;
    seen.add(id);
    const deps = (byId[id] && byId[id].dependsOn) || [];
    const v = deps.length ? 1 + Math.max(...deps.map(d => calc(d, seen))) : 0;
    lvl[id] = v;
    return v;
  };
  raw.forEach(n => calc(n.id, new Set()));

  const rowOf = {};
  const rowCount = {};
  CANVAS.nodes = raw.map((n, i) => {
    const l = lvl[n.id] || 0;
    rowOf[l] = (rowOf[l] || 0);
    const idx = rowCount[l] = (rowCount[l] || 0);
    rowCount[l] = idx + 1;
    const pos = layout[n.id] || { x: 40 + l * 236, y: 36 + idx * 104 };
    return {
      id: n.id, type: n.type, model: n.model || '', datasource: n.datasource || '',
      sql: n.sql || '', writeMode: n.writeMode || '', dependsOn: n.dependsOn || [],
      x: pos.x, y: pos.y, raw: n,
    };
  });
  CANVAS.edges = [];
  CANVAS.nodes.forEach(n => (n.dependsOn || []).forEach(d => CANVAS.edges.push({ from: d, to: n.id })));
  CANVAS.dirty = false;
}

function readLayout(name) {
  try {
    const all = JSON.parse(localStorage.getItem(CANVAS.layoutKey) || '{}');
    return all[name] || {};
  } catch (e) { return {}; }
}

function saveLayout() {
  try {
    const all = JSON.parse(localStorage.getItem(CANVAS.layoutKey) || '{}');
    all[CANVAS.pipe.name] = {};
    CANVAS.nodes.forEach(n => { all[CANVAS.pipe.name][n.id] = { x: n.x, y: n.y }; });
    localStorage.setItem(CANVAS.layoutKey, JSON.stringify(all));
  } catch (e) { /* 忽略隐私模式下的写入失败 */ }
}

function layoutByLevel() {
  const byId = {}; CANVAS.nodes.forEach(n => byId[n.id] = n);
  const level = {};
  const visit = (id, d) => {
    if (level[id] !== undefined && level[id] >= d) return;
    level[id] = d;
    if (d > 12) return;
    (byId[id] ? byId[id].dependsOn : []).forEach(u => visit(u, d + 1));
  };
  CANVAS.nodes.forEach(n => visit(n.id, 0));
  const groups = {};
  CANVAS.nodes.forEach(n => (groups[level[n.id] || 0] = groups[level[n.id] || 0] || []).push(n));
  Object.keys(groups).forEach(k => {
    groups[k].forEach((n, i) => {
      n.x = 40 + Number(k) * 230;
      n.y = 30 + i * 100;
    });
  });
  saveLayout();
  drawCanvas();
}

function drawCanvas() {
  const stage = document.getElementById('cv-stage');
  if (!stage) return;
  const maxX = Math.max(900, ...CANVAS.nodes.map(n => n.x + NODE_W + 60));
  const maxY = Math.max(420, ...CANVAS.nodes.map(n => n.y + NODE_H + 60));

  const svgEdges = CANVAS.edges.map(e => {
    const a = CANVAS.nodes.find(n => n.id === e.from);
    const b = CANVAS.nodes.find(n => n.id === e.to);
    if (!a || !b) return '';
    const x1 = a.x + NODE_W, y1 = a.y + NODE_H / 2;
    const x2 = b.x, y2 = b.y + NODE_H / 2;
    const mx = (x1 + x2) / 2;
    return `<path d="M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}"
      stroke="#2f5db3" stroke-width="1.6" fill="none" marker-end="url(#arrow)"/>`;
  }).join('');

  stage.style.width = maxX + 'px';
  stage.style.height = maxY + 'px';
  stage.innerHTML = `
    <svg class="cv-edges" width="${maxX}" height="${maxY}">
      <defs><marker id="arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
        <path d="M0,0 L8,4 L0,8 z" fill="#2f5db3"/></marker></defs>
      ${svgEdges}
    </svg>
    ${CANVAS.nodes.map(n => `
      <div class="cv-node ${CANVAS.connectFrom === n.id ? 'connecting' : ''}" data-node="${esc(n.id)}"
           style="left:${n.x}px;top:${n.y}px;width:${NODE_W}px;height:${NODE_H}px">
        <div class="cv-node-title">${esc(n.id)}</div>
        <div class="cv-node-meta"><span class="pill info">${esc(n.type)}</span>${n.model ? `<span class="mono">${esc(n.model)}</span>` : ''}</div>
        <div class="cv-port in" data-port="in" data-node="${esc(n.id)}"></div>
        <div class="cv-port out" data-port="out" data-node="${esc(n.id)}"></div>
      </div>`).join('')}`;
}

function bindCanvasStage() {
  const stage = document.getElementById('cv-stage');
  if (!stage) return;
  let drag = null;

  stage.addEventListener('mousedown', (ev) => {
    const port = ev.target.closest('.cv-port');
    if (port) {
      ev.preventDefault();
      const id = port.dataset.node;
      if (port.dataset.port === 'out') {
        CANVAS.connectFrom = CANVAS.connectFrom === id ? null : id;
        setStatus(CANVAS.connectFrom ? `从 ${id} 连线：点击目标节点的左侧圆点` : '已取消连线');
        drawCanvas();
      } else if (CANVAS.connectFrom && CANVAS.connectFrom !== id) {
        const from = CANVAS.connectFrom;
        const node = CANVAS.nodes.find(n => n.id === id);
        if (node && !node.dependsOn.includes(from)) {
          node.dependsOn.push(from);
          CANVAS.edges.push({ from, to: id });
          CANVAS.dirty = true;
          setStatus(`已连接 ${from} → ${id}`);
        }
        CANVAS.connectFrom = null;
        drawCanvas();
      }
      return;
    }
    const el = ev.target.closest('.cv-node');
    if (!el) return;
    const node = CANVAS.nodes.find(n => n.id === el.dataset.node);
    if (!node) return;
    drag = { node, dx: ev.clientX - node.x, dy: ev.clientY - node.y, moved: false };
    ev.preventDefault();
  });

  window.addEventListener('mousemove', (ev) => {
    if (!drag) return;
    drag.moved = true;
    drag.node.x = Math.max(10, ev.clientX - drag.dx);
    drag.node.y = Math.max(10, ev.clientY - drag.dy);
    const el = document.querySelector(`.cv-node[data-node="${drag.node.id}"]`);
    if (el) { el.style.left = drag.node.x + 'px'; el.style.top = drag.node.y + 'px'; }
  });

  window.addEventListener('mouseup', () => {
    if (drag && drag.moved) { CANVAS.dirty = true; saveLayout(); drawCanvas(); }
    drag = null;
  });

  stage.addEventListener('dblclick', (ev) => {
    const el = ev.target.closest('.cv-node');
    if (!el) return;
    const node = CANVAS.nodes.find(n => n.id === el.dataset.node);
    if (node) nodeEditor(node);
  });
}

function setStatus(msg) {
  const el = document.getElementById('cv-status');
  if (el) el.textContent = msg;
}

function nodeEditor(node) {
  const types = ['source', 'sql', 'quality', 'sink', 'shell', 'http', 'branch'];
  modal('节点 ' + node.id, `
    <div class="stack">
      <div class="grid c2" style="gap:10px">
        <div class="field"><label>节点 ID</label><input id="nd-id" value="${esc(node.id)}" class="mono" /></div>
        <div class="field"><label>类型</label><select id="nd-type">
          ${types.map(t => `<option value="${t}" ${t === node.type ? 'selected' : ''}>${t}</option>`).join('')}
        </select></div>
      </div>
      <div class="grid c2" style="gap:10px">
        <div class="field"><label>目标模型</label><input id="nd-model" value="${esc(node.model)}" class="mono" placeholder="dwd_order" /></div>
        <div class="field"><label>数据源</label><input id="nd-ds" value="${esc(node.datasource)}" class="mono" placeholder="pg_dw" /></div>
      </div>
      <div class="field"><label>写入模式</label><select id="nd-wm">
        ${['', 'append', 'overwrite', 'upsert', 'overwrite_partition'].map(m => `<option value="${m}" ${m === (node.writeMode || '') ? 'selected' : ''}>${m || '（默认）'}</option>`).join('')}
      </select></div>
      <div class="field"><label>SQL</label><textarea id="nd-sql" rows="8" class="mono">${esc(node.sql)}</textarea></div>
      <div class="field"><label>依赖（逗号分隔）</label><input id="nd-deps" value="${esc((node.dependsOn || []).join(', '))}" class="mono" /></div>
      <div class="row" style="justify-content:space-between">
        <button class="danger" data-act="cv-del-node" data-id="${esc(node.id)}">删除节点</button>
        <div class="row">
          <button class="ghost" data-act="close-modal">取消</button>
          <button class="primary" data-act="cv-save-node" data-id="${esc(node.id)}">应用</button>
        </div>
      </div>
    </div>`);
}

function addNodeModal() {
  modal('新增节点', `
    <div class="stack">
      <div class="grid c2" style="gap:10px">
        <div class="field"><label>节点 ID</label><input id="nd-id" placeholder="dwd_order" class="mono" /></div>
        <div class="field"><label>类型</label><select id="nd-type">
          <option value="sql">sql</option><option value="source">source</option>
          <option value="quality">quality</option><option value="sink">sink</option>
          <option value="shell">shell</option><option value="http">http</option>
        </select></div>
      </div>
      <div class="field"><label>目标模型</label><input id="nd-model" placeholder="dwd_order" class="mono" /></div>
      <div class="field"><label>数据源</label><input id="nd-ds" placeholder="pg_dw" class="mono" /></div>
      <div class="field"><label>依赖（逗号分隔）</label><input id="nd-deps" placeholder="src_order" class="mono" /></div>
      <div class="field"><label>SQL</label><textarea id="nd-sql" rows="6" class="mono" placeholder="SELECT ..."></textarea></div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="cv-create-node">创建</button>
      </div>
    </div>`);
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

/* ---------------- 动作 ---------------- */

document.addEventListener('click', async (ev) => {
  const el = ev.target.closest('[data-act]');
  if (!el) return;
  const act = el.dataset.act;
  const acts = ['cv-add-node', 'cv-create-node', 'cv-save-node', 'cv-del-node', 'cv-save', 'cv-autolayout'];
  if (!acts.includes(act)) return;

  try {
    switch (act) {
      case 'cv-add-node': addNodeModal(); break;
      case 'cv-create-node': {
        const id = val('nd-id');
        if (!id) { toast('节点 ID 必填', 'err'); return; }
        if (CANVAS.nodes.some(n => n.id === id)) { toast('节点 ID 已存在', 'err'); return; }
        const deps = val('nd-deps').split(',').map(s => s.trim()).filter(Boolean);
        CANVAS.nodes.push({
          id, type: val('nd-type'), model: val('nd-model'), datasource: val('nd-ds'),
          sql: val('nd-sql'), writeMode: '', dependsOn: deps,
          x: 60 + CANVAS.nodes.length * 20, y: 60 + CANVAS.nodes.length * 30,
        });
        deps.forEach(d => CANVAS.edges.push({ from: d, to: id }));
        CANVAS.dirty = true;
        closeModal(); drawCanvas(); setStatus('已新增 ' + id + '，记得保存');
        break;
      }
      case 'cv-save-node': {
        const node = CANVAS.nodes.find(n => n.id === el.dataset.id);
        if (!node) return;
        const newID = val('nd-id');
        if (newID !== node.id && CANVAS.nodes.some(n => n.id === newID)) {
          toast('节点 ID 已存在', 'err'); return;
        }
        node.id = newID;
        node.type = val('nd-type');
        node.model = val('nd-model');
        node.datasource = val('nd-ds');
        node.writeMode = val('nd-wm');
        node.sql = val('nd-sql');
        node.dependsOn = val('nd-deps').split(',').map(s => s.trim()).filter(Boolean);
        CANVAS.edges = [];
        CANVAS.nodes.forEach(n => n.dependsOn.forEach(d => CANVAS.edges.push({ from: d, to: n.id })));
        CANVAS.dirty = true;
        closeModal(); drawCanvas(); setStatus('已修改，记得保存');
        break;
      }
      case 'cv-del-node': {
        const id = el.dataset.id;
        CANVAS.nodes = CANVAS.nodes.filter(n => n.id !== id);
        CANVAS.nodes.forEach(n => { n.dependsOn = n.dependsOn.filter(d => d !== id); });
        CANVAS.edges = CANVAS.edges.filter(e => e.from !== id && e.to !== id);
        CANVAS.dirty = true;
        closeModal(); drawCanvas(); setStatus('已删除 ' + id);
        break;
      }
      case 'cv-autolayout': layoutByLevel(); setStatus('已自动布局'); break;
      case 'cv-save': {
        el.disabled = true;
        const spec = specFromCanvas();
        const yaml = yamlOf(spec);
        try {
          await api('/pipelines', { method: 'POST', body: { yaml, publish: true } });
          toast('已保存并发布', 'ok');
          CANVAS.dirty = false;
          await render();
        } catch (err) {
          modal('校验失败', `<div class="err-box">${esc(err.message)}</div>
            <div class="hint" style="margin-top:10px">以下是生成的 YAML，可直接修改后保存：</div>
            <textarea rows="16" style="margin-top:8px">${esc(yaml)}</textarea>`);
        }
        el.disabled = false;
        break;
      }
    }
  } catch (err) {
    toast(err.message, 'err');
    el.disabled = false;
  }
});
