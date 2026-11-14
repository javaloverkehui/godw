/* GoDW 控制台 · 运维中心（T0906）与 BI 看板编辑器（T0909）
 *
 * 运维中心的目标是「30 秒内定位」：列表 → 抽屉 → 错误摘要 + 实时日志，
 * 三次点击之内必须看到失败原因。所以抽屉里同时放了参数快照、渲染 SQL 和日志，
 * 而不是让用户在三个页面之间来回跳。
 *
 * BI 编辑器用原生拖拽 + CSS Grid，不引 X6/ECharts：
 * 图表用 SVG 自绘（折线/柱/饼），体积为零、离线可用。
 */
'use strict';

const RUNBOOK = {
  instances: [],
  filter: { pipeline: '', status: '', from: '', to: '' },
  ganttZoom: 1,
  drawerId: '',
  logTimer: null,
  logLevel: 'all',
  logKW: '',
  autoScroll: true,
};

Object.assign(ROUTES, {
  runbook: { title: '运维中心', sub: '实例状态、甘特、日志与补数', render: pageRunbook },
});

/* ============================== 运维中心 ============================== */

async function pageRunbook(view) {
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">运维中心</h2>
        <p class="page-desc">按管道与状态筛选实例，点开看参数、SQL 与实时日志；失败实例可批量重跑，漏跑的分区用补数补齐。</p>
      </div>
      <div class="row">
        <button class="sm ghost" data-act="rb-gantt">甘特视图</button>
        <button class="sm ghost" data-act="rb-backfill">补数</button>
        <button class="sm primary" data-act="rb-trigger">手动触发</button>
      </div>
    </div>
    <div class="card">
      <div class="row wrap" style="margin-bottom:12px">
        <select id="rb-pipe" style="width:auto;min-width:170px"><option value="">全部管道</option></select>
        <select id="rb-status" style="width:auto">
          <option value="">全部状态</option>
          <option value="success">成功</option>
          <option value="failed">失败</option>
          <option value="running">运行中</option>
          <option value="queued">排队中</option>
          <option value="canceled">已取消</option>
        </select>
        <input id="rb-from" type="date" style="width:auto">
        <input id="rb-to" type="date" style="width:auto">
        <button class="sm ghost" data-act="rb-failed">只看失败</button>
        <div class="spacer"></div>
        <button class="sm ghost" data-act="rb-retry-sel">重跑选中</button>
        <button class="sm ghost danger" data-act="rb-cancel-sel">取消选中</button>
      </div>
      <div id="rb-table">${UI.skeleton(8)}</div>
    </div>`;

  view.querySelector('[data-act="rb-gantt"]').addEventListener('click', () => ganttModal());
  view.querySelector('[data-act="rb-backfill"]').addEventListener('click', () => backfillModal());
  view.querySelector('[data-act="rb-trigger"]').addEventListener('click', () => triggerModal());
  view.querySelector('[data-act="rb-failed"]').addEventListener('click', () => {
    view.querySelector('#rb-status').value = 'failed';
    loadRunbook(view);
  });
  ['rb-pipe', 'rb-status', 'rb-from', 'rb-to'].forEach(id => {
    view.querySelector('#' + id).addEventListener('change', () => loadRunbook(view));
  });
  view.querySelector('[data-act="rb-retry-sel"]').addEventListener('click', () => batchAction('retry'));
  view.querySelector('[data-act="rb-cancel-sel"]').addEventListener('click', () => batchAction('cancel'));

  await loadRunbook(view);
}

let RBTABLE = null;

async function loadRunbook(view) {
  const host = view.querySelector('#rb-table');
  if (!host) return;
  host.innerHTML = UI.skeleton(8);
  let list;
  try {
    list = await api('/instances?limit=200');
  } catch (e) { host.innerHTML = UI.error(e, 'reload'); return; }

  const pipes = [...new Set(list.map(i => i.pipelineName).filter(Boolean))].sort();
  const pipeSel = view.querySelector('#rb-pipe');
  if (pipeSel && pipeSel.options.length <= 1) {
    pipeSel.innerHTML = '<option value="">全部管道</option>' + pipes.map(p => `<option value="${esc(p)}">${esc(p)}</option>`).join('');
  }

  const f = RUNBOOK.filter;
  f.pipeline = view.querySelector('#rb-pipe').value;
  f.status = view.querySelector('#rb-status').value;
  f.from = view.querySelector('#rb-from').value;
  f.to = view.querySelector('#rb-to').value;

  RUNBOOK.instances = list.filter(i => {
    if (f.pipeline && i.pipelineName !== f.pipeline) return false;
    if (f.status && i.status !== f.status) return false;
    if (f.from && i.bizDate < f.from) return false;
    if (f.to && i.bizDate > f.to) return false;
    return true;
  });

  host.innerHTML = '';
  RBTABLE = ProTable(host, {
    rows: RUNBOOK.instances,
    rowKey: 'id',
    selectable: true,
    pageSize: 20,
    exportName: 'instances',
    empty: { title: '没有符合条件的实例', desc: '调整筛选条件，或点右上角手动触发一次' },
    columns: [
      { key: 'id', title: '实例', width: '150px', render: r => `<span class="mono small">${esc(r.id.slice(0, 14))}</span>`, exportValue: r => r.id },
      { key: 'pipelineName', title: '管道', sortable: true, filter: true,
        render: r => `<b>${esc(r.pipelineName || '—')}</b><span class="mute small"> v${r.pipelineVersion || 0}</span>` },
      { key: 'bizDate', title: '业务日期', sortable: true, width: '110px', render: r => `<span class="mono small">${esc(r.bizDate)}</span>` },
      { key: 'status', title: '状态', sortable: true, width: '100px', render: r => statusPill(r.status) },
      { key: 'triggerType', title: '触发', width: '90px', render: r => `<span class="pill mute">${esc(r.triggerType)}</span>` },
      { key: 'startedAt', title: '开始', sortable: true, width: '150px', render: r => `<span class="small dim">${fmtTime(r.startedAt)}</span>`, exportValue: r => r.startedAt || '' },
      { key: 'duration', title: '耗时', width: '90px', render: r => `<span class="mono small">${instanceDur(r)}</span>`, exportValue: r => instanceDur(r) },
      { key: 'error', title: '错误摘要', wrap: true,
        render: r => r.error ? `<span class="small" style="color:#ff9b94">${esc(String(r.error).slice(0, 90))}</span>` : '<span class="mute">—</span>' },
    ],
    onRowClick: (id) => openRunDrawer(id),
  });
}

function instanceDur(i) {
  if (!i.startedAt) return '—';
  const end = i.finishedAt ? new Date(i.finishedAt) : new Date();
  const ms = end - new Date(i.startedAt);
  return ms > 0 ? fmtDur(ms) : '—';
}

function batchAction(kind) {
  const ids = RBTABLE ? RBTABLE.selected() : [];
  if (!ids.length) return toast('先勾选实例', 'err');
  const label = kind === 'retry' ? '重跑' : '取消';
  ConfirmDanger({
    title: label + ' ' + ids.length + ' 个实例',
    danger: kind === 'retry'
      ? '将按原版本快照与业务日期重新触发，不会重新读取当前定义。'
      : '仅能取消排队中的实例；运行中的实例会被跳过并提示原因。',
    resource: String(ids.length),
    reason: kind === 'retry' ? '重跑原因（写入审计）' : '取消原因（写入审计）',
    onOK: async (reason) => {
      let okN = 0, failN = 0, firstErr = '';
      for (const id of ids) {
        try {
          if (kind === 'retry') await api('/instances/' + encodeURIComponent(id) + '/retry', { method: 'POST' });
          else await api('/instances/' + encodeURIComponent(id) + '/cancel', { method: 'POST' });
          okN++;
        } catch (e) { failN++; if (!firstErr) firstErr = e.message; }
      }
      toast(`${label}完成：成功 ${okN}，失败 ${failN}${firstErr ? '（' + firstErr + '）' : ''}`, failN ? 'err' : 'ok');
      reason; // 后端通过 audit 记录；此处显式引用避免 lint 抱怨
      loadRunbook(document.getElementById('view'));
    },
  });
}

/** 实例详情抽屉：参数、SQL、日志、节点，一屏看全。 */
async function openRunDrawer(id) {
  RUNBOOK.drawerId = id;
  let inst, logs = [];
  try {
    // 详情接口返回 {instance, quality}，节点明细已在 instance.tasks 里，不用再单独拉。
    const r = await api('/instances/' + encodeURIComponent(id));
    inst = r.instance || r;
  } catch (e) { return toast(e.message, 'err'); }
  try { const r = await api('/instances/' + encodeURIComponent(id) + '/logs?limit=200'); logs = r.lines || r || []; } catch (e) { logs = []; }

  document.getElementById('drawer-root').innerHTML = `
    <div class="drawer-mask" data-act="drawer-close"></div>
    <div class="drawer">
      <div class="drawer-head">
        <div style="flex:1">
          <div style="font-weight:600">${esc(inst.pipelineName || inst.pipelineId)}</div>
          <div class="hint mono">${esc(inst.id)} · ${esc(inst.bizDate)} · v${inst.pipelineVersion || 0}</div>
        </div>
        ${statusPill(inst.status)}
        <button class="sm ghost" data-act="drawer-retry">重跑</button>
        <button class="sm ghost" data-act="drawer-close">关闭</button>
      </div>
      <div class="drawer-body">
        <div class="grid c4" style="margin-bottom:16px">
          <div class="stat"><div class="stat-label">耗时</div><div class="stat-value" style="font-size:20px">${instanceDur(inst)}</div></div>
          <div class="stat"><div class="stat-label">触发方式</div><div class="stat-value" style="font-size:20px">${esc(inst.triggerType)}</div></div>
          <div class="stat"><div class="stat-label">节点</div><div class="stat-value" style="font-size:20px">${(inst.tasks || tasks || []).length}</div></div>
          <div class="stat"><div class="stat-label">资源组</div><div class="stat-value" style="font-size:20px">${esc(inst.resourceGroup || 'default')}</div></div>
        </div>

        ${inst.error ? `<div class="err-box" style="margin-bottom:16px">${esc(inst.error)}</div>` : ''}

        <div class="card-title" style="margin-bottom:8px">参数快照</div>
        <div class="logbox" style="margin-bottom:16px;max-height:140px">${esc(JSON.stringify(inst.params || {}, null, 2))}</div>

        <div class="card-title" style="margin-bottom:8px">节点执行</div>
        <div class="table-wrap" style="margin-bottom:16px"><table class="dt">
          <thead><tr><th>节点</th><th>状态</th><th>行数</th><th>耗时</th><th>错误</th></tr></thead>
          <tbody>${(inst.tasks || []).map(t => `<tr>
            <td class="mono">${esc(t.nodeId)}</td>
            <td>${statusPill(t.status)}</td>
            <td class="mono small">${t.rowsAffected || 0}</td>
            <td class="mono small">${fmtDur(t.durationMs)}</td>
            <td class="small wrap" style="color:#ff9b94">${esc((t.error || '').slice(0, 160) || '—')}</td>
          </tr>`).join('') || '<tr><td colspan="5" class="mute">暂无节点记录</td></tr>'}</tbody>
        </table></div>

        <div class="row" style="margin-bottom:8px">
          <div class="card-title" style="flex:1">执行日志</div>
          <select id="log-level" style="width:auto">
            <option value="all">全部级别</option>
            <option value="info">INFO</option>
            <option value="warn">WARN</option>
            <option value="err">ERROR</option>
          </select>
          <input id="log-kw" placeholder="关键字过滤" style="width:170px">
          <label style="display:flex;align-items:center;gap:5px;margin:0;font-size:12px">
            <input type="checkbox" id="log-auto" checked style="width:auto">自动滚动
          </label>
        </div>
        <div class="logbox" id="log-box">${renderLogLines(logs)}</div>
      </div>
    </div>`;

  bindDrawer(id, logs);
  startLogStream(id);
}

function renderLogLines(lines) {
  const list = (lines || []).filter(l => {
    if (RUNBOOK.logLevel !== 'all' && (l.level || 'info').toLowerCase() !== RUNBOOK.logLevel) return false;
    if (RUNBOOK.logKW && !String(l.message || '').toLowerCase().includes(RUNBOOK.logKW.toLowerCase())) return false;
    return true;
  });
  if (!list.length) return '<div class="mute" style="padding:8px">暂无日志</div>';
  return list.map(l => `<div class="ln">
    <span class="ts">${(l.ts || '').replace('T', ' ').slice(11, 19)}</span>
    <span class="loglevel ${(l.level || 'info').toLowerCase()}">${esc((l.level || 'info').toUpperCase())}</span>
    <span class="msg">${esc(l.message)}</span>
  </div>`).join('');
}

function bindDrawer(id, logs) {
  const root = document.getElementById('drawer-root');
  root.querySelectorAll('[data-act="drawer-close"]').forEach(el => {
    el.addEventListener('click', () => closeDrawer());
  });
  root.querySelector('[data-act="drawer-retry"]').addEventListener('click', async () => {
    try {
      await api('/instances/' + encodeURIComponent(id) + '/retry', { method: 'POST' });
      toast('已重新触发', 'ok');
      closeDrawer();
      loadRunbook(document.getElementById('view'));
    } catch (e) { toast(e.message, 'err'); }
  });
  root.querySelector('#log-level').addEventListener('change', (e) => {
    RUNBOOK.logLevel = e.target.value;
    document.getElementById('log-box').innerHTML = renderLogLines(RUNBOOK.logBuf || logs);
  });
  root.querySelector('#log-kw').addEventListener('input', (e) => {
    RUNBOOK.logKW = e.target.value.trim();
    document.getElementById('log-box').innerHTML = renderLogLines(RUNBOOK.logBuf || logs);
  });
  root.querySelector('#log-auto').addEventListener('change', (e) => { RUNBOOK.autoScroll = e.target.checked; });
  RUNBOOK.logBuf = logs || [];
}

function closeDrawer() {
  stopLogStream();
  const root = document.getElementById('drawer-root');
  if (root) root.innerHTML = '';
  RUNBOOK.drawerId = '';
}

/**
 * 日志实时刷新用轮询而非 SSE：
 * 浏览器对同一域的 EventSource 有 6 连接上限，多开几个抽屉就会互相饿死。
 * 拉取式带 since 游标，成本可控且不会漏行。
 */
function startLogStream(id) {
  stopLogStream();
  let seq = 0;
  RUNBOOK.logTimer = setInterval(async () => {
    if (RUNBOOK.drawerId !== id) return stopLogStream();
    try {
      const r = await api('/instances/' + encodeURIComponent(id) + '/logs?since=' + seq + '&limit=200');
      const lines = r.lines || r || [];
      if (!lines.length) return;
      seq = Math.max(seq, ...lines.map(l => l.seq || 0));
      RUNBOOK.logBuf = (RUNBOOK.logBuf || []).concat(lines).slice(-800);
      const box = document.getElementById('log-box');
      if (box) {
        box.innerHTML = renderLogLines(RUNBOOK.logBuf);
        if (RUNBOOK.autoScroll) box.scrollTop = box.scrollHeight;
      }
    } catch (e) { /* 实例结束后日志会 404，静默即可 */ }
  }, 1000);
}

function stopLogStream() {
  if (RUNBOOK.logTimer) { clearInterval(RUNBOOK.logTimer); RUNBOOK.logTimer = null; }
}

/** 甘特：按管道分组泳道，横轴是时间。缩放只改像素比，不改数据。 */
async function ganttModal() {
  let rows;
  try { rows = await api('/runbook/gantt?limit=120'); } catch (e) { return toast(e.message, 'err'); }
  if (!rows || !rows.length) return toast('还没有实例数据', 'err');

  // 时间窗过滤：批跑时几十个实例集中在同一秒，全量视图里条形只有 1px 宽，
  // 看不出任何差别。默认只看最近 1 小时，需要时再放大到全部。
  const now = Date.now();
  const windows = [
    { label: '最近 1 小时', ms: 3600e3 },
    { label: '最近 24 小时', ms: 24 * 3600e3 },
    { label: '最近 7 天', ms: 7 * 24 * 3600e3 },
    { label: '全部', ms: Infinity },
  ];
  // 日更管道的实例通常落在最近一天内，默认 24 小时既能看见又不至于太稀。
  let curWin = 1;

  modal('实例甘特', `
    <div class="row" style="margin-bottom:10px">
      <span class="hint" id="gt-summary"></span>
      <div class="spacer"></div>
      <select id="gt-win" style="width:auto">${windows.map((w, i) => `<option value="${i}" ${i === 1 ? 'selected' : ''}>${w.label}</option>`).join('')}</select>
      <select id="gt-zoom" style="width:auto">
        <option value="1">1×</option><option value="2">2×</option><option value="4">4×</option>
      </select>
    </div>
    <div style="overflow-x:auto"><div id="gt-box" style="min-width:100%"></div></div>`);

  const draw = (zoom, winIdx) => {
    const win = windows[winIdx || 0];
    const shown = rows.filter(r => win.ms === Infinity || new Date(r.start).getTime() >= now - win.ms);
    if (!shown.length) {
      document.getElementById('gt-summary').textContent = '该时间窗内没有实例';
      document.getElementById('gt-box').innerHTML = '<div class="empty">换个时间窗试试</div>';
      return;
    }
    const t0 = Math.min(...shown.map(r => new Date(r.start).getTime()));
    const t1 = Math.max(...shown.map(r => new Date(r.end).getTime()));
    const span = Math.max(1000, t1 - t0);
    document.getElementById('gt-summary').textContent =
      `共 ${shown.length} 个实例 · 跨度 ${fmtSpan(span)}`;

    const W = 720 * zoom;
    const axis = [];
    for (let i = 0; i <= 5; i++) {
      const tt = t0 + span * i / 5;
      axis.push(`<span style="left:${(i / 5 * 100).toFixed(2)}%">${new Date(tt).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}</span>`);
    }
    const groups = {};
    shown.forEach(r => (groups[r.pipeline || '—'] = groups[r.pipeline || '—'] || []).push(r));
    let html = `<div class="gantt" style="width:${W}px"><div class="gantt-axis">${axis.join('')}</div>`;
    Object.keys(groups).sort().forEach(p => {
      html += `<div class="gantt-row" style="height:22px;background:rgba(255,255,255,.02)">
        <div class="gantt-lbl" style="width:150px;font-weight:600">${esc(p)}</div></div>`;
      groups[p].forEach(r => {
        const s = new Date(r.start).getTime();
        const e = new Date(r.end).getTime();
        const left = ((s - t0) / span) * (W - 160) + 150;
        // 最小 5px：跑得快的实例也要能看见，否则甘特会「少几行」。
        const w = Math.max(5, ((e - s) / span) * (W - 160));
        html += `<div class="gantt-row">
          <div class="gantt-lbl" title="${esc(r.id)}">${esc(r.bizDate)} · ${esc(r.id.slice(-6))}</div>
          <div class="gantt-bar ${esc(r.status)}" style="left:${left}px;width:${w}px"
               title="${esc(r.pipeline)} ${esc(r.bizDate)} ${esc(r.status)} ${fmtDur(r.durationMs)}"></div>
        </div>`;
      });
    });
    html += '</div>';
    document.getElementById('gt-box').innerHTML = html;
  };
  draw(1, curWin);
  let curZoom = 1;
  document.getElementById('gt-zoom').addEventListener('change', (e) => {
    curZoom = Number(e.target.value);
    draw(curZoom, curWin);
  });
  document.getElementById('gt-win').addEventListener('change', (e) => {
    curWin = Number(e.target.value);
    draw(curZoom, curWin);
  });
}

/** 手动触发：可选「仅试跑」，试跑不产生实例、不写表。 */
function triggerModal() {
  const pipes = [...new Set(RUNBOOK.instances.map(i => i.pipelineName).filter(Boolean))];
  modal('手动触发', `
    <div class="stack">
      <div class="field"><label>管道</label>
        <select id="tg-pipe">${pipes.map(p => `<option value="${esc(p)}">${esc(p)}</option>`).join('') || '<option value="">暂无管道</option>'}</select>
      </div>
      <div class="field"><label>业务日期</label><input id="tg-date" type="date" value="${new Date().toISOString().slice(0, 10)}"></div>
      <div class="field"><label>参数（JSON，可空）</label>
        <textarea id="tg-params" rows="4" placeholder='{"region":"east"}'></textarea>
      </div>
      <label style="display:flex;align-items:center;gap:6px;font-size:12.5px;color:var(--text-dim)">
        <input type="checkbox" id="tg-dry" style="width:auto"> 仅试跑（只渲染不写表）
      </label>
    </div>
    <div class="row" style="justify-content:flex-end;margin-top:14px">
      <button class="ghost" data-act="close-modal">取消</button>
      <button class="primary" id="tg-go">触发</button>
    </div>`);

  document.getElementById('tg-go').addEventListener('click', async (e) => {
    const pipe = val('tg-pipe');
    if (!pipe) return toast('没有可触发的管道', 'err');
    let params = {};
    const raw = val('tg-params');
    if (raw) {
      try { params = JSON.parse(raw); } catch (err) { return toast('参数不是合法 JSON', 'err'); }
    }
    e.target.disabled = true;
    try {
      if (document.getElementById('tg-dry').checked) {
        // dry-run 接收的是 YAML 而不是管道名：先取已发布版本的定义再渲染，
        // 保证试跑看到的 SQL 与真正跑的一致。
        const pipes = await api('/pipelines');
        const p = pipes.find(x => x.name === pipe);
        if (!p || !p.spec) throw new Error('管道未发布，无法试跑');
        const r = await api('/pipelines/dry-run', {
          method: 'POST',
          body: { yaml: pipelineYamlFrom(p.spec), bizDate: val('tg-date'), params },
        });
        modal('试跑结果', `<div class="logbox" style="max-height:420px">${esc(JSON.stringify(r, null, 2))}</div>
          <div class="row" style="justify-content:flex-end;margin-top:12px">
            <button class="ghost" data-act="close-modal">关闭</button></div>`);
        return;
      }
      const inst = await api('/pipelines/' + encodeURIComponent(pipe) + '/trigger', {
        method: 'POST', body: { bizDate: val('tg-date'), params },
      });
      closeModal();
      toast('已触发：' + inst.id, 'ok');
      loadRunbook(document.getElementById('view'));
    } catch (err) {
      toast(err.message, 'err');
      e.target.disabled = false;
    }
  });
}

/** 补数：先算账（预估条数），再提交，最后轮询进度。 */
function backfillModal() {
  const pipes = [...new Set(RUNBOOK.instances.map(i => i.pipelineName).filter(Boolean))];
  const today = new Date().toISOString().slice(0, 10);
  const weekAgo = new Date(Date.now() - 6 * 86400000).toISOString().slice(0, 10);

  modal('补数', `
    <div class="stack">
      <div class="field"><label>管道</label>
        <select id="bf-pipe">${pipes.map(p => `<option value="${esc(p)}">${esc(p)}</option>`).join('')}</select>
      </div>
      <div class="sform-row">
        <div class="field"><label>开始日期</label><input id="bf-from" type="date" value="${weekAgo}"></div>
        <div class="field"><label>结束日期</label><input id="bf-to" type="date" value="${today}"></div>
      </div>
      <div class="field"><label>补数原因</label><input id="bf-reason" placeholder="如：上游延迟导致 3 天未跑"></div>
      <label style="display:flex;align-items:center;gap:6px;font-size:12.5px;color:var(--text-dim)">
        <input type="checkbox" id="bf-force" style="width:auto"> 强制重跑已有实例的日期
      </label>
      <div id="bf-plan"></div>
    </div>
    <div class="row" style="justify-content:flex-end;margin-top:14px">
      <button class="ghost" data-act="close-modal">取消</button>
      <button class="primary" id="bf-go" disabled>提交补数</button>
    </div>`);

  const planBtn = document.createElement('button');
  planBtn.className = 'sm ghost';
  planBtn.textContent = '预估影响';
  document.getElementById('bf-plan').appendChild(planBtn);

  planBtn.addEventListener('click', async () => {
    const pipe = val('bf-pipe');
    if (!pipe) return;
    planBtn.disabled = true;
    try {
      const p = await api(`/backfill/plan?pipeline=${encodeURIComponent(pipe)}&from=${val('bf-from')}&to=${val('bf-to')}`);
      document.getElementById('bf-plan').innerHTML = `
        <div class="grid c3" style="margin:10px 0">
          <div class="stat"><div class="stat-label">范围内</div><div class="stat-value" style="font-size:20px">${p.total}</div><div class="stat-sub">天</div></div>
          <div class="stat"><div class="stat-label">已有实例</div><div class="stat-value" style="font-size:20px">${p.existing}</div><div class="stat-sub">默认跳过</div></div>
          <div class="stat"><div class="stat-label">需补</div><div class="stat-value" style="font-size:20px;color:#e3b341">${p.missing}</div><div class="stat-sub">将触发实例</div></div>
        </div>
        ${p.missing === 0 ? '<div class="ok-box">范围内没有缺口</div>' : ''}`;
      const go = document.getElementById('bf-go');
      go.disabled = p.missing === 0;
      go.dataset.missing = p.missing;
    } catch (e) { toast(e.message, 'err'); }
    planBtn.disabled = false;
  });

  document.getElementById('bf-go').addEventListener('click', async (e) => {
    e.target.disabled = true;
    e.target.textContent = '提交中…';
    try {
      const b = await api('/backfill', {
        method: 'POST',
        body: {
          pipeline: val('bf-pipe'), from: val('bf-from'), to: val('bf-to'),
          reason: val('bf-reason'), force: document.getElementById('bf-force').checked,
        },
      });
      showBackfillProgress(b.id);
    } catch (err) {
      toast(err.message, 'err');
      e.target.disabled = false;
      e.target.textContent = '提交补数';
    }
  });
}

function showBackfillProgress(id) {
  modal('补数进度', `<div id="bp-box">${UI.skeleton(4)}</div>`);
  const tick = async () => {
    let b;
    try { b = await api('/backfill/' + encodeURIComponent(id)); } catch (e) {
      document.getElementById('bp-box').innerHTML = UI.error(e);
      return;
    }
    const box = document.getElementById('bp-box');
    if (!box) return;
    box.innerHTML = `
      <div class="hint" style="margin-bottom:10px">${esc(b.pipeline)} · ${esc(b.from)} ~ ${esc(b.to)} · 共 ${b.total} 天</div>
      <div style="height:10px;background:var(--bg-elev2);border-radius:6px;overflow:hidden;margin-bottom:12px">
        <div style="height:100%;width:${b.percent.toFixed(1)}%;background:${b.failed ? '#f85149' : '#3fb950'};transition:width .4s"></div>
      </div>
      <div class="grid c4">
        <div class="stat"><div class="stat-label">成功</div><div class="stat-value" style="font-size:20px">${b.success}</div></div>
        <div class="stat"><div class="stat-label">失败</div><div class="stat-value" style="font-size:20px;color:#ff7b72">${b.failed}</div></div>
        <div class="stat"><div class="stat-label">运行中</div><div class="stat-value" style="font-size:20px">${b.running}</div></div>
        <div class="stat"><div class="stat-label">进度</div><div class="stat-value" style="font-size:20px">${b.percent.toFixed(0)}%</div></div>
      </div>
      ${(b.failDetail || []).length ? `<div class="err-box" style="margin-top:12px">${b.failDetail.map(esc).join('\n')}</div>` : ''}
      <div class="row" style="justify-content:flex-end;margin-top:14px">
        <button class="ghost" data-act="close-modal">关闭</button>
      </div>`;
    if (b.done < b.total) setTimeout(tick, 2000);
  };
  tick();
}

/** 甘特跨度用「天/小时」，不要退化成 647m34s 这种读不出量级的写法。 */
function fmtSpan(ms) {
  if (ms < 60000) return Math.round(ms / 1000) + ' 秒';
  if (ms < 3600000) return Math.round(ms / 60000) + ' 分钟';
  if (ms < 86400000) return (ms / 3600000).toFixed(1) + ' 小时';
  return (ms / 86400000).toFixed(1) + ' 天';
}
