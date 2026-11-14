/* GoDW 控制台：无框架，单文件应用。所有数据来自 /api/v1。 */
'use strict';

const API = '/api/v1';
const S = {
  route: 'overview',
  me: null,
  cache: {},
  selected: {},
  timer: null,
};

/* ---------------- 基础工具 ---------------- */

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function fmtTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (isNaN(d)) return esc(v);
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtDur(ms) {
  if (!ms) return '—';
  if (ms < 1000) return ms + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  return Math.floor(ms / 60000) + 'm' + Math.round((ms % 60000) / 1000) + 's';
}

function fmtBytes(n) {
  if (!n) return '0';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 10 || i === 0 ? 0 : 1) + u[i];
}

function statusPill(s) {
  const map = {
    success: ['ok', '成功'], ok: ['ok', '正常'], failed: ['err', '失败'], error: ['err', '异常'],
    running: ['running', '运行中'], queued: ['mute', '排队中'], pending: ['mute', '待执行'],
    blocked: ['warn', '阻断'], skipped: ['mute', '跳过'], canceled: ['mute', '已取消'],
    unknown: ['mute', '未知'], draft: ['mute', '草稿'], published: ['ok', '已发布'],
    archived: ['mute', '已归档'], waiting: ['warn', '等待'],
  };
  const [cls, label] = map[s] || ['mute', s || '—'];
  return `<span class="pill ${cls}">${esc(label)}</span>`;
}

function toast(msg, type) {
  const root = document.getElementById('toast-root');
  const div = document.createElement('div');
  div.className = 'toast ' + (type || '');
  div.textContent = msg;
  root.appendChild(div);
  setTimeout(() => div.remove(), type === 'err' ? 7000 : 3200);
}

function token() {
  try { return localStorage.getItem('godw.token') || ''; } catch (e) { return ''; }
}

function setToken(t) {
  try {
    if (t) localStorage.setItem('godw.token', t);
    else localStorage.removeItem('godw.token');
  } catch (e) { /* 隐私模式忽略 */ }
}

async function api(path, opts) {
  opts = opts || {};
  const init = { method: opts.method || 'GET', headers: {} };
  const tk = token();
  if (tk) init.headers['Authorization'] = 'Bearer ' + tk;
  if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  }
  const resp = await fetch(API + path, init);
  let data = null;
  try { data = await resp.json(); } catch (e) { /* 非 JSON */ }
  if (resp.status === 401) {
    setToken('');
    throw new Error('登录已过期，请重新登录');
  }
  if (!resp.ok || (data && data.code !== 0)) {
    let msg = (data && data.message) || ('HTTP ' + resp.status);
    if (data && data.data && Array.isArray(data.data.problems)) msg += '：' + data.data.problems.join('；');
    throw new Error(msg);
  }
  return data.data;
}

/**
 * modal(title, bodyHTML) —— 打开一个模态框（覆盖式，同时只允许一个）。
 *
 * 关闭路径全部走 document 上的事件委托（data-act），因此这里千万不能在
 * .modal 上写 onclick="event.stopPropagation()"：那会把冒泡掐在 .modal 上，
 * document 收不到点击，弹窗内所有 data-act 按钮（关闭/取消/保存…）集体失效。
 * 遮罩的「点空白关闭」改为在委托里判断 ev.target === 遮罩本身，效果一样且不挡事件。
 */
function modal(title, bodyHTML) {
  const root = document.getElementById('modal-root');
  root.innerHTML = `<div class="modal-mask" data-act="mask-close">
    <div class="modal" role="dialog" aria-modal="true" tabindex="-1" aria-label="${esc(title)}">
      <div class="row" style="margin-bottom:16px">
        <h3 style="flex:1">${esc(title)}</h3>
        <button class="sm ghost" data-act="close-modal" title="关闭（Esc）">关闭</button>
      </div>
      ${bodyHTML}
    </div>
  </div>`;
  // 键盘可达：打开即把焦点交给弹窗，避免 Tab 落在被遮住的页面上。
  const box = root.querySelector('.modal');
  if (box) box.focus?.();
}

function closeModal() {
  const root = document.getElementById('modal-root');
  if (root) root.innerHTML = '';
}

// Esc 关闭当前弹窗（抽屉兜底）。SQL 编辑器自己要用 Esc 收补全弹层，先让给它。
document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape') return;
  if (document.querySelector('.sqled-pop')) return;
  const ae = document.activeElement;
  if (ae && ae.closest && ae.closest('.sqled')) return;
  const root = document.getElementById('modal-root');
  if (root && root.firstElementChild) { closeModal(); return; }
  if (typeof closeDrawer === 'function') {
    const dr = document.getElementById('drawer-root');
    if (dr && dr.firstElementChild) closeDrawer();
  }
});

// val 读取表单值；治理与画布脚本共用。
function val(id) {
  const e = document.getElementById(id);
  return e ? e.value.trim() : '';
}

/* ---------------- 路由 ---------------- */

const ROUTES = {
  overview: { title: '工作台', sub: '全局状态与最近运行', render: pageOverview },
  instances: { title: '运行实例', sub: '调度、重跑与节点明细', render: pageInstances },
  sources: { title: '数据源', sub: '接入并采集元数据', render: pageSources },
  datasets: { title: '表结构', sub: '已采集的库表与字段', render: pageDatasets },
  models: { title: '模型', sub: '分层建模、DDL 编译与版本', render: pageModels },
  pipelines: { title: '管道', sub: 'DAG 编排、发布与触发', render: pagePipelines },
  metrics: { title: '指标', sub: '语义层口径与趋势', render: pageMetrics },
  dashboards: { title: '看板', sub: '指标组合与经营视图', render: pageDashboards },
  lineage: { title: '血缘', sub: '表与模型的上游依赖', render: pageLineage },
  quality: { title: '质量', sub: '规则结果与门禁', render: pageQuality },
  audit: { title: '审计', sub: '操作留痕', render: pageAudit },
};

function go(route) {
  location.hash = '#/' + route;
}

async function renderShare(token) {
  // 分享视图不带导航：外部访客只看内容，避免暴露内部结构
  document.body.classList.add('share-mode');
  document.getElementById('crumb').innerHTML = '分享视图<span>只读</span>';
  const view = document.getElementById('view');
  view.innerHTML = '<div class="empty">加载中…</div>';
  try {
    const r = await fetch('/api/v1/public/share/' + encodeURIComponent(token)).then(x => x.json());
    if (r.code !== 0) throw new Error(r.message || '分享不存在');
    const d = r.data;
    if (d.kind === 'dashboard') {
      view.innerHTML = `
        <div class="page-head"><div>
          <h2 class="page-title">${esc(d.target)}</h2>
          <p class="page-desc">${esc(d.data.description || '')} · 只读分享视图</p>
        </div></div>
        <div class="grid c2">
          ${d.data.cards.map((c, i) => `
            <div class="card" style="${c.span === 2 ? 'grid-column:1 / -1' : ''}">
              <div class="card-head"><div class="card-title">${esc(c.title || c.metric)}</div><div class="hint mono">${esc(c.metric)} · ${c.days}天</div></div>
              ${c.error ? `<div class="err-box">${esc(c.error)}</div>` : `<svg class="dag" id="sh-${i}" height="220"></svg>`}
            </div>`).join('')}
        </div>`;
      d.data.cards.forEach((c, i) => {
        if (!c.error && c.points && c.points.length) drawLineChart(document.getElementById('sh-' + i), c.points, c.unit);
      });
    } else {
      view.innerHTML = `
        <div class="page-head"><div>
          <h2 class="page-title">${esc(d.target)}</h2>
          <p class="page-desc">只读分享视图</p>
        </div></div>
        <div class="card"><svg class="dag" id="sh-metric" height="320"></svg></div>`;
      drawLineChart(document.getElementById('sh-metric'), d.data.points || [], d.data.unit);
    }
  } catch (e) {
    view.innerHTML = `<div class="card"><div class="err-box">${esc(e.message)}</div></div>`;
  }
}

async function render() {
  const hash = (location.hash || '#/overview').replace('#/', '') || 'overview';
  if (hash.startsWith('share/')) { await renderShare(hash.slice(6)); return; }
  document.body.classList.remove('share-mode');
  const route = hash;
  const def = ROUTES[route] || ROUTES.overview;
  S.route = route;

  document.querySelectorAll('.nav-item').forEach(el => {
    el.classList.toggle('active', el.dataset.route === route);
  });
  document.getElementById('crumb').innerHTML = esc(def.title) + `<span>${esc(def.sub)}</span>`;

  const view = document.getElementById('view');
  view.innerHTML = '<div class="empty">加载中…</div>';
  try {
    await def.render(view);
  } catch (err) {
    view.innerHTML = `<div class="card"><div class="err-box">${esc(err.message)}</div></div>`;
  }
}

/* ---------------- 工作台 ---------------- */

async function pageOverview(view) {
  const [ov, insts] = await Promise.all([api('/overview'), api('/instances?limit=12')]);
  const success = (insts || []).filter(i => i.status === 'success').length;
  const rate = insts.length ? Math.round(success * 100 / insts.length) : 0;

  view.innerHTML = `
    <div class="grid c4">
      <div class="stat"><div class="stat-label">数据源</div><div class="stat-value">${ov.datasources}</div><div class="stat-sub">已采集表：${ov.datasets || 0}</div></div>
      <div class="stat"><div class="stat-label">模型</div><div class="stat-value">${ov.models}</div><div class="stat-sub">覆盖 ODS 到 ADS</div></div>
      <div class="stat"><div class="stat-label">管道</div><div class="stat-value">${ov.pipelines}</div><div class="stat-sub">启用中：${(ov.pipelines || 0)}</div></div>
      <div class="stat"><div class="stat-label">近期成功率</div><div class="stat-value">${rate}%</div><div class="stat-sub">最近 ${insts.length} 次运行</div></div>
    </div>

    <div class="card" style="margin-top:16px">
      <div class="card-head">
        <div class="card-title">链路健康</div>
        <div class="hint">ODS → DWD → DWS → ADS</div>
      </div>
      <div id="pipeline-strip"><div class="empty">加载中…</div></div>
    </div>

    <div class="card">
      <div class="card-head">
        <div class="card-title">最近运行</div>
        <button class="sm ghost" data-act="go" data-route="instances">查看全部</button>
      </div>
      ${insts.length ? `<div class="table-wrap"><table>
        <thead><tr><th>实例</th><th>管道</th><th>业务日期</th><th>状态</th><th>开始</th><th>耗时</th></tr></thead>
        <tbody>${insts.map(i => `
          <tr class="clickable" data-act="open-instance" data-id="${esc(i.id)}" style="cursor:pointer">
            <td class="mono">${esc(i.id.slice(-8))}</td>
            <td>${esc(i.pipelineName || i.pipelineId.slice(-8))}</td>
            <td class="mono">${esc(i.bizDate)}</td>
            <td>${statusPill(i.status)}</td>
            <td class="dim small">${fmtTime(i.startedAt || i.createdAt)}</td>
            <td class="dim small">${i.startedAt && i.finishedAt ? fmtDur(new Date(i.finishedAt) - new Date(i.startedAt)) : '—'}</td>
          </tr>`).join('')}
        </tbody></table></div>` : `<div class="empty"><b>还没有运行记录</b>先接入数据源、发布模型与管道，再点右上角「执行一轮」</div>`}
    </div>`;

  // 管道条：用最近实例反映每个管道的健康度
  const pipes = await api('/pipelines');
  const strip = document.getElementById('pipeline-strip');
  if (!pipes.length) {
    strip.innerHTML = '<div class="empty"><b>尚未定义管道</b>在「管道」页创建第一条 ODS→DWD 链路</div>';
    return;
  }
  strip.innerHTML = pipes.map(p => {
    const ins = insts.filter(i => i.pipelineId === p.id);
    const last = ins[0];
    return `<div class="row" style="padding:9px 0;border-bottom:1px solid var(--line-soft)">
      <div style="width:210px">
        <div style="font-weight:500">${esc(p.name)}</div>
        <div class="mute small mono">${esc(p.schedule || '手动')}</div>
      </div>
      <div style="flex:1">${p.spec ? (p.spec.nodes || []).map(n => `<span class="pill mute" style="margin-right:5px">${esc(n.id)}</span>`).join('') : '<span class="mute small">未发布</span>'}</div>
      <div>${last ? statusPill(last.status) : '<span class="pill mute">未运行</span>'}</div>
      <div style="width:80px;text-align:right"><button class="sm ghost" data-act="trigger-pl" data-name="${esc(p.name)}">触发</button></div>
    </div>`;
  }).join('');
}

