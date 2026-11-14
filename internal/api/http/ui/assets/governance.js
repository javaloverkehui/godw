/* GoDW 控制台 · 治理与运维扩展页：列/行策略、成员、分享、订阅、导出、缓存。 */
'use strict';

Object.assign(ROUTES, {
  governance: { title: '治理', sub: '脱敏、行级权限、成员与共享', render: pageGovernance },
  ops: { title: '运维', sub: '导出、订阅、缓存与容量', render: pageOps },
});

const GOV_TABS = [
  { key: 'columns', label: '列策略' },
  { key: 'rows', label: '行级权限' },
  { key: 'simulate', label: '策略命中' },
  { key: 'approvals', label: '导出审批' },
  { key: 'members', label: '成员' },
  { key: 'shares', label: '分享' },
];

async function pageGovernance(view) {
  const tab = S.selected.govTab || 'columns';
  let body = '';
  if (tab === 'columns') body = await govColumns();
  else if (tab === 'rows') body = await govRows();
  else if (tab === 'simulate') body = await govSimulate();
  else if (tab === 'approvals') body = await govApprovals();
  else if (tab === 'members') body = await govMembers();
  else body = await govShares();

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">治理</h2>
        <p class="page-desc">策略在服务端生效：查询预览、指标、导出走同一条路径，不依赖前端自觉。</p>
      </div>
    </div>
    <div class="tabs">
      ${GOV_TABS.map(t => `<div class="tab ${t.key === tab ? 'active' : ''}" data-act="gov-tab" data-tab="${t.key}">${t.label}</div>`).join('')}
    </div>
    ${body}`;
}

// 命中测试：把「配了策略」和「策略真的生效」这两件事对上。
// 表单默认填当前登录身份，因为最常见的问题是「我自己怎么看得到」。
async function govSimulate() {
  const models = await api('/models').catch(() => []);
  const me = S.me || {};
  const role = me.role || 'viewer';
  const first = models[0] ? models[0].name : '';
  return `
    <div class="card">
      <div class="card-head">
        <div class="card-title">策略命中测试</div>
        <button class="sm primary" data-act="run-simulate">运行测试</button>
      </div>
      <div class="sim-form">
        <div class="field"><label>表 / 模型</label>
          <input id="sim-table" list="sim-models" value="${esc(first)}" placeholder="dwd_order 或 dwd.dwd_order" />
          <datalist id="sim-models">${models.map(m => `<option value="${esc(m.name)}"></option>`).join('')}</datalist>
        </div>
        <div class="field"><label>用户</label><input id="sim-user" value="${esc(me.userId || me.email || '')}" placeholder="用来替换 \${user}" /></div>
        <div class="field"><label>角色</label>
          <select id="sim-role">
            ${['viewer', 'editor', 'admin'].map(r => `<option value="${r}" ${r === role ? 'selected' : ''}>${r}</option>`).join('')}
          </select>
        </div>
      </div>
      <div class="hint" style="margin-top:8px">返回的是「服务端实际会套上什么」：列级脱敏按输出列名生效，行级过滤按角色（含通配 *）注入。</div>
      <div id="sim-result" style="margin-top:14px"></div>
    </div>`;
}

function renderSimulation(res) {
  const lvl = l => l === 'high' ? 'err' : l === 'medium' ? 'warn' : 'mute';
  return `
    <div class="doc-meta" style="margin-top:0">
      <div><b>表</b><span class="mono">${esc(res.table)}</span></div>
      <div><b>身份</b><span>${esc(res.user || '—')} · ${esc(res.role)}</span></div>
      <div><b>最高分级</b><span class="pill ${lvl(res.maxLevel)}">${esc(res.maxLevel)}</span></div>
      <div><b>行过滤</b><span class="mono">${res.where ? esc(res.where) : '无'}</span></div>
    </div>
    <div class="grid c2" style="margin-top:12px">
      <div>
        <div class="card-title" style="margin-bottom:6px">列级（${res.columns.length}）</div>
        ${res.columns.length ? `<div class="table-wrap"><table>
          <thead><tr><th>列</th><th>原始分级</th><th>判定</th><th>脱敏</th></tr></thead>
          <tbody>${res.columns.map(c => `<tr>
            <td class="mono">${esc(c.column)}</td>
            <td class="mono dim small">${esc(c.classification)}</td>
            <td><span class="pill ${lvl(c.level)}">${esc(c.level)}</span></td>
            <td>${c.enforced ? `<span class="pill info">${esc(c.mask)}</span>` : '<span class="dim small">不脱敏</span>'}</td>
          </tr>`).join('')}</tbody></table></div>`
          : '<div class="empty" style="padding:16px">没有列策略命中</div>'}
      </div>
      <div>
        <div class="card-title" style="margin-bottom:6px">行级（${res.rows.length}）</div>
        ${res.rows.length ? `<div class="table-wrap"><table>
          <thead><tr><th>角色</th><th>原始条件</th><th>渲染后</th></tr></thead>
          <tbody>${res.rows.map(r => `<tr>
            <td><span class="pill info">${esc(r.role)}</span></td>
            <td class="mono small">${esc(r.filter)}</td>
            <td class="mono small">${esc(r.rendered)}</td>
          </tr>`).join('')}</tbody></table></div>`
          : '<div class="empty" style="padding:16px">没有行策略命中</div>'}
      </div>
    </div>
    ${(res.notes || []).length ? `<div class="hint" style="margin-top:10px">${res.notes.map(n => '· ' + esc(n)).join('<br/>')}</div>` : ''}`;
}

async function govColumns() {
  const [items, dss] = await Promise.all([api('/policies/columns'), api('/datasources')]);
  return `
    <div class="card">
      <div class="card-head">
        <div class="card-title">列级脱敏（${items.length}）</div>
        <button class="sm primary" data-act="new-col-policy">新增策略</button>
      </div>
      ${items.length ? `<div class="table-wrap"><table>
        <thead><tr><th>表</th><th>列</th><th>分级</th><th>脱敏</th><th>更新时间</th><th></th></tr></thead>
        <tbody>${items.map(p => `<tr>
          <td class="mono">${esc(p.table)}</td>
          <td class="mono">${esc(p.column)}</td>
          <td><span class="pill ${p.classification === 'secret' ? 'err' : p.classification === 'confidential' ? 'warn' : 'mute'}">${esc(p.classification)}</span></td>
          <td class="mono">${esc(p.mask)}</td>
          <td class="dim small">${fmtTime(p.updatedAt)}</td>
          <td><button class="sm ghost" data-act="prev-table" data-table="${esc(p.table)}">看效果</button></td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><b>还没有列策略</b>给敏感列设置脱敏方式，预览与导出都会生效</div>`}
      <div class="hint" style="margin-top:10px">已接入 ${dss.length} 个数据源；脱敏在结果返回前生效，SQL 仍按原值计算。</div>
    </div>`;
}

async function govRows() {
  const items = await api('/policies/rows');
  return `
    <div class="card">
      <div class="card-head">
        <div class="card-title">行级权限（${items.length}）</div>
        <button class="sm primary" data-act="new-row-policy">新增策略</button>
      </div>
      ${items.length ? `<div class="table-wrap"><table>
        <thead><tr><th>表</th><th>角色</th><th>过滤条件</th><th>说明</th><th></th></tr></thead>
        <tbody>${items.map(p => `<tr>
          <td class="mono">${esc(p.table)}</td>
          <td><span class="pill info">${esc(p.role)}</span></td>
          <td class="mono small">${esc(p.filter)}</td>
          <td class="dim small">${esc(p.description || '—')}</td>
          <td><button class="sm ghost" data-act="del-row-policy" data-id="${esc(p.id)}">删除</button></td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><b>还没有行级策略</b>可按角色限定可见行，例如外部访客只看已支付订单</div>`}
      <div class="hint" style="margin-top:10px">条件支持 <span class="mono">${'${user}'}</span> 与 <span class="mono">${'${role}'}</span> 占位符；多条策略之间是 AND。</div>
    </div>`;
}

async function govApprovals() {
  const items = await api('/exports?limit=50');
  const pending = items.filter(e => e.status === 'pending_approval');
  const recent = items.filter(e => e.status !== 'pending_approval').slice(0, 12);
  const canApprove = S.me && S.me.role === 'admin';
  const cls = e => (e.params && e.params._classification) || '—';
  const tables = e => {
    const t = e.params && e.params._tables;
    return Array.isArray(t) && t.length ? t.join(', ') : '—';
  };
  return `
    <div class="card">
      <div class="card-head">
        <div class="card-title">待审批导出（${pending.length}）</div>
        <span class="hint">high 级数据导出不会直接执行，批准后才开始导出</span>
      </div>
      ${pending.length ? `<div class="table-wrap"><table>
        <thead><tr><th>类型</th><th>目标</th><th>分级</th><th>涉及表</th><th>申请人</th><th>申请时间</th><th></th></tr></thead>
        <tbody>${pending.map(e => `<tr>
          <td><span class="pill info">${esc(e.kind)}</span></td>
          <td class="mono">${esc(e.target)}</td>
          <td><span class="pill err">${esc(cls(e))}</span></td>
          <td class="mono small">${esc(tables(e))}</td>
          <td class="dim small">${esc(e.createdBy || '—')}</td>
          <td class="dim small">${fmtTime(e.createdAt)}</td>
          <td class="row">${canApprove ? `
            <button class="sm primary" data-act="approve-export" data-id="${esc(e.id)}">批准并导出</button>
            <button class="sm ghost" data-act="reject-export" data-id="${esc(e.id)}">驳回</button>` : '<span class="dim small">等待管理员</span>'}
          </td>
        </tr>`).join('')}</tbody></table></div>`
      : `<div class="empty"><b>没有待审批的导出</b>只有命中 high 级列策略的导出才会进入这里</div>`}
      <div class="hint" style="margin-top:10px">分级按导出实际读取的表计算；metric / dashboard / dataset 都会解析到物理表。</div>
    </div>

    <div class="card">
      <div class="card-head"><div class="card-title">最近审批结果</div></div>
      ${recent.length ? `<div class="table-wrap"><table>
        <thead><tr><th>类型</th><th>目标</th><th>状态</th><th>说明</th><th>时间</th></tr></thead>
        <tbody>${recent.map(e => `<tr>
          <td><span class="pill info">${esc(e.kind)}</span></td>
          <td class="mono">${esc(e.target)}</td>
          <td>${statusPill(e.status)}</td>
          <td class="dim small">${esc(e.error || '—')}</td>
          <td class="dim small">${fmtTime(e.finishedAt || e.createdAt)}</td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty">还没有审批记录</div>`}
    </div>`;
}

async function govMembers() {
  const items = await api('/members');  return `
    <div class="card">
      <div class="card-head">
        <div class="card-title">工作区成员（${items.length}）</div>
        <button class="sm primary" data-act="new-member">添加成员</button>
      </div>
      ${items.length ? `<div class="table-wrap"><table>
        <thead><tr><th>用户</th><th>角色</th><th>加入时间</th><th></th></tr></thead>
        <tbody>${items.map(m => `<tr>
          <td class="mono">${esc(m.userId)}</td>
          <td><span class="pill ${m.role === 'admin' ? 'ok' : m.role === 'editor' ? 'info' : 'mute'}">${esc(m.role)}</span></td>
          <td class="dim small">${fmtTime(m.createdAt)}</td>
          <td><button class="sm ghost" data-act="del-member" data-user="${esc(m.userId)}">移除</button></td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><b>还没有显式成员</b>dev 环境默认以 admin 身份访问</div>`}
    </div>`;
}

async function govShares() {
  const items = await api('/shares');
  return `
    <div class="card">
      <div class="card-head">
        <div class="card-title">分享链接（${items.length}）</div>
        <button class="sm primary" data-act="new-share">创建链接</button>
      </div>
      ${items.length ? `<div class="table-wrap"><table>
        <thead><tr><th>类型</th><th>目标</th><th>令牌</th><th>过期</th><th></th></tr></thead>
        <tbody>${items.map(s => `<tr>
          <td><span class="pill info">${esc(s.kind)}</span></td>
          <td class="mono">${esc(s.target)}</td>
          <td class="mono small">${esc(s.token)}</td>
          <td class="dim small">${s.expiresAt ? fmtTime(s.expiresAt) : '永不过期'}</td>
          <td class="row">
            <button class="sm ghost" data-act="copy-share" data-token="${esc(s.token)}">复制链接</button>
            <button class="sm ghost" data-act="del-share" data-token="${esc(s.token)}">删除</button>
          </td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><b>还没有分享</b>分享链接无需登录，适合发给业务方看板</div>`}
    </div>`;
}

async function pageOps(view) {
  const [exports, subs, cache, insts] = await Promise.all([
    api('/exports?limit=20'), api('/subscriptions'), api('/cache/stats'), api('/instances?limit=50').then(r => r.items || []),
  ]);
  const failed = insts.filter(i => i.status === 'failed').length;
  const pendingExports = exports.filter(e => e.status === 'pending_approval').length;
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">运维</h2>
        <p class="page-desc">导出、订阅、缓存与近期失败。缓存按「工作区+指标+参数+用户」隔离，不会串数据。</p>
      </div>
    </div>

    <div class="grid c4">
      <div class="stat"><div class="stat-label">缓存条目</div><div class="stat-value">${cache.entries}</div><div class="stat-sub">命中 ${cache.hits} 次 · ${fmtBytes(cache.bytes)}</div></div>
      <div class="stat"><div class="stat-label">缓存 TTL</div><div class="stat-value">${cache.ttlSec}s</div><div class="stat-sub">0 表示关闭</div></div>
      <div class="stat"><div class="stat-label">订阅</div><div class="stat-value">${subs.length}</div><div class="stat-sub">${subs.filter(s => s.enabled).length} 个启用</div></div>
      <div class="stat"><div class="stat-label">近期失败</div><div class="stat-value">${failed}</div><div class="stat-sub">最近 ${insts.length} 个实例</div></div>
    </div>

    <div class="card">
      <div class="card-head">
        <div class="card-title">导出任务${pendingExports ? ` <span class="pill warn">待审批 ${pendingExports}</span>` : ''}</div>
        <div class="row">
          <button class="sm ghost" data-act="purge-cache">清理过期缓存</button>
          <button class="sm primary" data-act="new-export">新建导出</button>
        </div>
      </div>
      ${exports.length ? `<div class="table-wrap"><table>
        <thead><tr><th>类型</th><th>目标</th><th>状态</th><th>行数</th><th>创建时间</th><th></th></tr></thead>
        <tbody>${exports.map(e => `<tr>
          <td><span class="pill info">${esc(e.kind)}</span></td>
          <td class="mono">${esc(e.target)}</td>
          <td>${statusPill(e.status)}${e.error ? `<div class="dim small">${esc(e.error)}</div>` : ''}</td>
          <td class="mono">${e.rowCount}</td>
          <td class="dim small">${fmtTime(e.createdAt)}</td>
          <td>${e.status === 'success' ? `<a class="btn sm ghost" href="/api/v1/exports/${esc(e.id)}/download">下载</a>`
            : e.status === 'pending_approval' ? '<span class="dim small">待管理员审批</span>' : '—'}</td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><b>还没有导出</b>按指标或看板导 CSV</div>`}
    </div>

    <div class="card">
      <div class="card-head">
        <div class="card-title">订阅推送</div>
        <button class="sm primary" data-act="new-sub">新建订阅</button>
      </div>
      ${subs.length ? `<div class="table-wrap"><table>
        <thead><tr><th>名称</th><th>目标</th><th>渠道</th><th>cron</th><th>上次</th><th></th></tr></thead>
        <tbody>${subs.map(s => `<tr>
          <td><b>${esc(s.name)}</b></td>
          <td class="mono">${esc(s.kind)} · ${esc(s.target)}</td>
          <td class="mono dim small">${esc(s.channel)}${s.endpoint ? `<div class="mute small">${esc(s.endpoint)}</div>` : ''}</td>
          <td class="mono small">${esc(s.schedule || '手动')}</td>
          <td class="small">${s.lastRunAt ? fmtTime(s.lastRunAt) + ' ' + statusPill(s.lastStatus || 'unknown') : '—'}</td>
          <td class="row">
            <button class="sm ghost" data-act="run-sub" data-id="${esc(s.id)}">立即推送</button>
            <button class="sm ghost" data-act="del-sub" data-id="${esc(s.id)}">删除</button>
          </td>
        </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><b>还没有订阅</b>支持 webhook 与落盘两种渠道</div>`}
    </div>`;
}

/* ---------------- 治理相关弹窗与动作 ---------------- */

function newColumnPolicyModal() {
  modal('新增列策略', `
    <div class="stack">
      <div class="field"><label>表名</label><input id="cp-table" placeholder="dwd_order" /></div>
      <div class="field"><label>列名</label><input id="cp-col" placeholder="user_id" /></div>
      <div class="field"><label>数据分级</label>
        <select id="cp-class">
          <option value="internal">internal</option>
          <option value="public">public</option>
          <option value="confidential">confidential</option>
          <option value="secret">secret</option>
        </select>
      </div>
      <div class="field"><label>脱敏方式</label>
        <select id="cp-mask">
          <option value="partial">partial（保留首尾）</option>
          <option value="hash">hash（不可逆指纹）</option>
          <option value="email">email（保留域名）</option>
          <option value="phone">phone（中间四位）</option>
          <option value="full">full（整列 ***）</option>
          <option value="none">none（不脱敏）</option>
        </select>
      </div>
      <div class="hint">脱敏只影响返回给前端的值，SQL 计算仍使用原值。</div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-col-policy">保存</button>
      </div>
    </div>`);
}

function newRowPolicyModal() {
  modal('新增行级策略', `
    <div class="stack">
      <div class="field"><label>表名</label><input id="rp-table" placeholder="dwd_order" /></div>
      <div class="field"><label>角色</label>
        <select id="rp-role">
          <option value="*">全部角色（*）</option>
          <option value="viewer">viewer</option>
          <option value="editor">editor</option>
          <option value="admin">admin</option>
        </select>
      </div>
      <div class="field"><label>过滤条件</label><input id="rp-filter" placeholder="status = 'paid' AND region = ${'${user}'}" class="mono" /></div>
      <div class="field"><label>说明</label><input id="rp-desc" placeholder="外部访客只看已支付" /></div>
      <div class="hint">支持 ${'${user}'} / ${'${role}'} 占位符；条件之间自动 AND。</div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-row-policy">保存</button>
      </div>
    </div>`);
}

function newMemberModal() {
  modal('添加成员', `
    <div class="stack">
      <div class="field"><label>用户 ID</label><input id="mb-user" placeholder="alice" class="mono" /></div>
      <div class="field"><label>角色</label>
        <select id="mb-role"><option value="viewer">viewer</option><option value="editor">editor</option><option value="admin">admin</option></select>
      </div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-member">添加</button>
      </div>
    </div>`);
}

function newShareModal() {
  modal('创建分享链接', `
    <div class="stack">
      <div class="field"><label>类型</label>
        <select id="sh-kind"><option value="dashboard">看板</option><option value="metric">指标</option></select>
      </div>
      <div class="field"><label>目标名称</label><input id="sh-target" placeholder="ops_overview" class="mono" /></div>
      <div class="field"><label>有效期（天，0 表示永久）</label><input id="sh-ttl" type="number" value="7" /></div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-share">创建</button>
      </div>
    </div>`);
}

function newExportModal() {
  modal('新建导出', `
    <div class="stack">
      <div class="field"><label>类型</label>
        <select id="ex-kind"><option value="metric">指标</option><option value="dashboard">看板</option></select>
      </div>
      <div class="field"><label>目标名称</label><input id="ex-target" placeholder="gmv" class="mono" /></div>
      <div class="field"><label>起始日期</label><input id="ex-from" type="date" /></div>
      <div class="field"><label>结束日期</label><input id="ex-to" type="date" /></div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-export">开始导出</button>
      </div>
    </div>`);
}

function newSubModal() {
  modal('新建订阅', `
    <div class="stack">
      <div class="field"><label>名称</label><input id="sb-name" placeholder="gmv_daily" /></div>
      <div class="field"><label>类型</label>
        <select id="sb-kind"><option value="metric">指标</option><option value="dashboard">看板</option></select>
      </div>
      <div class="field"><label>目标名称</label><input id="sb-target" placeholder="gmv" class="mono" /></div>
      <div class="field"><label>渠道</label>
        <select id="sb-channel"><option value="webhook">webhook</option><option value="file">落盘文件</option></select>
      </div>
      <div class="field"><label>Endpoint</label><input id="sb-endpoint" placeholder="https://example.com/hook" /></div>
      <div class="field"><label>cron 表达式</label><input id="sb-schedule" placeholder="0 9 * * *" class="mono" /></div>
      <div class="row" style="justify-content:flex-end">
        <button class="ghost" data-act="close-modal">取消</button>
        <button class="primary" data-act="save-sub">保存</button>
      </div>
    </div>`);
}

/* 把治理页的动作挂到全局事件委托上 */
document.addEventListener('click', async (ev) => {
  const el = ev.target.closest('[data-act]');
  if (!el) return;
  const act = el.dataset.act;
  const acts = ['gov-tab', 'new-col-policy', 'save-col-policy', 'new-row-policy', 'save-row-policy',
    'del-row-policy', 'new-member', 'save-member', 'del-member', 'new-share', 'save-share',
    'del-share', 'copy-share', 'new-export', 'save-export', 'new-sub', 'save-sub', 'run-sub',
    'del-sub', 'purge-cache', 'prev-table', 'approve-export', 'reject-export', 'run-simulate'];
  if (!acts.includes(act)) return;

  try {
    switch (act) {
      case 'gov-tab':
        S.selected.govTab = el.dataset.tab;
        await render();
        break;
      case 'run-simulate': {
        el.disabled = true;
        const box = document.getElementById('sim-result');
        if (box) box.innerHTML = '<div class="empty" style="padding:16px">计算中…</div>';
        try {
          const res = await api('/policies/simulate?table=' + encodeURIComponent(val('sim-table')) +
            '&user=' + encodeURIComponent(val('sim-user')) +
            '&role=' + encodeURIComponent(val('sim-role')));
          if (box) box.innerHTML = renderSimulation(res);
        } finally {
          el.disabled = false;
        }
        break;
      }
      case 'new-col-policy': newColumnPolicyModal(); break;
      case 'save-col-policy': {
        el.disabled = true;
        await api('/policies/columns', {
          method: 'POST',
          body: {
            table: val('cp-table'), column: val('cp-col'),
            classification: val('cp-class'), mask: val('cp-mask'),
          },
        });
        closeModal(); toast('列策略已保存', 'ok'); await render();
        break;
      }
      case 'new-row-policy': newRowPolicyModal(); break;
      case 'save-row-policy': {
        el.disabled = true;
        await api('/policies/rows', {
          method: 'POST',
          body: { table: val('rp-table'), role: val('rp-role'), filter: val('rp-filter'), description: val('rp-desc') },
        });
        closeModal(); toast('行级策略已保存', 'ok'); await render();
        break;
      }
      case 'del-row-policy':
        if (!confirm('删除该策略？')) return;
        await api('/policies/rows/' + el.dataset.id, { method: 'DELETE' });
        toast('已删除', 'ok'); await render();
        break;
      case 'new-member': newMemberModal(); break;
      case 'save-member': {
        el.disabled = true;
        await api('/members', { method: 'POST', body: { userId: val('mb-user'), role: val('mb-role') } });
        closeModal(); toast('成员已添加', 'ok'); await render();
        break;
      }
      case 'del-member':
        if (!confirm('移除该成员？')) return;
        await api('/members/' + encodeURIComponent(el.dataset.user), { method: 'DELETE' });
        toast('已移除', 'ok'); await render();
        break;
      case 'new-share': newShareModal(); break;
      case 'save-share': {
        el.disabled = true;
        const s = await api('/shares', {
          method: 'POST',
          body: { kind: val('sh-kind'), target: val('sh-target'), ttlDays: Number(val('sh-ttl')) },
        });
        closeModal();
        const url = location.origin + '/#/share/' + s.token;
        try { await navigator.clipboard.writeText(url); toast('链接已复制', 'ok'); }
        catch (e) { toast('已创建：' + url, 'ok'); }
        await render();
        break;
      }
      case 'del-share':
        await api('/shares/' + encodeURIComponent(el.dataset.token), { method: 'DELETE' });
        toast('已删除', 'ok'); await render();
        break;
      case 'copy-share': {
        const url = location.origin + '/#/share/' + el.dataset.token;
        try { await navigator.clipboard.writeText(url); toast('已复制', 'ok'); }
        catch (e) { toast(url, ''); }
        break;
      }
      case 'new-export': newExportModal(); break;
      case 'approve-export': {
        if (!confirm('批准这次导出？批准后立即开始导出 high 级数据，并写入审计。')) return;
        el.disabled = true;
        await api('/exports/' + encodeURIComponent(el.dataset.id) + '/approve', { method: 'POST' });
        toast('已批准，导出开始执行', 'ok');
        await render();
        break;
      }
      case 'reject-export': {
        const reason = prompt('驳回理由（必填）：');
        if (!reason) return;
        el.disabled = true;
        await api('/exports/' + encodeURIComponent(el.dataset.id) + '/reject', { method: 'POST', body: { reason } });
        toast('已驳回', 'ok');
        await render();
        break;
      }
      case 'save-export': {
        el.disabled = true;
        const target = val('ex-target');
        const job = await api('/exports', {
          method: 'POST',
          body: {
            kind: val('ex-kind'), target,
            params: { from: val('ex-from'), to: val('ex-to'), granularity: 'day' },
          },
        });
        closeModal();
        if (job.status === 'pending_approval') {
          toast('该导出命中 high 级数据，已提交管理员审批', 'warn');
          S.selected.govTab = 'approvals';
          await render();
          return;
        }
        toast('导出已排队：' + job.id, 'ok');
        await render();
        // 轮询直到完成，最多 30 秒
        for (let i = 0; i < 15; i++) {
          await new Promise(r => setTimeout(r, 2000));
          const j = await api('/exports/' + job.id);
          if (j.status === 'success') { toast('导出完成，共 ' + j.rowCount + ' 行', 'ok'); await render(); return; }
          if (j.status === 'failed') { toast('导出失败：' + j.error, 'err'); await render(); return; }
        }
        break;
      }
      case 'new-sub': newSubModal(); break;
      case 'save-sub': {
        el.disabled = true;
        await api('/subscriptions', {
          method: 'POST',
          body: {
            name: val('sb-name'), kind: val('sb-kind'), target: val('sb-target'),
            channel: val('sb-channel'), endpoint: val('sb-endpoint'), schedule: val('sb-schedule'),
            enabled: true,
          },
        });
        closeModal(); toast('订阅已保存', 'ok'); await render();
        break;
      }
      case 'run-sub': {
        el.disabled = true;
        const r = await api('/subscriptions/' + el.dataset.id + '/run', { method: 'POST' });
        toast('推送完成：' + (r.channel === 'webhook' ? 'HTTP ' + r.status : r.path), 'ok');
        await render();
        break;
      }
      case 'del-sub':
        if (!confirm('删除该订阅？')) return;
        await api('/subscriptions/' + el.dataset.id, { method: 'DELETE' });
        toast('已删除', 'ok'); await render();
        break;
      case 'purge-cache': {
        const r = await api('/cache/purge', { method: 'POST' });
        toast('已清理 ' + r.purged + ' 条过期缓存', 'ok');
        await render();
        break;
      }
      case 'prev-table': {
        const t = el.dataset.table;
        const dss = await api('/datasources');
        // 用已采集元数据反查 schema 与所属数据源，避免写死
        const dssList = dss;
        let schema = '', dsName = dssList[0] && dssList[0].name;
        for (const d of dssList) {
          const dsets = await api('/datasets?datasourceId=' + encodeURIComponent(d.id));
          const hit = dsets.items ? dsets.items.find(x => x.name.toLowerCase() === t.toLowerCase()) : null;
          if (hit) { schema = hit.schema; dsName = d.name; break; }
        }
        const rows = await api('/table/rows?datasource=' + encodeURIComponent(dsName) +
          '&schema=' + encodeURIComponent(schema) + '&table=' + encodeURIComponent(t) + '&limit=8');
        modal('预览：' + t, `
          ${Object.keys(rows.masks).length ? `<div class="hint" style="margin-bottom:8px">已脱敏：${Object.entries(rows.masks).map(([k, v]) => `<span class="pill warn">${esc(k)}:${esc(v)}</span>`).join(' ')}</div>` : ''}
          ${rows.filter ? `<div class="hint mono" style="margin-bottom:8px">行过滤：${esc(rows.filter)}</div>` : ''}
          <div class="table-wrap"><table>
            <thead><tr>${rows.columns.map(c => `<th>${esc(c)}</th>`).join('')}</tr></thead>
            <tbody>${rows.rows.map(r => `<tr>${r.map(v => `<td class="mono small">${esc(v == null ? 'NULL' : v)}</td>`).join('')}</tr>`).join('')}</tbody>
          </table></div>`);
        break;
      }
    }
  } catch (err) {
    toast(err.message, 'err');
    el.disabled = false;
  }
});