/* ---------------- 数据源 ---------------- */

async function pageSources(view) {
  const ds = await api('/datasources');
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">数据源</h2>
        <p class="page-desc">接入后先采集元数据，再据此建模。凭证以 AES-GCM 加密存储，接口不回传明文。</p>
      </div>
      <button class="primary" data-act="new-ds">接入数据源</button>
    </div>
    <div class="card">
      ${ds.length ? `<div class="table-wrap"><table>
        <thead><tr><th>名称</th><th>类型</th><th>地址</th><th>状态</th><th>最近检查</th><th style="width:230px">操作</th></tr></thead>
        <tbody>${ds.map(d => `
          <tr>
            <td><b>${esc(d.name)}</b></td>
            <td><span class="pill info">${esc(d.type)}</span></td>
            <td class="mono dim">${esc(d.config.host || '')}${d.config.port ? ':' + d.config.port : ''}${d.config.database ? '/' + d.config.database : ''}</td>
            <td>${statusPill(d.status)}${d.lastError ? `<div class="mute small" style="margin-top:3px">${esc(d.lastError.slice(0, 80))}</div>` : ''}</td>
            <td class="dim small">${fmtTime(d.checkedAt)}</td>
            <td>
              <button class="sm ghost" data-act="test-ds" data-name="${esc(d.name)}">测试</button>
              <button class="sm ghost" data-act="collect-ds" data-name="${esc(d.name)}">采集</button>
              <button class="sm danger" data-act="del-ds" data-name="${esc(d.name)}">删除</button>
            </td>
          </tr>`).join('')}
        </tbody></table></div>` : `<div class="empty"><b>还没有数据源</b>支持 PostgreSQL / MySQL / ClickHouse</div>`}
    </div>`;
}

async function newDatasourceModal() {
  const types = await api('/datasource-types');
  const names = Object.keys(types);
  modal('接入数据源', `
    <div class="stack">
      <div class="grid c2">
        <div><label>名称</label><input id="ds-name" placeholder="pg_main" /></div>
        <div><label>类型</label><select id="ds-type">${names.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('')}</select></div>
      </div>
      <div class="grid c3">
        <div><label>主机</label><input id="ds-host" value="127.0.0.1" /></div>
        <div><label>端口</label><input id="ds-port" value="5432" /></div>
        <div><label>数据库</label><input id="ds-db" value="dw" /></div>
      </div>
      <div class="grid c2">
        <div><label>用户名</label><input id="ds-user" value="postgres" /></div>
        <div><label>密码</label><input id="ds-pass" type="password" /></div>
      </div>
      <div class="hint">创建前会先真实连一次；连不上不会入库。</div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-ds">测试并保存</button>
      </div>
    </div>`);

  // 切换引擎时带出常用默认值，减少手填。
  const defaults = { postgres: 5432, mysql: 3306, clickhouse: 8123 };
  const typeEl = document.getElementById('ds-type');
  const portEl = document.getElementById('ds-port');
  const userEl = document.getElementById('ds-user');
  const dbEl = document.getElementById('ds-db');
  if (typeEl) typeEl.onchange = () => {
    const t = typeEl.value;
    if (defaults[t]) portEl.value = defaults[t];
    userEl.value = t === 'postgres' ? 'postgres' : (t === 'mysql' ? 'root' : 'default');
    if (dbEl.value === 'dw' || !dbEl.value) dbEl.value = t === 'clickhouse' ? 'default' : 'dw';
  };
}

/* ---------------- 表结构 ---------------- */

async function pageDatasets(view) {
  const ds = await api('/datasources');
  const dsid = S.selected.datasource || (ds[0] && ds[0].id) || '';
  const page = await api('/datasets?datasourceId=' + encodeURIComponent(dsid) + '&pageSize=200');
  S.cache.datasetCount = page.total;

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">表结构</h2>
        <p class="page-desc">采集自数据源的物理表快照。建模时可直接引用这些字段。</p>
      </div>
      <select id="ds-filter" style="width:220px">
        ${ds.map(d => `<option value="${esc(d.id)}" ${d.id === dsid ? 'selected' : ''}>${esc(d.name)}</option>`).join('')}
      </select>
    </div>
    <div class="card">
      ${page.items.length ? `<div class="table-wrap"><table>
        <thead><tr><th>Schema</th><th>表</th><th>类型</th><th>行数</th><th>大小</th><th>列数</th><th>采集时间</th></tr></thead>
        <tbody>${page.items.map(t => `
          <tr data-act="open-table" data-id="${esc(t.id)}" style="cursor:pointer">
            <td class="mono dim">${esc(t.schema)}</td>
            <td><b>${esc(t.name)}</b>${t.comment ? `<div class="mute small">${esc(t.comment)}</div>` : ''}</td>
            <td><span class="pill mute">${esc(t.type)}</span></td>
            <td class="mono">${(t.rowCount || 0).toLocaleString()}</td>
            <td class="mono dim">${fmtBytes(t.sizeBytes)}</td>
            <td class="mono dim">${(t.columns || []).length}</td>
            <td class="dim small">${fmtTime(t.collectedAt)}</td>
          </tr>`).join('')}
        </tbody></table></div>` : `<div class="empty"><b>还没有采集到表</b>在「数据源」页点一次「采集」</div>`}
    </div>`;
  S.cache.datasets = page.items;

  const sel = document.getElementById('ds-filter');
  if (sel) sel.onchange = () => { S.selected.datasource = sel.value; render(); };
}

function openTable(id) {
  const t = (S.cache.datasets || []).find(x => x.id === id);
  if (!t) return;
  modal(`${t.schema}.${t.name}`, `
    <div class="hint" style="margin-bottom:12px">${esc(t.comment || '无注释')} · ${(t.rowCount || 0).toLocaleString()} 行 · ${fmtBytes(t.sizeBytes)}</div>
    <div class="table-wrap"><table>
      <thead><tr><th>#</th><th>列名</th><th>类型</th><th>可空</th><th>注释</th></tr></thead>
      <tbody>${(t.columns || []).map((c, i) => `<tr>
        <td class="mono mute">${i + 1}</td><td class="mono">${esc(c.name)}</td>
        <td class="mono dim">${esc(c.type)}</td><td>${c.nullable ? '是' : '否'}</td>
        <td class="dim small">${esc(c.comment || '')}</td></tr>`).join('')}
      </tbody></table></div>`);
}

/* ---------------- 模型 ---------------- */

const MODEL_TPL = `kind: Model
name: dwd_order
layer: dwd
description: 订单明细，按天分区
datasource: pg_main
schema: dw
columns:
  - name: order_id
    type: bigint
    nullable: false
    comment: 订单号
  - name: user_id
    type: bigint
    nullable: false
    comment: 用户ID
  - name: amount
    type: decimal
    nullable: false
    comment: 订单金额
  - name: status
    type: string
    nullable: false
  - name: dt
    type: date
    nullable: false
    comment: 分区日期
primary_key: [order_id]
unique_key: [order_id]
partition_by: [dt]
indexes:
  - columns: [user_id, dt]
`;

async function pageModels(view) {
  const models = await api('/models');
  S.cache.models = models;
  const cur = S.selected.model || (models[0] && models[0].name) || '';
  const m = models.find(x => x.name === cur);

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">模型</h2>
        <p class="page-desc">YAML 是唯一事实来源。发布后生成不可变版本，执行与重跑都锁定版本快照。</p>
      </div>
      <button class="primary" data-act="new-model">新建模型</button>
    </div>
    <div class="split">
      <div class="card" style="padding:8px">
        ${models.length ? models.map(x => `
          <div class="list-item ${x.name === cur ? 'active' : ''}" data-act="sel-model" data-name="${esc(x.name)}">
            <div class="name">${esc(x.name)}</div>
            <div class="meta">${esc(x.layer)} · ${x.activeVersion ? 'v' + x.activeVersion : '未发布'}</div>
          </div>`).join('') : '<div class="empty"><b>暂无模型</b></div>'}
      </div>
      <div>
        ${m ? modelDetail(m) : `<div class="card"><div class="empty"><b>选择或新建一个模型</b>建议顺序：ODS → DWD → DWS → ADS</div></div>`}
      </div>
    </div>`;

  if (m) {
    api('/models/' + encodeURIComponent(m.name) + '/versions').then(vs => {
      const box = document.getElementById('version-box');
      if (!box) return;
      box.innerHTML = vs.length ? vs.map(v => `
        <div class="row" style="padding:7px 0;border-bottom:1px solid var(--line-soft)">
          <div class="mono" style="width:56px">v${v.version}</div>
          <div style="flex:1">${statusPill(v.status)}</div>
          <div class="dim small" style="width:150px">${fmtTime(v.createdAt)}</div>
          ${v.status !== 'published' ? `<button class="sm ghost" data-act="rollback-model" data-name="${esc(m.name)}" data-version="${v.version}">回滚到此版本</button>` : '<span class="mute small">当前生效</span>'}
        </div>`).join('') : '<div class="hint">尚无版本</div>';
    }).catch(() => {});
  }
}

function modelDetail(m) {
  const spec = m.spec || {};
  return `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">${esc(m.name)} <span class="pill info">${esc(m.layer)}</span></div>
          <div class="hint" style="margin-top:4px">${esc(m.description || '无描述')} · 数据源 ${esc(spec.datasource || '—')} · 已发布 v${m.activeVersion}</div>
        </div>
        <div class="row">
          <button class="sm ghost" data-act="edit-model" data-name="${esc(m.name)}">编辑定义</button>
        </div>
      </div>
      ${(spec.columns || []).length ? `<div class="table-wrap"><table>
        <thead><tr><th>列</th><th>类型</th><th>可空</th><th>注释</th></tr></thead>
        <tbody>${spec.columns.map(c => `<tr>
          <td class="mono">${esc(c.name)}</td><td class="mono dim">${esc(c.type)}</td>
          <td>${c.nullable ? '是' : '否'}</td><td class="dim small">${esc(c.comment || '')}</td>
        </tr>`).join('')}</tbody></table></div>` : '<div class="hint">未发布版本，无列信息</div>'}
    </div>
    <div class="card">
      <div class="card-head"><div class="card-title">编译后的 DDL</div></div>
      <pre class="code">${esc(m.ddl || '—')}</pre>
    </div>
    <div class="card">
      <div class="card-head"><div class="card-title">版本历史</div></div>
      <div id="version-box"><div class="hint">加载中…</div></div>
    </div>`;
}

function modelEditor(yaml) {
  modal('模型定义', `
    <div class="stack">
      <textarea id="model-yaml" rows="22">${esc(yaml || MODEL_TPL)}</textarea>
      <div class="row">
        <div class="hint" style="flex:1">发布前会先编译 DDL；勾选「同时建表」会真正执行 DDL。</div>
      </div>
      <div class="row" style="justify-content:flex-end">
        <label class="row" style="margin:0;gap:6px;font-size:12.5px;color:var(--text-dim)">
          <input type="checkbox" id="model-apply" style="width:auto" /> 同时建表
        </label>
        <button class="ghost" data-act="preview-ddl">预览 DDL</button>
        <button class="primary" data-act="save-model">校验并发布</button>
      </div>
      <div id="ddl-preview"></div>
    </div>`);
}

function modelYamlFrom(m) {
  const s = m.spec;
  if (!s) return MODEL_TPL;
  const lines = ['kind: Model', `name: ${s.name}`, `layer: ${s.layer}`];
  if (s.description) lines.push(`description: ${s.description}`);
  lines.push(`datasource: ${s.datasource}`);
  if (s.schema) lines.push(`schema: ${s.schema}`);
  lines.push('columns:');
  (s.columns || []).forEach(c => {
    lines.push(`  - name: ${c.name}`);
    lines.push(`    type: ${c.type}`);
    lines.push(`    nullable: ${!!c.nullable}`);
    if (c.comment) lines.push(`    comment: ${c.comment}`);
  });
  if (s.primaryKey && s.primaryKey.length) lines.push(`primary_key: [${s.primaryKey.join(', ')}]`);
  if (s.uniqueKey && s.uniqueKey.length) lines.push(`unique_key: [${s.uniqueKey.join(', ')}]`);
  if (s.partitionBy && s.partitionBy.length) lines.push(`partition_by: [${s.partitionBy.join(', ')}]`);
  return lines.join('\n') + '\n';
}

/* ---------------- 管道 ---------------- */

const PIPE_TPL = `kind: Pipeline
name: dwd_order_daily
description: 订单明细日更
schedule: "0 2 * * *"
nodes:
  - id: src_order
    type: source
    datasource: pg_main
    sql: SELECT * FROM ods."order" WHERE dt = \${biz_date}
  - id: dwd_order
    type: sql
    depends_on: [src_order]
    model: dwd_order
    write_mode: upsert
    datasource: pg_main
    sql: >
      SELECT order_id, user_id, amount, status, dt
      FROM ods."order"
      WHERE dt = \${biz_date}
`;

async function pagePipelines(view) {
  const pipes = await api('/pipelines');
  S.cache.pipelines = pipes;
  const cur = S.selected.pipeline || (pipes[0] && pipes[0].name) || '';
  const p = pipes.find(x => x.name === cur);

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">管道</h2>
        <p class="page-desc">同层节点并发、层间串行。发布后的版本可重跑，不受后续修改影响。</p>
      </div>
      <button class="primary" data-act="new-pipeline">新建管道</button>
    </div>
    <div class="split">
      <div class="card" style="padding:8px">
        ${pipes.length ? pipes.map(x => `
          <div class="list-item ${x.name === cur ? 'active' : ''}" data-act="sel-pipeline" data-name="${esc(x.name)}">
            <div class="name">${esc(x.name)}</div>
            <div class="meta">${esc(x.schedule || '手动')} · ${x.activeVersion ? 'v' + x.activeVersion : '未发布'}</div>
          </div>`).join('') : '<div class="empty"><b>暂无管道</b></div>'}
      </div>
      <div>${p ? pipelineDetail(p) : `<div class="card"><div class="empty"><b>选择或新建管道</b>从一条 source → sql 的最小链路开始</div></div>`}</div>
    </div>`;

  if (p && p.spec) drawDAG(document.getElementById('dag-' + p.name), p.spec.nodes || []);
}

function pipelineDetail(p) {
  const spec = p.spec || {};
  return `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">${esc(p.name)} ${p.enabled ? '<span class="pill ok">启用</span>' : '<span class="pill mute">停用</span>'}</div>
          <div class="hint" style="margin-top:4px">${esc(p.description || '无描述')} · cron <span class="mono">${esc(p.schedule || '手动')}</span> · ${esc(p.timezone)}</div>
        </div>
        <div class="row">
          <button class="sm ghost" data-act="toggle-pl" data-name="${esc(p.name)}" data-enabled="${p.enabled}">${p.enabled ? '停用' : '启用'}</button>
          <button class="sm ghost" data-act="edit-pipeline" data-name="${esc(p.name)}">编辑</button>
          <button class="sm primary" data-act="trigger-pl" data-name="${esc(p.name)}">立即运行</button>
        </div>
      </div>
      <div class="hint" style="margin-bottom:8px">执行图（同层并发）</div>
      <svg class="dag" id="dag-${esc(p.name)}" height="240"></svg>
    </div>
    <div class="card">
      <div class="card-head"><div class="card-title">节点</div></div>
      <div class="table-wrap"><table>
        <thead><tr><th>ID</th><th>类型</th><th>依赖</th><th>目标模型</th><th>写入</th></tr></thead>
        <tbody>${(spec.nodes || []).map(n => `<tr>
          <td class="mono">${esc(n.id)}</td>
          <td><span class="pill info">${esc(n.type)}</span></td>
          <td class="mono dim">${esc((n.dependsOn || []).join(', ') || '—')}</td>
          <td class="mono">${esc(n.model || '—')}</td>
          <td class="mono dim">${esc(n.writeMode || 'append')}</td>
        </tr>`).join('')}</tbody></table></div>
    </div>`;
}

function drawDAG(svg, nodes) {
  if (!svg || !nodes.length) return;
  const byId = {};
  nodes.forEach(n => byId[n.id] = n);
  const level = {};
  const calc = (id, seen) => {
    if (level[id] !== undefined) return level[id];
    if (seen[id]) return 0;
    seen[id] = true;
    const deps = (byId[id] && byId[id].dependsOn) || [];
    let max = -1;
    deps.forEach(d => { max = Math.max(max, calc(d, seen)); });
    level[id] = max + 1;
    return level[id];
  };
  nodes.forEach(n => calc(n.id, {}));

  const groups = {};
  nodes.forEach(n => {
    const l = level[n.id] || 0;
    (groups[l] = groups[l] || []).push(n);
  });

  const maxLevel = Math.max.apply(null, Object.keys(groups).map(Number));
  const maxRows = Math.max.apply(null, Object.values(groups).map(g => g.length));
  const w = 160, h = 46, gapX = 70, gapY = 26, padX = 26, padY = 26;
  const width = padX * 2 + (maxLevel + 1) * w + maxLevel * gapX;
  const height = Math.max(120, padY * 2 + maxRows * h + (maxRows - 1) * gapY);

  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('height', height);
  svg.innerHTML = '';

  const pos = {};
  Object.keys(groups).forEach(l => {
    groups[l].forEach((n, i) => {
      const total = groups[l].length;
      const y = padY + (height - padY * 2 - total * h - (total - 1) * gapY) / 2 + i * (h + gapY);
      pos[n.id] = { x: padX + Number(l) * (w + gapX), y };
    });
  });

  const ns = 'http://www.w3.org/2000/svg';
  const edges = [];
  nodes.forEach(n => (n.dependsOn || []).forEach(d => {
    if (!pos[d] || !pos[n.id]) return;
    const a = pos[d], b = pos[n.id];
    const x1 = a.x + w, y1 = a.y + h / 2, x2 = b.x, y2 = b.y + h / 2;
    const mx = (x1 + x2) / 2;
    const path = document.createElementNS(ns, 'path');
    path.setAttribute('class', 'edge');
    path.setAttribute('d', `M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`);
    path.setAttribute('marker-end', 'url(#arrow)');
    edges.push(path);
  }));

  const defs = document.createElementNS(ns, 'defs');
  defs.innerHTML = `<marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="#33465c"></path></marker>`;
  svg.appendChild(defs);
  edges.forEach(e => svg.appendChild(e));

  nodes.forEach(n => {
    const p = pos[n.id];
    const g = document.createElementNS(ns, 'g');
    const rect = document.createElementNS(ns, 'rect');
    rect.setAttribute('class', 'node-rect');
    rect.setAttribute('x', p.x); rect.setAttribute('y', p.y);
    rect.setAttribute('width', w); rect.setAttribute('height', h);
    rect.setAttribute('rx', 8);
    g.appendChild(rect);

    const t1 = document.createElementNS(ns, 'text');
    t1.setAttribute('x', p.x + 12); t1.setAttribute('y', p.y + 19);
    t1.textContent = n.id;
    g.appendChild(t1);

    const t2 = document.createElementNS(ns, 'text');
    t2.setAttribute('x', p.x + 12); t2.setAttribute('y', p.y + 34);
    t2.setAttribute('style', 'fill:#6b7887;font-size:10px');
    t2.textContent = n.type + (n.model ? ' · ' + n.model : '');
    g.appendChild(t2);

    svg.appendChild(g);
  });
}

function pipelineEditor(yaml) {
  modal('管道定义', `
    <div class="stack">
      <textarea id="pipe-yaml" rows="24">${esc(yaml || PIPE_TPL)}</textarea>
      <div class="hint">变量语法 <span class="mono">\${biz_date}</span>，运行期替换为实例业务日期。</div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-pipeline">校验并发布</button>
      </div>
    </div>`);
}

/* ---------------- 实例 ---------------- */

async function pageInstances(view) {
  const insts = await api('/instances?limit=80');
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">运行实例</h2>
        <p class="page-desc">一次调度对应一个实例。重跑复用原版本快照与业务日期，已成功的节点会跳过。</p>
      </div>
      <button class="primary" data-act="run-once">执行一轮</button>
    </div>
    <div class="card">
      ${insts.length ? `<div class="table-wrap"><table>
        <thead><tr><th>实例</th><th>管道</th><th>业务日期</th><th>触发</th><th>状态</th><th>开始</th><th>耗时</th><th></th></tr></thead>
        <tbody>${insts.map(i => `<tr>
          <td class="mono">${esc(i.id.slice(-10))}</td>
          <td>${esc(i.pipelineName || '—')}</td>
          <td class="mono">${esc(i.bizDate)}</td>
          <td><span class="pill mute">${esc(i.triggerType)}</span></td>
          <td>${statusPill(i.status)}</td>
          <td class="dim small">${fmtTime(i.startedAt || i.createdAt)}</td>
          <td class="dim small">${i.startedAt && i.finishedAt ? fmtDur(new Date(i.finishedAt) - new Date(i.startedAt)) : '—'}</td>
          <td><button class="sm ghost" data-act="open-instance" data-id="${esc(i.id)}">详情</button></td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><b>还没有实例</b>触发一条管道即可看到节点级执行过程</div>`}
    </div>`;
}

async function openInstance(id) {
  const data = await api('/instances/' + encodeURIComponent(id));
  const inst = data.instance;
  const tasks = inst.tasks || [];
  const qs = data.quality || [];

  modal('实例 ' + inst.id.slice(-10), `
    <dl class="kv" style="margin-bottom:16px">
      <dt>状态</dt><dd>${statusPill(inst.status)}</dd>
      <dt>业务日期</dt><dd class="mono">${esc(inst.bizDate)}</dd>
      <dt>触发方式</dt><dd>${esc(inst.triggerType)}</dd>
      <dt>版本</dt><dd class="mono">v${inst.pipelineVer}</dd>
      <dt>开始 / 结束</dt><dd class="small">${fmtTime(inst.startedAt)} → ${fmtTime(inst.finishedAt)}</dd>
      ${inst.error ? `<dt>错误</dt><dd><div class="err-box">${esc(inst.error)}</div></dd>` : ''}
    </dl>
    <div class="card-title" style="margin-bottom:8px">节点</div>
    <div class="table-wrap"><table>
      <thead><tr><th>节点</th><th>类型</th><th>状态</th><th>影响行数</th><th>耗时</th></tr></thead>
      <tbody>${tasks.length ? tasks.map(t => `<tr>
        <td class="mono">${esc(t.nodeId)}</td>
        <td class="dim">${esc(t.nodeType)}</td>
        <td>${statusPill(t.status)}${t.error ? `<div class="mute small">${esc(t.error.slice(0, 90))}</div>` : ''}</td>
        <td class="mono">${t.rowsAffected || 0}</td>
        <td class="mono dim">${fmtDur(t.durationMs)}</td>
      </tr>`).join('') : '<tr><td colspan="5" class="mute">尚无节点记录</td></tr>'}</tbody>
    </table></div>
    ${qs.length ? `<div class="card-title" style="margin:18px 0 8px">质量结果</div>
      <div class="table-wrap"><table>
        <thead><tr><th>规则</th><th>结果</th><th>实测</th><th>期望</th></tr></thead>
        <tbody>${qs.map(q => `<tr>
          <td class="mono">${esc(q.ruleName)}</td>
          <td>${q.passed ? '<span class="pill ok">通过</span>' : '<span class="pill err">未通过</span>'}</td>
          <td class="mono">${esc(q.observed)}</td><td class="mono dim">${esc(q.expected)}</td>
        </tr>`).join('')}</tbody></table></div>` : ''}
    <div class="row" style="margin-top:18px;justify-content:flex-end">
      <button class="ghost" data-act="close-modal">关闭</button>
      <button class="primary" data-act="retry-inst" data-id="${esc(inst.id)}">重跑</button>
    </div>`);
}

/* ---------------- 血缘 ---------------- */

async function pageLineage(view) {
  const g = await api('/lineage');
  const models = await api('/models');
  const anchor = S.selected.lineageTable || '';
  const dir = S.selected.lineageDir || 'both';

  // 焦点列可以落在别的表上（顺着链路一路点过去），所以表名从焦点里取。
  const focusRef = S.selected.lineageFocus || '';
  const dot = focusRef.lastIndexOf('.');
  const focusTable = dot > 0 ? focusRef.slice(0, dot) : anchor;
  const focusCol = dot > 0 ? focusRef.slice(dot + 1) : '';

  let impact = null, cg = null, err = '';
  if (anchor) {
    try {
      cg = await api('/lineage/column-graph?depth=6&direction=' + dir +
        '&table=' + encodeURIComponent(focusTable) + '&column=' + encodeURIComponent(focusCol));
      // 没显式选列时后端会挑一列，影响分析要跟着这一列走，否则「聚焦」和「影响面」对不上。
      const effCol = focusCol || (cg.focus && cg.focus.column) || '';
      impact = await api('/lineage/impact?table=' + encodeURIComponent(focusTable) +
        '&column=' + encodeURIComponent(effCol) + '&depth=6');
    } catch (e) { err = e.message; }
  }

  // 下钻可能已经走到别的表，字段下拉始终跟着「当前表」，否则会显示成未选中。
  const focusModel = models.find(m => m.name === focusTable);
  const cols = (focusModel && focusModel.spec && focusModel.spec.columns) || [];
  const anchorCol = focusCol || (cg && cg.focus ? cg.focus.column : '');
  const focusLabel = cg && cg.focus && cg.focus.column ? cg.focus.table + '.' + cg.focus.column : '未选择';

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">血缘</h2>
        <p class="page-desc">表级来自 depends_on，列级来自 SQL 解析。点表看字段，点字段追整条链路。</p>
      </div>
      <select id="lg-table" style="width:240px">
        <option value="">选择模型开始下钻</option>
        ${models.map(m => `<option value="${esc(m.name)}" ${m.name === anchor ? 'selected' : ''}>${esc(m.name)}</option>`).join('')}
      </select>
    </div>

    <div class="card">
      <div class="card-head">
        <div class="card-title">表级依赖</div>
        <div class="row">
          <label class="hint" style="display:flex;align-items:center;gap:6px;margin:0">
            <input type="checkbox" id="lg-ds" ${S.selected.lineageShowDS ? 'checked' : ''} style="width:auto" /> 显示数据源
          </label>
          <div class="hint">${g.nodes.length} 个节点 · ${g.edges.length} 条边${anchor ? '' : ' · 点击节点可下钻'}</div>
        </div>
      </div>
      ${g.edges.length ? `<svg class="dag" id="lineage-svg" height="360"></svg>` : `<div class="empty"><b>暂无血缘</b>发布带 depends_on 的管道后自动生成</div>`}
    </div>

    ${err ? `<div class="card"><div class="err-box">${esc(err)}</div></div>` : ''}

    ${anchor ? `
    <div class="card">
      <div class="card-head" style="gap:10px;flex-wrap:wrap">
        <div class="card-title">列级血缘</div>
        <select id="lg-col" style="width:210px">
          <option value="">选择字段</option>
          ${cols.map(c => `<option value="${esc(c.name)}" ${c.name === anchorCol ? 'selected' : ''}>${esc(c.name)}${c.comment ? ' · ' + esc(c.comment) : ''}</option>`).join('')}
        </select>
        <div class="row" style="gap:4px">
          <button class="${dir === 'both' ? 'primary' : 'ghost'}" data-dir="both" style="padding:4px 10px">双向</button>
          <button class="${dir === 'up' ? 'primary' : 'ghost'}" data-dir="up" style="padding:4px 10px">只看上游</button>
          <button class="${dir === 'down' ? 'primary' : 'ghost'}" data-dir="down" style="padding:4px 10px">只看下游</button>
        </div>
        <div class="spacer"></div>
        ${cg ? `<div class="hint">聚焦 <span class="mono">${esc(focusLabel)}</span> · 上游 ${cg.upstreamCount} 列 · 下游 ${cg.downstreamCount} 列${cg.truncated ? ' · 已达节点上限' : ''}</div>` : ''}
      </div>
      ${focusTable && focusTable !== anchor ? `<div class="hint" style="margin-bottom:8px">
          入口 <span class="mono">${esc(anchor)}</span> → 当前 <span class="mono">${esc(focusTable)}</span>
          <a href="#" id="lg-back" style="margin-left:8px">回到入口</a>
        </div>` : ''}
      <div class="hint" style="margin-bottom:8px">
        <span class="pill mute">PK</span> 主键　<span class="pill mute">分区</span> 分区列　右侧灰字为字段类型，灰显表为非本工作区模型
      </div>
      ${cg && cg.nodes.length ? `<div class="col-wrap"><svg class="col-dag" id="col-svg"></svg></div>`
        : `<div class="empty">${cg && cg.focus.column
            ? '该字段没有列级血缘（SELECT * 或解析失败时会缺失）'
            : '该表还没有列级血缘，发布含这张表的管道后自动生成'}</div>`}
    </div>

    <div class="grid c2">
      <div class="card">
        <div class="card-head"><div class="card-title">列级映射</div><div class="hint">${cg ? cg.edges.length : 0} 条</div></div>
        ${cg && cg.edges.length ? `<div class="table-wrap"><table>
          <thead><tr><th>上游列</th><th></th><th>下游列</th></tr></thead>
          <tbody>${cg.edges.slice(0, 80).map(e => `<tr>
            <td class="mono">${esc(e.source)}</td>
            <td class="dim" style="width:20px">→</td>
            <td class="mono">${esc(e.target)}</td>
          </tr>`).join('')}</tbody></table></div>` : `<div class="empty">${cg && cg.focus.column
            ? (dir === 'down' ? '该字段是链路末端，没有下游映射' : dir === 'up' ? '该字段是链路源头，没有上游映射' : '该字段没有列级映射')
            : '该表没有列级血缘（发布含这张表的管道后生成）'}</div>`}
      </div>
      <div class="card">
        <div class="card-head"><div class="card-title">影响分析</div><div class="hint">${impact ? impact.count + ' 个下游列' : '—'}</div></div>
        ${impact && impact.tables.length ? `
          <div style="margin-bottom:10px">${impact.tables.map(t => `<span class="pill info" style="margin-right:6px">${esc(t)}</span>`).join('')}</div>
          <div class="table-wrap"><table>
            <thead><tr><th>下游表</th><th>下游列</th></tr></thead>
            <tbody>${impact.edges.slice(0, 30).map(e => `<tr>
              <td class="mono">${esc(e.downstreamTable)}</td>
              <td class="mono dim">${esc(e.downstreamColumn)}</td>
            </tr>`).join('')}</tbody></table></div>` : `<div class="empty">没有下游依赖，可安全改动</div>`}
      </div>
    </div>` : ''}`;

  // 数据源节点默认隐藏：它只说明「这张表在哪」，混在模型链路里会干扰阅读
  const showDS = !!S.selected.lineageShowDS;
  const gv = { nodes: g.nodes, edges: g.edges };
  if (!showDS) {
    const dsNodes = new Set(g.nodes.filter(n => n.startsWith('datasource.')));
    gv.nodes = g.nodes.filter(n => !dsNodes.has(n));
    gv.edges = g.edges.filter(e => !dsNodes.has(e.source) && !dsNodes.has(e.target));
  }
  if (gv.edges.length) drawLineage(document.getElementById('lineage-svg'), gv, anchor);
  else if (g.edges.length) {
    const svg = document.getElementById('lineage-svg');
    if (svg) svg.outerHTML = '<div class="empty">只记录了数据源依赖，勾选上方「显示数据源」查看</div>';
  }

  const t = document.getElementById('lg-table');
  if (t) t.onchange = () => {
    S.selected.lineageTable = t.value;
    S.selected.lineageFocus = '';
    render();
  };
  const cb = document.getElementById('lg-ds');
  if (cb) cb.onchange = () => { S.selected.lineageShowDS = cb.checked; render(); };

  const chart = document.getElementById('lineage-svg');
  if (chart && gv.edges.length) {
    chart.onclick = ev => {
      const hit = ev.target.closest('[data-node]');
      if (!hit) return;
      const id = hit.getAttribute('data-node');
      // 数据源节点没有模型定义，点它没有可下钻的字段，直接忽略。
      if (!id || id.startsWith('datasource.')) return;
      S.selected.lineageTable = id;
      S.selected.lineageFocus = '';
      render();
    };
  }

  const colSel = document.getElementById('lg-col');
  if (colSel) colSel.onchange = () => {
    S.selected.lineageFocus = colSel.value ? focusTable + '.' + colSel.value : '';
    render();
  };
  const back = document.getElementById('lg-back');
  if (back) back.onclick = ev => { ev.preventDefault(); S.selected.lineageFocus = ''; render(); };
  view.querySelectorAll('[data-dir]').forEach(b => {
    b.onclick = () => { S.selected.lineageDir = b.getAttribute('data-dir'); render(); };
  });
  const csvg = document.getElementById('col-svg');
  if (csvg && cg) {
    drawColumnLineage(csvg, cg);
    csvg.addEventListener('click', ev => {
      const hit = ev.target.closest('[data-col]');
      if (!hit) return;
      S.selected.lineageFocus = hit.getAttribute('data-col');
      render();
    });
  }
}

// drawColumnLineage 按表画泳道，列是节点，边是列映射。
// 表按血缘层级自左向右排，列在表内按 主键→分区→普通字段 排序。
function drawColumnLineage(svg, g) {
  const ns = 'http://www.w3.org/2000/svg';
  const focus = g.focus && g.focus.column ? g.focus.table + '.' + g.focus.column : '';
  const tables = new Map();
  g.nodes.forEach(n => {
    if (!tables.has(n.table)) tables.set(n.table, { name: n.table, layer: n.layer || '', cols: [] });
    const t = tables.get(n.table);
    if (n.layer) t.layer = n.layer;
    t.cols.push(n);
  });
  const kindRank = { key: 0, partition: 1, field: 2 };
  tables.forEach(t => t.cols.sort((a, b) =>
    (kindRank[a.kind] == null ? 3 : kindRank[a.kind]) - (kindRank[b.kind] == null ? 3 : kindRank[b.kind]) ||
    a.column.localeCompare(b.column)));

  // 表级分层：把列边投影成表边，Kahn 求最长路径。
  const names = [...tables.keys()];
  const inDeg = {}, adj = {};
  names.forEach(n => { inDeg[n] = 0; adj[n] = []; });
  const seen = new Set();
  g.edges.forEach(e => {
    const a = e.source.slice(0, e.source.lastIndexOf('.'));
    const b = e.target.slice(0, e.target.lastIndexOf('.'));
    if (a === b || !tables.has(a) || !tables.has(b)) return;
    const k = a + '\u0000' + b;
    if (seen.has(k)) return;
    seen.add(k);
    adj[a].push(b);
    inDeg[b]++;
  });
  const level = {};
  const q = names.filter(n => inDeg[n] === 0);
  q.forEach(n => { level[n] = 0; });
  const queue = q.slice();
  while (queue.length) {
    const cur = queue.shift();
    adj[cur].forEach(nx => {
      level[nx] = Math.max(level[nx] || 0, (level[cur] || 0) + 1);
      if (--inDeg[nx] === 0) queue.push(nx);
    });
  }
  names.forEach(n => { if (level[n] === undefined) level[n] = 0; });

  const boxW = 216, headH = 26, rowH = 22, gapX = 64, gapY = 22, pad = 18;
  const byLevel = {};
  names.forEach(n => (byLevel[level[n]] = byLevel[level[n]] || []).push(n));
  const maxLevel = Math.max.apply(null, Object.keys(byLevel).map(Number));
  const boxH = t => headH + t.cols.length * rowH + 8;
  const levelH = {};
  Object.keys(byLevel).forEach(l => {
    const arr = byLevel[l];
    levelH[l] = arr.reduce((s, n) => s + boxH(tables.get(n)), 0) + gapY * (arr.length - 1);
  });
  const contentH = Math.max.apply(null, Object.values(levelH));
  const width = pad * 2 + (maxLevel + 1) * boxW + maxLevel * gapX;
  const height = Math.max(120, pad * 2 + contentH);

  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('width', width);
  svg.setAttribute('height', height);
  svg.innerHTML = `<defs><marker id="col-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
      <path d="M0,0 L8,4 L0,8 z" fill="#3d5169"/></marker>
    <marker id="col-arrow-hl" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
      <path d="M0,0 L8,4 L0,8 z" fill="#58a6ff"/></marker></defs>`;

  const pos = {};
  Object.keys(byLevel).forEach(l => {
    let y = (height - levelH[l]) / 2;
    byLevel[l].forEach(name => {
      const t = tables.get(name);
      const h = boxH(t);
      t.x = pad + Number(l) * (boxW + gapX);
      t.y = y;
      t.h = h;
      t.cols.forEach((c, i) => { pos[c.id] = { x: t.x, y: t.y + headH + i * rowH + rowH / 2, table: name }; });
      y += h + gapY;
    });
  });

  // 先画边，再画框，避免线压在文字上。
  g.edges.forEach(e => {
    const a = pos[e.source], b = pos[e.target];
    if (!a || !b) return;
    const hl = !!focus && (e.source === focus || e.target === focus);
    const p = document.createElementNS(ns, 'path');
    p.setAttribute('class', 'col-edge' + (hl ? ' hl' : ''));
    p.setAttribute('marker-end', hl ? 'url(#col-arrow-hl)' : 'url(#col-arrow)');
    const x1 = a.x + boxW, y1 = a.y, x2 = b.x, y2 = b.y, mx = (x1 + x2) / 2;
    p.setAttribute('d', x2 >= x1
      ? `M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`
      : `M ${x1} ${y1} C ${x1 + 40} ${y1}, ${x2 - 40} ${y2}, ${x2} ${y2}`);
    svg.appendChild(p);
  });

  tables.forEach(t => {
    const box = document.createElementNS(ns, 'rect');
    box.setAttribute('class', 'tbl-box');
    box.setAttribute('x', t.x); box.setAttribute('y', t.y);
    box.setAttribute('width', boxW); box.setAttribute('height', t.h);
    box.setAttribute('rx', 8);
    svg.appendChild(box);

    const head = document.createElementNS(ns, 'path');
    head.setAttribute('class', 'tbl-head');
    head.setAttribute('d', `M ${t.x} ${t.y + headH} L ${t.x} ${t.y + 8} Q ${t.x} ${t.y} ${t.x + 8} ${t.y}
      L ${t.x + boxW - 8} ${t.y} Q ${t.x + boxW} ${t.y} ${t.x + boxW} ${t.y + 8} L ${t.x + boxW} ${t.y + headH} Z`);
    svg.appendChild(head);

    const title = document.createElementNS(ns, 'text');
    title.setAttribute('class', 'tbl-title');
    title.setAttribute('x', t.x + 9);
    title.setAttribute('y', t.y + 17);
    const layer = t.layer ? t.layer.toUpperCase() + ' · ' : '';
    title.textContent = layer + (t.name.length > 22 ? t.name.slice(0, 21) + '…' : t.name);
    svg.appendChild(title);

    t.cols.forEach((c, i) => {
      const cy = t.y + headH + i * rowH;
      const isFocus = c.id === focus;
      const row = document.createElementNS(ns, 'rect');
      row.setAttribute('class', 'col-row' + (isFocus ? ' focus' : ''));
      row.setAttribute('x', t.x + 1);
      row.setAttribute('y', cy + 1);
      row.setAttribute('width', boxW - 2);
      row.setAttribute('height', rowH - 2);
      row.setAttribute('rx', 4);
      row.setAttribute('data-col', c.id);
      svg.appendChild(row);

      const label = document.createElementNS(ns, 'text');
      label.setAttribute('class', 'col-text' + (isFocus ? ' focus' : ''));
      label.setAttribute('x', t.x + 9);
      label.setAttribute('y', cy + 15);
      label.setAttribute('data-col', c.id);
      const name = c.column.length > 24 ? c.column.slice(0, 23) + '…' : c.column;
      label.textContent = name;
      if (c.external || !c.layer) label.setAttribute('opacity', '0.72');
      svg.appendChild(label);

      // 类型/标记放右侧，避免长列名被挤断
      const badge = c.kind === 'key' ? 'PK' : c.kind === 'partition' ? '分区' : (c.type || '');
      if (badge) {
        const b = document.createElementNS(ns, 'text');
        b.setAttribute('class', 'col-badge');
        b.setAttribute('x', t.x + boxW - 9);
        b.setAttribute('y', cy + 15);
        b.setAttribute('text-anchor', 'end');
        b.setAttribute('data-col', c.id);
        b.textContent = badge;
        svg.appendChild(b);
      }
    });
  });
}

function drawLineage(svg, g, highlight) {
  // 后端空图曾经返回 "edges":null，这里不能假设数组一定存在。
  const nodes = (g && g.nodes) || [];
  const edges = (g && g.edges) || [];
  const inDeg = {}, adj = {};
  nodes.forEach(n => { inDeg[n] = 0; adj[n] = []; });
  edges.forEach(e => { adj[e.source] = adj[e.source] || []; adj[e.source].push(e.target); inDeg[e.target] = (inDeg[e.target] || 0) + 1; });

  const level = {};
  const queue = nodes.filter(n => !inDeg[n]);
  queue.forEach(n => level[n] = 0);
  const q = queue.slice();
  while (q.length) {
    const cur = q.shift();
    (adj[cur] || []).forEach(nx => {
      level[nx] = Math.max(level[nx] || 0, (level[cur] || 0) + 1);
      if (--inDeg[nx] === 0) q.push(nx);
    });
  }
  nodes.forEach(n => { if (level[n] === undefined) level[n] = 0; });

  const groups = {};
  nodes.forEach(n => (groups[level[n]] = groups[level[n]] || []).push(n));
  if (!nodes.length) { svg.innerHTML = ''; return; }
  const maxL = Math.max.apply(null, Object.keys(groups).map(Number));
  const maxR = Math.max.apply(null, Object.values(groups).map(x => x.length));
  const w = 150, h = 34, gx = 60, gy = 18, pad = 24;
  const width = pad * 2 + (maxL + 1) * w + maxL * gx;
  const height = Math.max(200, pad * 2 + maxR * h + (maxR - 1) * gy);
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('height', height);
  svg.innerHTML = '';

  const pos = {};
  Object.keys(groups).forEach(l => {
    groups[l].forEach((n, i) => {
      const total = groups[l].length;
      pos[n] = {
        x: pad + Number(l) * (w + gx),
        y: pad + (height - pad * 2 - total * h - (total - 1) * gy) / 2 + i * (h + gy),
      };
    });
  });

  const ns = 'http://www.w3.org/2000/svg';
  edges.forEach(e => {
    const a = pos[e.source], b = pos[e.target];
    if (!a || !b) return;
    const path = document.createElementNS(ns, 'path');
    path.setAttribute('class', 'edge');
    const x1 = a.x + w, y1 = a.y + h / 2, x2 = b.x, y2 = b.y + h / 2, mx = (x1 + x2) / 2;
    path.setAttribute('d', `M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`);
    svg.appendChild(path);
  });

  nodes.forEach(n => {
    const p = pos[n];
    if (!p) return;
    const rect = document.createElementNS(ns, 'rect');
    rect.setAttribute('class', 'node-rect' + (highlight && n === highlight ? ' focus' : ''));
    rect.setAttribute('x', p.x); rect.setAttribute('y', p.y);
    rect.setAttribute('width', w); rect.setAttribute('height', h);
    rect.setAttribute('rx', 7);
    rect.setAttribute('data-node', n);
    svg.appendChild(rect);

    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', p.x + 11); t.setAttribute('y', p.y + 21);
    t.setAttribute('data-node', n);
    t.textContent = n.length > 20 ? n.slice(0, 19) + '…' : n;
    svg.appendChild(t);
  });
}


/* ---------------- 指标 ---------------- */

const METRIC_TPL = `name: gmv
displayName: 成交总额
model: dws_order_daily
agg: sum
expr: gmv
timeColumn: dt
dimensions: [status]
unit: 元
description: 按天汇总的成交金额
`;

function ymd(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
}

async function pageMetrics(view) {
  const metrics = await api('/metrics');
  if (!metrics.length) {
    view.innerHTML = `
      <div class="page-head">
        <div>
          <h2 class="page-title">指标</h2>
          <p class="page-desc">把业务口径固化成可复用对象，看板与 API 共用同一份定义。</p>
        </div>
        <button class="primary" data-act="new-metric">新建指标</button>
      </div>
      <div class="card"><div class="empty"><b>还没有指标</b>先发布 dws 层模型，再定义指标</div></div>`;
    return;
  }

  const cur = S.selected.metric || metrics[0].name;
  const m = metrics.find(x => x.name === cur) || metrics[0];
  const days = S.selected.metricDays || 30;
  const dim = S.selected.metricDim || '';
  const to = new Date();
  const from = new Date(to.getTime() - days * 86400000);
  const qs = `from=${ymd(from)}&to=${ymd(to)}&granularity=day` + (dim ? `&dimension=${encodeURIComponent(dim)}` : '');
  let series = null, qerr = '';
  try { series = await api('/metrics/' + encodeURIComponent(m.name) + '/query?' + qs); }
  catch (e) { qerr = e.message; }

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">指标</h2>
        <p class="page-desc">${esc(m.displayName || m.name)} · <span class="mono">${esc(m.agg)}(${esc(m.expr)})</span> on ${esc(m.model)}</p>
      </div>
      <button class="primary" data-act="new-metric">新建指标</button>
    </div>

    <div class="card">
      <div class="card-head" style="gap:10px;flex-wrap:wrap">
        <select id="m-pick" style="width:200px">
          ${metrics.map(x => `<option value="${esc(x.name)}" ${x.name === m.name ? 'selected' : ''}>${esc(x.displayName || x.name)}</option>`).join('')}
        </select>
        <select id="m-days" style="width:130px">
          ${[7, 30, 90, 365].map(d => `<option value="${d}" ${d === days ? 'selected' : ''}>最近 ${d} 天</option>`).join('')}
        </select>
        <select id="m-dim" style="width:170px">
          <option value="">不拆维度</option>
          ${(m.dimensions || []).map(d => `<option value="${esc(d)}" ${d === dim ? 'selected' : ''}>按 ${esc(d)} 拆分</option>`).join('')}
        </select>
        <div class="spacer"></div>
        <div class="hint">${m.unit ? '单位：' + esc(m.unit) : ''}</div>
      </div>
      ${qerr ? `<div class="err-box">${esc(qerr)}</div>` : `<svg class="dag" id="metric-chart" height="320"></svg>`}
      ${series && series.points.length ? `<div class="hint mono" style="margin-top:10px">${series.points.length} 个数据点 · ${esc(series.sql.split('\n')[0])} …</div>` : ''}
      ${series && !series.points.length && !qerr ? '<div class="hint" style="margin-top:10px">所选区间没有数据，换个时间范围或先跑一轮管道</div>' : ''}
    </div>

    <div class="card">
      <div class="card-head"><div class="card-title">全部指标</div></div>
      <div class="table-wrap"><table>
        <thead><tr><th>名称</th><th>口径</th><th>模型</th><th>维度</th><th>单位</th></tr></thead>
        <tbody>${metrics.map(x => `<tr>
          <td><b>${esc(x.displayName || x.name)}</b><div class="mute small mono">${esc(x.name)}</div></td>
          <td class="mono">${esc(x.agg)}(${esc(x.expr)})</td>
          <td class="mono dim">${esc(x.model)}</td>
          <td class="dim small">${esc((x.dimensions || []).join(', ') || '—')}</td>
          <td class="dim">${esc(x.unit || '')}</td>
        </tr>`).join('')}</tbody></table></div>
    </div>`;

  if (series && series.points.length) {
    drawLineChart(document.getElementById('metric-chart'), series.points, m.unit);
  }
  const pick = document.getElementById('m-pick');
  if (pick) pick.onchange = () => { S.selected.metric = pick.value; S.selected.metricDim = ''; render(); };
  const dd = document.getElementById('m-days');
  if (dd) dd.onchange = () => { S.selected.metricDays = Number(dd.value); render(); };
  const dm = document.getElementById('m-dim');
  if (dm) dm.onchange = () => { S.selected.metricDim = dm.value; render(); };
}

function drawLineChart(svg, points, unit) {
  if (!svg) return;
  const ns = 'http://www.w3.org/2000/svg';
  const W = 960, H = 320, padL = 64, padR = 18, padT = 18, padB = 46;

  const dims = {};
  points.forEach(p => (dims[p.dim || ''] = dims[p.dim || ''] || []).push(p));
  const keys = Object.keys(dims);
  const colors = ['#4f8cff', '#3fb950', '#d29922', '#f85149', '#a371f7', '#39c5cf'];

  let maxV = 0, minV = 0;
  points.forEach(p => { maxV = Math.max(maxV, p.value); minV = Math.min(minV, p.value); });
  if (maxV === minV) maxV = minV + 1;

  // 刻度对齐到 1/2/5×10^n：否则会出现 5.36、21 这种刻度，
  // 读图的人要在脑子里做除法才能估出中间值。
  const step = niceStep(maxV - minV, 5);
  maxV = Math.ceil(maxV / step) * step;
  minV = minV < 0 ? Math.floor(minV / step) * step : 0;
  if (maxV === minV) maxV = minV + step;

  const xs = {};
  const tsList = Array.from(new Set(points.map(p => p.ts))).sort();
  tsList.forEach((t, i) => { xs[t] = i; });

  const x = i => padL + (tsList.length <= 1 ? 0 : (W - padL - padR) * i / (tsList.length - 1));
  const y = v => padT + (H - padT - padB) * (1 - (v - minV) / (maxV - minV));

  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('height', H);
  svg.innerHTML = '';

  // 网格与 Y 轴刻度：按 step 步进，保证每条网格线都落在整数刻度上。
  const tickCount = Math.max(1, Math.round((maxV - minV) / step));
  for (let i = 0; i <= tickCount; i++) {
    const v = minV + step * i;
    const yy = y(v);
    const line = document.createElementNS(ns, 'line');
    line.setAttribute('x1', padL); line.setAttribute('x2', W - padR);
    line.setAttribute('y1', yy); line.setAttribute('y2', yy);
    line.setAttribute('stroke', '#1c232c'); line.setAttribute('stroke-width', '1');
    svg.appendChild(line);

    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', padL - 10); t.setAttribute('y', yy + 4);
    t.setAttribute('text-anchor', 'end');
    t.setAttribute('style', 'fill:#6b7887;font-size:10.5px');
    // 小量级保留两位：退款率这类 0~20 的指标，toFixed(0) 会把 0.4/0.8 全抹成 0 和 1。
    t.textContent = Math.abs(v) >= 10000 ? (v / 10000).toFixed(1) + '万'
                  : Math.abs(v) < 10 ? v.toFixed(2).replace(/\.00$/, '')
                  : v.toFixed(0);
    svg.appendChild(t);
  }

  // X 轴标签：首、中、尾
  [0, Math.floor(tsList.length / 2), tsList.length - 1].forEach((i, k) => {
    if (i < 0 || !tsList[i]) return;
    if (k === 1 && tsList.length < 3) return;
    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', x(i)); t.setAttribute('y', H - 28);
    t.setAttribute('text-anchor', k === 0 ? 'start' : (k === 2 ? 'end' : 'middle'));
    t.setAttribute('style', 'fill:#6b7887;font-size:10.5px');
    t.textContent = tsList[i];
    svg.appendChild(t);
  });

  keys.forEach((k, idx) => {
    const arr = dims[k].slice().sort((a, b) => a.ts < b.ts ? -1 : 1);
    const color = colors[idx % colors.length];
    let d = '';
    arr.forEach((p, i) => {
      const px = x(xs[p.ts]), py = y(p.value);
      d += (i === 0 ? 'M ' : ' L ') + px + ' ' + py;
    });

    // 只在单系列时填充面积。多系列各填一层会叠成一片灰，线也被盖住，
    // 而多系列本来就是要比较彼此走势的。
    if (arr.length > 1 && keys.length === 1) {
      const area = document.createElementNS(ns, 'path');
      area.setAttribute('d', d + ` L ${x(xs[arr[arr.length - 1].ts])} ${H - padB} L ${x(xs[arr[0].ts])} ${H - padB} Z`);
      area.setAttribute('fill', color); area.setAttribute('opacity', '0.08');
      svg.appendChild(area);
    }

    const path = document.createElementNS(ns, 'path');
    path.setAttribute('d', d);
    path.setAttribute('fill', 'none');
    path.setAttribute('stroke', color);
    path.setAttribute('stroke-width', '1.8');
    path.setAttribute('stroke-linejoin', 'round');
    svg.appendChild(path);

    if (arr.length === 1) {
      const dot = document.createElementNS(ns, 'circle');
      dot.setAttribute('cx', x(xs[arr[0].ts])); dot.setAttribute('cy', y(arr[0].value));
      dot.setAttribute('r', '3'); dot.setAttribute('fill', color);
      const title = document.createElementNS(ns, 'title');
      title.textContent = `${arr[0].ts}${k ? ' · ' + k : ''}: ${arr[0].value}${unit || ''}`;
      dot.appendChild(title);
      svg.appendChild(dot);
    }
  });

  // 图例：按文字宽度累加位置。固定 110px 间距在标签长短不一（中文/英文混排）
  // 时会重叠，而系列名恰恰是判断「哪条线是谁」的唯一线索。
  if (keys.length > 1 || keys[0]) {
    let gx = padL;
    keys.forEach((k, idx) => {
      const rect = document.createElementNS(ns, 'rect');
      rect.setAttribute('x', gx); rect.setAttribute('y', H - 14);
      rect.setAttribute('width', 8); rect.setAttribute('height', 8);
      rect.setAttribute('rx', 2);
      rect.setAttribute('fill', colors[idx % colors.length]);
      svg.appendChild(rect);
      const t = document.createElementNS(ns, 'text');
      t.setAttribute('x', gx + 13); t.setAttribute('y', H - 6);
      t.setAttribute('style', 'fill:#9aa7b4;font-size:10.5px');
      // 空维度是数据问题（status 为空串），不能显示成「全部」——
      // 那会让人以为是合计，把脏数据当成正常汇总。
      const label = k || '(空值)';
      t.textContent = label;
      svg.appendChild(t);
      gx += 13 + label.length * 10 + 16;
    });
  }
}

function metricEditor() {
  modal('指标定义', `
    <div class="stack">
      <textarea id="metric-yaml" rows="12">${esc(METRIC_TPL)}</textarea>
      <div class="hint">expr 只能是被聚合的列名；维度必须已在 dimensions 中声明。</div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-metric">保存</button>
      </div>
    </div>`);
}


/* ---------------- 看板 ---------------- */

const DASH_TPL = `name: ops_overview
description: 经营总览
cards:
  - title: 成交总额
    metric: gmv
    days: 30
  - title: 订单量
    metric: order_cnt
    days: 30
`;

async function pageDashboards(view) {
  const list = await api('/dashboards');
  if (!list.length) {
    view.innerHTML = `
      <div class="page-head">
        <div>
          <h2 class="page-title">看板</h2>
          <p class="page-desc">把指标组合成一页。卡片只引用指标，口径仍在语义层统一定义。</p>
        </div>
        <button class="primary" data-act="new-dashboard">新建看板</button>
      </div>
      <div class="card"><div class="empty"><b>还没有看板</b>先定义指标，再组合成看板</div></div>`;
    return;
  }

  const cur = S.selected.dashboard || list[0].name;
  let data = null, err = '';
  try { data = await api('/dashboards/' + encodeURIComponent(cur)); }
  catch (e) { err = e.message; }

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">看板</h2>
        <p class="page-desc">${data ? esc(data.description || '') : ''}</p>
      </div>
      <div class="row">
        <select id="d-pick" style="width:200px">
          ${list.map(d => `<option value="${esc(d.name)}" ${d.name === cur ? 'selected' : ''}>${esc(d.name)}</option>`).join('')}
        </select>
        <button class="primary" data-act="new-dashboard">新建看板</button>
      </div>
    </div>
    ${err ? `<div class="card"><div class="err-box">${esc(err)}</div></div>` : ''}
    <div class="grid c2" id="dash-grid">
      ${data ? data.cards.map((c, i) => `
        <div class="card" style="${c.span === 2 ? 'grid-column: 1 / -1;' : ''}">
          <div class="card-head">
            <div class="card-title">${esc(c.title || c.metric)} ${c.unit ? `<span class="mute small">${esc(c.unit)}</span>` : ''}</div>
            <div class="hint mono">${esc(c.metric)} · ${c.days}天${c.dimension ? ' · ' + esc(c.dimension) : ''}</div>
          </div>
          ${c.error ? `<div class="err-box">${esc(c.error)}</div>` : `<svg class="dag" id="dc-${i}" height="220"></svg>`}
        </div>`).join('') : ''}
    </div>`;

  if (data) {
    data.cards.forEach((c, i) => {
      if (c.error || !c.points || !c.points.length) return;
      drawLineChart(document.getElementById('dc-' + i), c.points, c.unit);
    });
  }
  const pick = document.getElementById('d-pick');
  if (pick) pick.onchange = () => { S.selected.dashboard = pick.value; render(); };
}

function dashboardEditor() {
  modal('看板定义', `
    <div class="stack">
      <textarea id="dash-yaml" rows="14">${esc(DASH_TPL)}</textarea>
      <div class="hint">cards 里每一项引用一个已存在的指标；span=2 表示独占一行。</div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-dashboard">保存</button>
      </div>
    </div>`);
}

/* ---------------- 质量 / 审计 ---------------- */

async function pageQuality(view) {
  const [overview, trend] = await Promise.all([api('/quality/overview'), api('/quality/trend?days=14')]);
  const totalRules = overview.reduce((a, r) => a + r.total, 0);
  const totalPassed = overview.reduce((a, r) => a + r.passed, 0);
  const rate = totalRules ? (totalPassed / totalRules * 100).toFixed(1) : '—';
  const failing = overview.filter(r => r.failed > 0);

  const insts = await api('/instances?limit=30');
  const done = insts.filter(i => i.status === 'success' || i.status === 'failed');
  if (!done.length) {
    view.innerHTML = `<div class="card"><div class="empty"><b>暂无质量数据</b>在管道中加入 quality 节点后，每轮运行都会留下校验结果</div></div>`;
    return;
  }
  const id = S.selected.qualityInstance || done[0].id;
  const rows = await api('/quality?instanceId=' + encodeURIComponent(id));
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">质量</h2>
        <p class="page-desc">severity=error 的规则未通过会阻断下游，warn 只记录。</p>
      </div>
      <select id="q-inst" style="width:300px">
        ${done.map(i => `<option value="${esc(i.id)}" ${i.id === id ? 'selected' : ''}>${esc(i.id.slice(-10))} · ${esc(i.bizDate)} · ${esc(i.status)}</option>`).join('')}
      </select>
    </div>

    <div class="grid c3">
      <div class="stat"><div class="stat-label">总通过率</div><div class="stat-value">${rate}${rate === '—' ? '' : '%'}</div><div class="stat-sub">${totalPassed}/${totalRules} 次校验</div></div>
      <div class="stat"><div class="stat-label">规则数</div><div class="stat-value">${overview.length}</div><div class="stat-sub">${failing.length} 条曾失败</div></div>
      <div class="stat"><div class="stat-label">近 14 天</div><div class="stat-value">${trend.length ? (trend[trend.length - 1].passRate * 100).toFixed(0) + '%' : '—'}</div><div class="stat-sub">最近一次 ${trend.length ? trend[trend.length - 1].date : '—'}</div></div>
    </div>

    ${trend.length ? `<div class="card"><div class="card-head"><div class="card-title">通过率趋势</div></div>
      <svg class="dag" id="q-trend" height="180"></svg></div>` : ''}

    <div class="card">
      <div class="card-head">
        <div class="card-title">按规则汇总</div>
        <button class="sm ghost" data-act="profile-table">数据画像与推荐规则</button>
      </div>
      ${overview.length ? `<div class="table-wrap"><table>
        <thead><tr><th>规则</th><th>通过率</th><th>总次数</th><th>失败</th><th>最近</th></tr></thead>
        <tbody>${overview.map(r => `<tr>
          <td class="mono">${esc(r.rule)}</td>
          <td><span class="pill ${r.passRate === 1 ? 'ok' : r.passRate > 0.9 ? 'warn' : 'err'}">${(r.passRate * 100).toFixed(1)}%</span></td>
          <td class="mono dim">${r.total}</td>
          <td class="mono ${r.failed ? '' : 'dim'}">${r.failed}</td>
          <td class="dim small">${fmtTime(r.lastAt)}</td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty">还没有规则结果</div>`}
    </div>
    <div class="card">
      ${rows.length ? `<div class="table-wrap"><table>
        <thead><tr><th>规则</th><th>节点</th><th>结果</th><th>实测</th><th>期望</th><th>级别</th></tr></thead>
        <tbody>${rows.map(q => `<tr>
          <td class="mono">${esc(q.ruleName)}</td>
          <td class="mono dim">${esc(q.nodeId)}</td>
          <td>${q.passed ? '<span class="pill ok">通过</span>' : '<span class="pill err">未通过</span>'}</td>
          <td class="mono">${esc(q.observed)}</td>
          <td class="mono dim">${esc(q.expected)}</td>
          <td><span class="pill ${q.severity === 'error' ? 'err' : 'warn'}">${esc(q.severity)}</span></td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty">该实例没有质量结果</div>`}
    </div>`;
  if (trend.length) drawQualityTrend(document.getElementById('q-trend'), trend);
  const sel = document.getElementById('q-inst');
  if (sel) sel.onchange = () => { S.selected.qualityInstance = sel.value; render(); };
}

function drawQualityTrend(svg, trend) {
  if (!svg || !trend.length) return;
  const ns = 'http://www.w3.org/2000/svg';
  const W = 960, H = 180, padL = 48, padR = 16, padT = 14, padB = 26;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('height', H);
  svg.innerHTML = '';
  const n = trend.length;
  const x = i => padL + (W - padL - padR) * (n <= 1 ? 0 : i / (n - 1));
  const y = v => padT + (H - padT - padB) * (1 - v);
  for (let i = 0; i <= 4; i++) {
    const v = i / 4, yy = y(v);
    const line = document.createElementNS(ns, 'line');
    line.setAttribute('x1', padL); line.setAttribute('x2', W - padR);
    line.setAttribute('y1', yy); line.setAttribute('y2', yy);
    line.setAttribute('stroke', '#1c232c');
    svg.appendChild(line);
    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', padL - 8); t.setAttribute('y', yy + 4);
    t.setAttribute('text-anchor', 'end');
    t.setAttribute('style', 'fill:#6b7887;font-size:10px');
    t.textContent = (v * 100).toFixed(0) + '%';
    svg.appendChild(t);
  }
  let d = '';
  trend.forEach((p, i) => { d += (i ? ' L ' : 'M ') + x(i) + ' ' + y(p.passRate); });
  const path = document.createElementNS(ns, 'path');
  path.setAttribute('d', d);
  path.setAttribute('fill', 'none');
  path.setAttribute('stroke', '#3fb950');
  path.setAttribute('stroke-width', '1.8');
  svg.appendChild(path);
  trend.forEach((p, i) => {
    const dot = document.createElementNS(ns, 'circle');
    dot.setAttribute('cx', x(i)); dot.setAttribute('cy', y(p.passRate)); dot.setAttribute('r', '2.6');
    dot.setAttribute('fill', p.passRate === 1 ? '#3fb950' : '#d29922');
    const tip = document.createElementNS(ns, 'title');
    tip.textContent = `${p.date} 通过率 ${(p.passRate * 100).toFixed(1)}%（${p.passed}/${p.total}）`;
    dot.appendChild(tip);
    svg.appendChild(dot);
  });
  // 去重后再画刻度，否则两个点时中间与末尾会重叠
  const idxs = Array.from(new Set([0, Math.floor(n / 2), n - 1])).sort((a, b) => a - b);
  idxs.forEach((i, k) => {
    if (!trend[i]) return;
    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', x(i)); t.setAttribute('y', H - 8);
    t.setAttribute('text-anchor', k === 0 ? 'start' : k === idxs.length - 1 ? 'end' : 'middle');
    t.setAttribute('style', 'fill:#6b7887;font-size:10px');
    t.textContent = trend[i].date;
    svg.appendChild(t);
  });
}

function profileModal() {
  modal('数据画像与规则推荐', `
    <div class="stack">
      <div class="grid c3" style="gap:10px">
        <div class="field"><label>数据源</label><input id="pf-ds" value="pg_dw" class="mono" /></div>
        <div class="field"><label>Schema</label><input id="pf-schema" value="ods" class="mono" /></div>
        <div class="field"><label>表名</label><input id="pf-table" value="order" class="mono" /></div>
      </div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">关闭</button>
        <button class="primary" data-act="run-profile">开始画像</button>
      </div>
      <div id="pf-result"></div>
    </div>`);
}

async function runProfile() {
  const box = document.getElementById('pf-result');
  box.innerHTML = '<div class="empty">扫描中…</div>';
  const r = await api('/quality/profile', {
    method: 'POST',
    body: { datasource: val('pf-ds'), schema: val('pf-schema'), table: val('pf-table') },
  });
  box.innerHTML = `
    <div class="hint" style="margin-bottom:8px">共 ${r.rows} 行，${r.columns.length} 列</div>
    <div class="table-wrap" style="max-height:260px;overflow:auto"><table>
      <thead><tr><th>列</th><th>类型</th><th>空值率</th><th>去重数</th><th>范围</th></tr></thead>
      <tbody>${r.columns.map(c => `<tr>
        <td class="mono">${esc(c.column)}</td>
        <td class="mono dim small">${esc(c.type)}</td>
        <td class="mono">${((c.nullRate || 0) * 100).toFixed(1)}%</td>
        <td class="mono dim">${c.distinct}</td>
        <td class="mono small dim">${c.min !== undefined ? esc(c.min) + ' ~ ' + esc(c.max) : '—'}</td>
      </tr>`).join('')}</tbody></table></div>
    <div class="hint" style="margin:12px 0 6px">推荐的规则（${r.recommended.length}）</div>
    <div>${r.recommended.map(x => `<span class="pill info" style="margin:0 6px 6px 0">${esc(x.name)} · ${esc(x.kind)}</span>`).join('') || '<span class="dim">没有可推荐的规则</span>'}</div>`;
}

async function pageAudit(view) {
  const rows = await api('/audit?limit=200');
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">审计</h2>
        <p class="page-desc">所有写操作留痕：谁、在什么时间、对哪个资源做了什么。</p>
      </div>
    </div>
    <div class="card">
      ${rows.length ? `<div class="table-wrap"><table>
        <thead><tr><th>时间</th><th>操作人</th><th>动作</th><th>资源</th><th>请求 ID</th></tr></thead>
        <tbody>${rows.map(a => `<tr>
          <td class="dim small">${fmtTime(a.createdAt)}</td>
          <td class="mono">${esc(a.actor)}</td>
          <td><span class="pill info">${esc(a.action)}</span></td>
          <td class="mono">${esc(a.resource)}</td>
          <td class="mono mute small">${esc((a.requestId || '').slice(-12))}</td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty">暂无记录</div>`}
    </div>`;
}

/* ---------------- 事件 ---------------- */

document.addEventListener('click', async (ev) => {
  const nav = ev.target.closest('.nav-item');
  if (nav) { go(nav.dataset.route); return; }

  const el = ev.target.closest('[data-act]');
  if (!el) return;
  const act = el.dataset.act;

  try {
    switch (act) {
      case 'reload': await render(); break;
      case 'go': go(el.dataset.route); break;
      case 'close-modal': closeModal(); break;
      // 遮罩空白处关闭：目标不在 .modal 内部才算「点了空白」。
      case 'mask-close': if (!ev.target.closest('.modal')) closeModal(); break;

      case 'run-once': {
        el.disabled = true;
        const r = await api('/worker/run-once', { method: 'POST' });
        toast(r.ran ? '已执行一个实例' : '当前没有排队实例', r.ran ? 'ok' : '');
        await render();
        el.disabled = false;
        break;
      }

      case 'new-ds': await newDatasourceModal(); break;
      case 'save-ds': {
        const body = {
          name: document.getElementById('ds-name').value.trim(),
          type: document.getElementById('ds-type').value,
          config: {
            host: document.getElementById('ds-host').value.trim(),
            port: Number(document.getElementById('ds-port').value || 0),
            database: document.getElementById('ds-db').value.trim(),
            user: document.getElementById('ds-user').value.trim(),
          },
          secret: { password: document.getElementById('ds-pass').value },
        };
        if (!body.name) { toast('请填写名称', 'err'); break; }
        el.disabled = true; el.textContent = '连接中…';
        await api('/datasources', { method: 'POST', body });
        closeModal(); toast('数据源已接入', 'ok'); await render();
        break;
      }
      case 'test-ds': {
        el.disabled = true;
        await api('/datasources/' + encodeURIComponent(el.dataset.name) + '/test', { method: 'POST' });
        toast('连接正常', 'ok'); await render();
        break;
      }
      case 'collect-ds': {
        el.disabled = true; el.textContent = '采集中…';
        const r = await api('/datasources/' + encodeURIComponent(el.dataset.name) + '/collect', { method: 'POST' });
        toast(`已采集 ${r.tables} 张表`, 'ok'); await render();
        break;
      }
      case 'del-ds': {
        if (!confirm('删除数据源会同时删除已采集的表结构与相关模型绑定，确认？')) break;
        await api('/datasources/' + encodeURIComponent(el.dataset.name), { method: 'DELETE' });
        toast('已删除', 'ok'); await render();
        break;
      }

      case 'open-table': openTable(el.dataset.id); break;

      case 'do-login': {
        el.disabled = true;
        const r = await api('/auth/login', { method: 'POST', body: { email: val('lg-email'), password: val('lg-pass') } });
        setToken(r.token);
        closeModal(); toast('已登录', 'ok');
        await bootstrapSession(); await render();
        break;
      }
      case 'switch-ws': {
        const r = await api('/workspaces/' + el.dataset.id + '/switch', { method: 'POST' });
        setToken(r.token); closeModal();
        toast('已切换到 ' + r.workspace.name, 'ok');
        // 切租户等于换了数据视图，清掉本地选择避免看到旧 ID
        S.selected = {}; S.cache = {};
        await bootstrapSession(); await render();
        break;
      }
      case 'create-ws': {
        const name = val('ws-new');
        if (!name) { toast('请输入名称', 'err'); break; }
        await api('/workspaces', { method: 'POST', body: { name } });
        toast('已创建 ' + name, 'ok'); closeModal(); await bootstrapSession();
        break;
      }
      case 'profile-table': profileModal(); break;
      case 'run-profile': el.disabled = true; await runProfile(); el.disabled = false; break;
      case 'new-metric': metricEditor(); break;
      case 'new-dashboard': dashboardEditor(); break;
      case 'save-dashboard': {
        const yaml = document.getElementById('dash-yaml').value;
        el.disabled = true;
        const d = await api('/dashboards', { method: 'POST', body: { yaml } });
        closeModal(); S.selected.dashboard = d.name;
        toast('看板 ' + d.name + ' 已保存', 'ok');
        await render();
        break;
      }
      case 'save-metric': {
        const yaml = document.getElementById('metric-yaml').value;
        el.disabled = true;
        const m = await api('/metrics', { method: 'POST', body: { yaml } });
        closeModal(); S.selected.metric = m.name;
        toast('指标 ' + m.name + ' 已保存', 'ok');
        await render();
        break;
      }

      case 'sel-model': S.selected.model = el.dataset.name; await render(); break;
      case 'new-model': modelEditor(''); break;
      case 'edit-model': {
        const m = (S.cache.models || []).find(x => x.name === el.dataset.name);
        modelEditor(m ? modelYamlFrom(m) : '');
        break;
      }
      case 'preview-ddl': {
        const yaml = document.getElementById('model-yaml').value;
        const r = await api('/models/preview', { method: 'POST', body: { yaml } });
        document.getElementById('ddl-preview').innerHTML = `<div class="card-title" style="margin:12px 0 6px">DDL 预览</div><pre class="code">${esc(r.ddl)}</pre>`;
        break;
      }
      case 'save-model': {
        const yaml = document.getElementById('model-yaml').value;
        const apply = document.getElementById('model-apply').checked;
        el.disabled = true; el.textContent = '发布中…';
        const m = await api('/models', { method: 'POST', body: { yaml, publish: true, apply } });
        closeModal(); S.selected.model = m.name;
        toast(`模型 ${m.name} 已发布 v${m.activeVersion}`, 'ok');
        await render();
        break;
      }
      case 'rollback-model': {
        await api('/models/' + encodeURIComponent(el.dataset.name) + '/rollback', {
          method: 'POST', body: { version: Number(el.dataset.version) },
        });
        toast('已回滚到 v' + el.dataset.version, 'ok'); await render();
        break;
      }

      case 'sel-pipeline': S.selected.pipeline = el.dataset.name; await render(); break;
      case 'new-pipeline': pipelineEditor(''); break;
      case 'edit-pipeline': {
        const p = (S.cache.pipelines || []).find(x => x.name === el.dataset.name);
        pipelineEditor(p && p.spec ? pipelineYamlFrom(p.spec) : '');
        break;
      }
      case 'save-pipeline': {
        const yaml = document.getElementById('pipe-yaml').value;
        el.disabled = true; el.textContent = '发布中…';
        const p = await api('/pipelines', { method: 'POST', body: { yaml, publish: true } });
        closeModal(); S.selected.pipeline = p.name;
        toast(`管道 ${p.name} 已发布 v${p.activeVersion}`, 'ok');
        await render();
        break;
      }
      case 'trigger-pl': {
        el.disabled = true;
        const r = await api('/pipelines/' + encodeURIComponent(el.dataset.name) + '/trigger', {
          method: 'POST', body: {},
        });
        toast(r.created ? '已创建实例 ' + r.instance.id.slice(-8) : '实例已存在，复用 ' + r.instance.id.slice(-8), 'ok');
        go('instances');
        break;
      }
      case 'toggle-pl': {
        const enabled = el.dataset.enabled === 'true';
        await api('/pipelines/' + encodeURIComponent(el.dataset.name) + '/enabled', {
          method: 'PUT', body: { enabled: !enabled },
        });
        toast(enabled ? '已停用' : '已启用', 'ok'); await render();
        break;
      }

      case 'open-instance': await openInstance(el.dataset.id); break;
      case 'retry-inst': {
        el.disabled = true;
        await api('/instances/' + encodeURIComponent(el.dataset.id) + '/retry', { method: 'POST' });
        closeModal(); toast('已创建重跑实例', 'ok'); await render();
        break;
      }
    }
  } catch (err) {
    toast(err.message, 'err');
    el.disabled = false;
    if (el.tagName === 'BUTTON') el.textContent = el.textContent.replace('中…', '');
  }
});

function pipelineYamlFrom(s) {
  const lines = [`kind: Pipeline`, `name: ${s.name}`];
  if (s.description) lines.push(`description: ${s.description}`);
  if (s.schedule) lines.push(`schedule: "${s.schedule}"`);
  lines.push('nodes:');
  (s.nodes || []).forEach(n => {
    lines.push(`  - id: ${n.id}`);
    lines.push(`    type: ${n.type}`);
    if (n.dependsOn && n.dependsOn.length) lines.push(`    depends_on: [${n.dependsOn.join(', ')}]`);
    if (n.datasource) lines.push(`    datasource: ${n.datasource}`);
    if (n.model) lines.push(`    model: ${n.model}`);
    if (n.writeMode) lines.push(`    write_mode: ${n.writeMode}`);
    if (n.sql) {
      lines.push(`    sql: |`);
      n.sql.trim().split('\n').forEach(l => lines.push('      ' + l));
    }
  });
  return lines.join('\n') + '\n';
}

window.addEventListener('hashchange', render);
async function bootstrapSession() {
  try {
    S.me = await api('/me');
    const wss = await api('/workspaces');
    const cur = wss.find(w => w.id === S.me.workspaceId);
    const badge = document.getElementById('env-badge');
    badge.innerHTML = `workspace: ${esc(cur ? cur.name : (S.me.workspaceId || 'default'))}`;
    badge.style.cursor = 'pointer';
    badge.title = '点击切换工作区';
    badge.onclick = () => workspaceSwitcher(wss, S.me.workspaceId);
    document.getElementById('version-badge').textContent = 'role: ' + (S.me.role || '—');
  } catch (e) {
    document.getElementById('version-badge').innerHTML =
      `<a href="#" onclick="loginModal();return false" style="color:#4f8cff">登录</a>`;
  }
}

function loginModal() {
  modal('登录', `
    <div class="stack">
      <div class="field"><label>邮箱</label><input id="lg-email" placeholder="admin@godw.local" /></div>
      <div class="field"><label>密码</label><input id="lg-pass" type="password" /></div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="do-login">登录</button>
      </div>
    </div>`);
}

async function workspaceSwitcher(list, cur) {
  modal('切换工作区', `
    <div class="stack">
      ${list.map(w => `<div class="list-item ${w.id === cur ? 'active' : ''}" data-act="switch-ws" data-id="${esc(w.id)}">
        <div class="name">${esc(w.name)}</div>
        <div class="meta">${esc(w.description || w.id)}</div>
      </div>`).join('') || '<div class="empty">没有可见工作区</div>'}
      <div class="row" style="justify-content:space-between;margin-top:8px">
        <input id="ws-new" placeholder="新工作区名称" style="max-width:220px" />
        <button class="ghost" data-act="create-ws">新建</button>
      </div>
    </div>`);
}

window.addEventListener('DOMContentLoaded', async () => {
  await bootstrapSession();
  await render();
});

/**
 * niceStep 把原始步长收敛到 1/2/5×10^n。
 * 例：range=21、5 条刻度 → 4.2 → 5；range=74113 → 14822 → 20000。
 */
function niceStep(range, ticks) {
  if (!(range > 0)) return 1;
  const raw = range / Math.max(1, ticks);
  const exp = Math.floor(Math.log10(raw));
  const base = Math.pow(10, exp);
  const n = raw / base;
  const mult = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return mult * base;
}
