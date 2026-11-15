/* GoDW 控制台 · 表结构浏览（T0903）与模型设计器（T0904）
 *
 * 设计要点：
 *   1. 表树按「数据源 → 库 → 表」三级，懒展开而不是一次拉全量；
 *      5000 张表也不卡，因为折叠的节点根本不进 DOM。
 *   2. 表详情把 PII 级别直接标在列上，不藏进二级弹窗——敏感字段是治理的主线。
 *   3. 模型设计器三栏：左树、中字段表格、右属性。字段编辑立刻重算 DDL，
 *      不点「预览」也能看到结果，减少一次无意义往返。
 *   4. 发布前强制 check：breaking 变更必须补 reason 并二次确认，
 *      前端拦不住的，后端也会拦。
 */
'use strict';

const DESIGN = {
  tables: [],
  datasources: [],
  dsMap: {},
  curTable: '',
  // 表结构树：系统表默认收起，名称排序（这两项是「看」的偏好，不写进后端）
  showSys: false,
  sortKey: 'name',
  models: [],
  curModel: '',
  spec: null,
  dirty: false,
  tab: 'fields',
  ddl: '',
  ddlTimer: null,
};

/* ============================== 表结构浏览（T0903） ============================== */

Object.assign(ROUTES, {
  datasets: { title: '表结构', sub: '已采集的库表、字段与敏感标记', render: pageTables },
  models: { title: '模型设计', sub: '字段级建模、DDL 实时编译与版本治理', render: pageModelDesign },
});

async function pageTables(view) {
  view.innerHTML = `<div class="page-head">
    <div>
      <h2 class="page-title">表结构</h2>
      <p class="page-desc">左侧按数据源分组浏览，右侧看字段、类型与敏感级别。可直接生成 ODS 模型进入建模流程。</p>
    </div>
    <div class="row">
      <select id="tb-ds" style="width:auto;min-width:150px"><option value="">全部数据源</option></select>
      <select id="tb-sort" style="width:auto" title="树的排序方式">
        <option value="name">按名称</option>
        <option value="rowCount">按行数</option>
        <option value="sizeBytes">按大小</option>
        <option value="collectedAt">最近采集</option>
      </select>
      <button class="sm ghost" data-act="tb-collect">采集元数据</button>
      <button class="sm ghost" data-act="tb-pii">识别敏感字段</button>
    </div>
  </div>
  <div class="split3">
    <div class="card" style="padding:12px">
      <input id="tb-search" placeholder="搜索表名或注释" style="margin-bottom:8px">
      <label class="hint" id="tb-sys-wrap" style="display:none;align-items:center;gap:6px;margin:0 0 8px">
        <input type="checkbox" id="tb-sys" style="width:auto" />
        <span id="tb-sys-label">显示系统表</span>
      </label>
      <div class="tree" id="tb-tree">${UI.skeleton(8)}</div>
    </div>
    <div class="card" id="tb-detail">${UI.empty({ title: '选择一张表', desc: '从左侧挑一张表查看字段明细' })}</div>
    <div class="card" id="tb-side">
      <div class="card-title" style="margin-bottom:12px">采集信息</div>
      <div class="hint">选中表后显示行数、体积、采集时间与敏感级别。</div>
    </div>
  </div>`;

  try {
    const [ds, dss] = await Promise.all([api('/datasets?pageSize=200'), api('/datasources')]);
    DESIGN.tables = (ds && ds.items) || [];
    DESIGN.datasources = dss || [];
    DESIGN.dsMap = {};
    DESIGN.datasources.forEach(d => { DESIGN.dsMap[d.id] = d.name; });
    const sel = view.querySelector('#tb-ds');
    sel.innerHTML = '<option value="">全部数据源</option>' +
      DESIGN.datasources.map(d => `<option value="${esc(d.id)}">${esc(d.name)}</option>`).join('');
    // 系统表（public 下平台自己的 45 张表）默认收起：它们和业务表混在一棵树里，
    // 「找 order 表」要先翻过 alert_event / audit_log 一大串，这是审计里点名的脏数据问题。
    const sysN = DESIGN.tables.filter(isSystemTable).length;
    const wrap = view.querySelector('#tb-sys-wrap');
    if (sysN) {
      wrap.style.display = 'flex';
      view.querySelector('#tb-sys-label').textContent = `显示系统表（${sysN}）`;
    }
  } catch (e) {
    view.querySelector('#tb-tree').innerHTML = UI.error(e, 'reload');
    return;
  }

  renderTree(view);
  view.querySelector('#tb-search').addEventListener('input', () => renderTree(view));
  view.querySelector('#tb-ds').addEventListener('change', () => renderTree(view));
  view.querySelector('#tb-sort').addEventListener('change', (ev) => { DESIGN.sortKey = ev.target.value; renderTree(view); });
  const sysBox = view.querySelector('#tb-sys');
  if (sysBox) sysBox.addEventListener('change', (ev) => { DESIGN.showSys = ev.target.checked; renderTree(view); });
  view.querySelector('[data-act="tb-collect"]').addEventListener('click', () => collectModal());
  view.querySelector('[data-act="tb-pii"]').addEventListener('click', () => piiModal());

  // 从全局搜索跳过来时直接打开对应表
  const want = sessionStorage.getItem('godw.openTable');
  if (want) {
    sessionStorage.removeItem('godw.openTable');
    const hit = DESIGN.tables.find(t => t.name === want || t.schema + '.' + t.name === want);
    if (hit) selectTable(view, hit.id);
  }
}

// 系统表判定：采集会把元数据库自己的表带进来（alert_event / app_user / audit_log …）
const SYS_SCHEMAS = { public: 1, information_schema: 1, pg_catalog: 1 };
function isSystemTable(t) {
  return !!SYS_SCHEMAS[t.schema] || String(t.name || '').startsWith('_godw');
}

function sortTables(list) {
  const key = DESIGN.sortKey || 'name';
  const arr = list.slice();
  if (key === 'name') return arr.sort((a, b) => a.name.localeCompare(b.name));
  if (key === 'collectedAt') return arr.sort((a, b) => String(b.collectedAt || '').localeCompare(String(a.collectedAt || '')));
  return arr.sort((a, b) => (b[key] || 0) - (a[key] || 0));
}

function renderTree(view) {
  const kw = (view.querySelector('#tb-search').value || '').trim().toLowerCase();
  const dsFilter = view.querySelector('#tb-ds').value;
  const tree = view.querySelector('#tb-tree');
  let hiddenSys = 0;

  const groups = {};
  DESIGN.tables.forEach(t => {
    if (dsFilter && t.datasourceId !== dsFilter) return;
    if (kw && !(t.name.toLowerCase().includes(kw) || (t.comment || '').toLowerCase().includes(kw))) return;
    if (!DESIGN.showSys && isSystemTable(t)) { hiddenSys++; return; }
    const dsName = DESIGN.dsMap[t.datasourceId] || t.datasourceId || 'unknown';
    const key = dsName + ' / ' + (t.schema || 'public');
    (groups[key] = groups[key] || []).push(t);
  });

  const keys = Object.keys(groups).sort();
  if (!keys.length) {
    tree.innerHTML = UI.empty({
      title: DESIGN.tables.length ? '没有匹配的表' : '还没有采集到表',
      desc: hiddenSys
        ? `已隐藏 ${hiddenSys} 张系统表；要找 alert_event / app_user 这类表，勾选上方「显示系统表」。`
        : (DESIGN.tables.length ? '换个关键词试试' : '先在数据源页点「采集元数据」'),
    });
    return;
  }
  tree.innerHTML = keys.map(k => {
    const list = sortTables(groups[k]);
    return `<div class="tree-group" data-g="${esc(k)}">
      <div class="tree-node tree-l1" data-toggle="${esc(k)}">
        <span class="tw">▾</span><span class="ti">▤</span>
        <span style="flex:1;overflow:hidden;text-overflow:ellipsis">${esc(k)}</span>
        <span class="mute small">${list.length}</span>
      </div>
      <div data-body="${esc(k)}">
        ${list.map(t => `<div class="tree-node tree-l2 ${DESIGN.curTable === t.id ? 'active' : ''}" data-table="${esc(t.id)}">
          <span class="tw"></span><span class="ti">▦</span>
          <span style="flex:1;overflow:hidden;text-overflow:ellipsis">${esc(t.name)}</span>
          ${t.pii && t.pii !== 'none' ? `<span class="tag sm" style="border-color:#4a3a1a;color:#e3b341">PII</span>` : ''}
        </div>`).join('')}
      </div>
    </div>`;
  }).join('');

  tree.querySelectorAll('[data-toggle]').forEach(n => {
    n.addEventListener('click', () => {
      const body = tree.querySelector(`[data-body="${CSS.escape(n.dataset.toggle)}"]`);
      const collapsed = body.style.display === 'none';
      body.style.display = collapsed ? '' : 'none';
      n.querySelector('.tw').textContent = collapsed ? '▾' : '▸';
    });
  });
  tree.querySelectorAll('[data-table]').forEach(n => {
    n.addEventListener('click', () => selectTable(view, n.dataset.table));
  });
}

async function selectTable(view, id) {
  DESIGN.curTable = id;
  const t = DESIGN.tables.find(x => x.id === id);
  if (!t) return;
  view.querySelectorAll('[data-table]').forEach(n => n.classList.toggle('active', n.dataset.table === id));

  const piiCls = { high: 'err', medium: 'warn', low: 'info', none: 'mute' };
  const detail = view.querySelector('#tb-detail');
  detail.innerHTML = `
    <div class="card-head">
      <div>
        <div class="card-title">${esc(t.schema ? t.schema + '.' : '')}${esc(t.name)}</div>
        <div class="hint">${esc(t.comment || '无注释')}</div>
      </div>
      <div class="row">
        <button class="sm primary" data-act="gen-ods">生成 ODS 模型</button>
      </div>
    </div>
    <div class="table-wrap"><table class="dt">
      <thead><tr><th>#</th><th>字段</th><th>类型</th><th>可空</th><th>注释</th><th>默认值</th></tr></thead>
      <tbody>${(t.columns || []).map((c, i) => `<tr>
        <td class="mute">${i + 1}</td>
        <td class="mono"><b>${esc(c.name)}</b>${c.primaryKey ? ' <span class="pill">PK</span>' : ''}</td>
        <td class="mono small">${esc(c.type)}</td>
        <td>${c.nullable ? '<span class="mute">是</span>' : '<span class="dim">否</span>'}</td>
        <td class="wrap dim small">${esc(c.comment || '—')}</td>
        <td class="mono small mute">${esc(c.default || '—')}</td>
      </tr>`).join('')}</tbody>
    </table></div>`;

  view.querySelector('#tb-side').innerHTML = `
    <div class="card-title" style="margin-bottom:12px">采集信息</div>
    <dl class="kv">
      <dt>数据源</dt><dd>${esc(DESIGN.dsMap[t.datasourceId] || t.datasourceId || '—')}</dd>
      <dt>库</dt><dd class="mono">${esc(t.schema || '—')}</dd>
      <dt>类型</dt><dd>${esc(t.type || 'table')}</dd>
      <dt>行数</dt><dd class="mono">${(t.rowCount || 0).toLocaleString()}</dd>
      <dt>体积</dt><dd class="mono">${fmtBytes(t.sizeBytes)}</dd>
      <dt>字段数</dt><dd class="mono">${(t.columns || []).length}</dd>
      <dt>敏感级别</dt><dd><span class="pill ${piiCls[t.pii] || 'mute'}">${esc(t.pii || 'none')}</span></dd>
      <dt>判定理由</dt><dd class="small dim">${esc(t.piiReason || '—')}</dd>
      <dt>采集时间</dt><dd class="small">${fmtTime(t.collectedAt)}</dd>
    </dl>
    <div class="card-title" style="margin:18px 0 10px">标签</div>
    <div class="row wrap" id="tb-tags">${(t.tags || []).length
      ? t.tags.map(x => `<span class="tag">${esc(x)}</span>`).join('')
      : '<span class="hint">暂无标签</span>'}</div>
    <button class="sm ghost" style="margin-top:10px" data-act="tb-tag">打标签</button>`;

  view.querySelector('[data-act="gen-ods"]').addEventListener('click', () => genODS(t));
  view.querySelector('[data-act="tb-tag"]').addEventListener('click', () => tagModal('dataset', t.name));
}

/** 一键生成 ODS 模型：走接入向导（看数据 → 挑字段 → 定分区），不再甩一段 YAML。 */
async function genODS(t) {
  return odsWizard(t);
}

/* ============================== 模型设计器（T0904） ============================== */

async function pageModelDesign(view) {
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">模型设计</h2>
        <p class="page-desc">字段改动即时编译 DDL；发布 breaking 变更会被拦截并要求填写原因。</p>
      </div>
      <div class="row">
        <button class="sm ghost" data-act="m-new">新建模型</button>
        <button class="sm ghost" data-act="m-check">校验</button>
        <button class="sm primary" data-act="m-publish">发布</button>
      </div>
    </div>
    <div class="split3" style="grid-template-columns:250px 1fr 290px">
      <div class="card" style="padding:12px">
        <input id="m-search" placeholder="搜索模型" style="margin-bottom:10px">
        <div class="tree" id="m-tree">${UI.skeleton(8)}</div>
      </div>
      <div class="card" id="m-main">${UI.empty({
        icon: '▤',
        title: '选择一个模型开始',
        desc: '分层顺序：ODS 原样落地 → DWD 清洗明细 → DWS 主题汇总 → ADS 对外口径。左侧点一个模型，或右上角「新建模型」。',
      })}</div>
      <div class="card" id="m-side">${UI.empty({ title: '属性', desc: '选中模型后可编辑分区、TTL、物化方式' })}</div>
    </div>`;

  await Promise.all([
    loadModels(view),
    // 属性面板的「数据源」下拉要用它。不并进来会变成直接进模型页时只有「未指定」，
    // 先从表结构页逛过来才正常——这种「看路径决定功能」的 bug 最难复现。
    loadDesignerDatasources(),
  ]);
  view.querySelector('#m-search').addEventListener('input', () => renderModelTree(view));
  view.querySelector('[data-act="m-new"]').addEventListener('click', () => newModelModal(view));
  view.querySelector('[data-act="m-check"]').addEventListener('click', () => checkCurrentModel());
  view.querySelector('[data-act="m-publish"]').addEventListener('click', () => publishCurrentModel());

  const want = sessionStorage.getItem('godw.openModel');
  if (want) { sessionStorage.removeItem('godw.openModel'); openModel(view, want); }
}

async function loadModels(view) {
  try {
    DESIGN.models = await api('/models');
  } catch (e) {
    view.querySelector('#m-tree').innerHTML = UI.error(e, 'reload');
    return;
  }
  renderModelTree(view);
}

async function loadDesignerDatasources() {
  try {
    const dss = await api('/datasources');
    if (!Array.isArray(dss)) return;
    DESIGN.datasources = dss;
    DESIGN.dsMap = {};
    dss.forEach(d => { DESIGN.dsMap[d.id] = d.name; });
  } catch (e) {
    // 数据源拉不到不该拦住建模：下拉退化成「未指定」，保存时后端仍会校验。
  }
}

function renderModelTree(view) {
  const kw = (view.querySelector('#m-search').value || '').trim().toLowerCase();
  const tree = view.querySelector('#m-tree');
  const layers = ['ods', 'dim', 'dwd', 'dws', 'ads'];
  const byLayer = {};
  DESIGN.models.forEach(m => {
    if (kw && !m.name.toLowerCase().includes(kw)) return;
    (byLayer[m.layer] = byLayer[m.layer] || []).push(m);
  });
  if (!Object.keys(byLayer).length) {
    tree.innerHTML = UI.empty({ title: '没有匹配的模型', desc: '换个关键词，或点右上角新建模型' });
    return;
  }
  tree.innerHTML = layers.filter(l => byLayer[l]).map(l => `
    <div class="tree-node tree-l1" data-toggle-m="${l}">
      <span class="tw">▾</span><span class="ti">◆</span>
      <span style="flex:1">${l.toUpperCase()}</span><span class="mute small">${byLayer[l].length}</span>
    </div>
    <div data-mbody="${l}">
      ${byLayer[l].map(m => `<div class="tree-node tree-l2 ${DESIGN.curModel === m.name ? 'active' : ''}" data-model="${esc(m.name)}">
        <span class="tw"></span><span class="ti">▢</span>
        <span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis" title="${esc(m.name)}">${esc(m.name)}</span>
        <span class="tag sm" style="flex:none">v${m.activeVersion}</span>
        <button class="tree-del" data-mdel="${esc(m.name)}" title="删除模型 ${esc(m.name)}">×</button>
      </div>`).join('')}
    </div>`).join('');
  tree.querySelectorAll('[data-toggle-m]').forEach(n => {
    n.addEventListener('click', () => {
      const body = tree.querySelector(`[data-mbody="${n.dataset.toggleM}"]`);
      const collapsed = body.style.display === 'none';
      body.style.display = collapsed ? '' : 'none';
      n.querySelector('.tw').textContent = collapsed ? '▾' : '▸';
    });
  });
  tree.querySelectorAll('[data-model]').forEach(n => {
    n.addEventListener('click', () => openModel(view, n.dataset.model));
  });
  // 删除键在行内，点它不能顺带把模型打开——stopPropagation 是必须的，不是保险。
  tree.querySelectorAll('[data-mdel]').forEach(n => {
    n.addEventListener('click', (ev) => {
      ev.stopPropagation();
      deleteModelFlow(view, n.dataset.mdel);
    });
  });
}

/** 删除前先查引用面，再走危险确认：删模型是元数据操作，物理表不动。 */
async function deleteModelFlow(view, name) {
  let refs = { metrics: [], pipelines: [] };
  try {
    refs = (await api('/models/' + encodeURIComponent(name) + '/refs')) || refs;
  } catch (e) {
    // 查不到引用也允许删：后端在 DELETE 时会再校验一次，不会真的留下悬空引用。
  }
  const metrics = refs.metrics || [];
  const pipelines = refs.pipelines || [];
  const hit = [];
  if (metrics.length) hit.push(`${metrics.length} 个指标：${metrics.join('、')}`);
  if (pipelines.length) hit.push(`${pipelines.length} 条管道：${pipelines.join('、')}`);

  ConfirmDanger({
    title: '删除模型',
    danger: `将删除模型 <b>${esc(name)}</b> 及其全部版本快照（含 DDL 历史），不可撤销。
      线上物理表不会被删除，需要清理数据请走保留策略。<br>
      ${hit.length
        ? `<b style="color:#ff7b72">以下对象仍引用它，删除后将失效：</b><br>${hit.map(h => esc(h)).join('<br>')}`
        : '当前没有指标或管道引用该模型。'}`,
    resource: name,
    onOK: async () => {
      await api('/models/' + encodeURIComponent(name) + '?force=' + (hit.length ? '1' : '0'), { method: 'DELETE' });
      toast('已删除模型 ' + name, 'ok');
      if (DESIGN.curModel === name) {
        DESIGN.curModel = '';
        DESIGN.spec = null;
        DESIGN.ddl = '';
        resetModelPanels();
      }
      await loadModels(view);
    },
  });
}

/** 清空中栏/右栏，回到「没选模型」的样子；删除当前模型后不能继续显示它的属性。 */
function resetModelPanels() {
  const main = document.getElementById('m-main');
  if (main) {
    main.innerHTML = UI.empty({
      icon: '▤',
      title: '选择一个模型开始',
      desc: '分层顺序：ODS 原样落地 → DWD 清洗明细 → DWS 主题汇总 → ADS 对外口径。左侧点一个模型，或右上角「新建模型」。',
    });
  }
  const side = document.getElementById('m-side');
  if (side) side.innerHTML = UI.empty({ title: '属性', desc: '选中模型后可编辑分区、TTL、物化方式' });
}

function openModel(view, name) {
  const m = DESIGN.models.find(x => x.name === name);
  if (!m) return;
  DESIGN.curModel = name;
  DESIGN.spec = JSON.parse(JSON.stringify(m.spec || {
    kind: 'Model', name, layer: m.layer, datasource: '', columns: [],
  }));
  DESIGN.tab = 'fields';
  DESIGN.dirty = false;
  DESIGN.ddl = m.ddl || '';
  view.querySelectorAll('[data-model]').forEach(n => n.classList.toggle('active', n.dataset.model === name));
  renderModelMain(view);
  renderModelSide(view);
  refreshDDL();
}

function renderModelMain(view) {
  const s = DESIGN.spec;
  const main = view.querySelector('#m-main');
  main.innerHTML = `
    <div class="card-head">
      <div>
        <div class="card-title">${esc(s.name)} <span class="pill info">${esc((s.layer || '').toUpperCase())}</span></div>
        <div class="hint">${esc(s.description || '无描述')} · 当前版本 v${(DESIGN.models.find(m => m.name === s.name) || {}).activeVersion || 0}</div>
      </div>
      <div class="row">
        <span class="hint" id="m-dirty">${DESIGN.dirty ? '有未保存改动' : '已同步'}</span>
        <button class="sm ghost danger" data-act="m-del" title="删除当前模型">删除</button>
      </div>
    </div>
    <div class="tabs">
      ${[['fields', '字段'], ['ddl', 'DDL 预览'], ['versions', '版本历史'], ['lineage', '血缘'], ['quality', '质量规则']]
        .map(([k, label]) => `<div class="tab ${DESIGN.tab === k ? 'active' : ''}" data-tab="${k}">${label}</div>`).join('')}
    </div>
    <div id="m-tab-body"></div>`;

  main.querySelectorAll('[data-tab]').forEach(el => {
    el.addEventListener('click', () => {
      DESIGN.tab = el.dataset.tab;
      renderModelMain(view);
    });
  });
  // 用已保存的模型名（curModel）而不是表单里的 name：属性面板可能被改成新名字，
  // 那还没落库，按它删会 404。
  main.querySelector('[data-act="m-del"]')?.addEventListener('click', () => deleteModelFlow(view, DESIGN.curModel || s.name));

  const body = main.querySelector('#m-tab-body');
  if (DESIGN.tab === 'fields') renderFieldTab(body);
  else if (DESIGN.tab === 'ddl') renderDDLTab(body);
  else if (DESIGN.tab === 'versions') renderVersionTab(body, s.name);
  else if (DESIGN.tab === 'lineage') renderModelLineage(body, s.name);
  else renderModelQuality(body, s.name);
}

function renderFieldTab(body) {
  const cols = DESIGN.spec.columns || [];
  const pk = DESIGN.spec.primaryKey || [];
  body.innerHTML = `
    <div class="row" style="margin-bottom:10px">
      <button class="sm ghost" data-act="f-add">+ 添加字段</button>
      <span class="hint">拖动手柄可调整顺序；主键列不可为空；描述会写进列注释（PostgreSQL / ClickHouse 生效）。</span>
    </div>
    <div class="table-wrap">
      ${cols.length ? `<div class="field-head">
        <span></span><span>字段</span><span>类型</span><span>可空</span><span>主键</span><span>描述</span><span></span>
      </div>` : ''}
      <div id="f-list">
        ${cols.length ? cols.map((c, i) => fieldRowHTML(c, i, pk.includes(c.name))).join('')
          : `<div class="empty">还没有字段，点「添加字段」开始</div>`}
      </div>
    </div>`;

  body.querySelector('[data-act="f-add"]')?.addEventListener('click', () => {
    DESIGN.spec.columns = DESIGN.spec.columns || [];
    DESIGN.spec.columns.push({ name: 'col_' + (DESIGN.spec.columns.length + 1), type: 'string', nullable: true });
    markDirty();
    renderFieldTab(body);
  });

  body.querySelectorAll('[data-f-name]').forEach(inp => {
    inp.addEventListener('input', () => {
      const i = Number(inp.dataset.fName);
      const old = DESIGN.spec.columns[i].name;
      const v = inp.value.trim();
      DESIGN.spec.columns[i].name = v;
      if (DESIGN.spec.primaryKey) {
        DESIGN.spec.primaryKey = DESIGN.spec.primaryKey.map(k => k === old ? v : k);
      }
      markDirty();
    });
  });
  body.querySelectorAll('[data-f-type]').forEach(sel => {
    sel.addEventListener('change', () => {
      DESIGN.spec.columns[Number(sel.dataset.fType)].type = sel.value.trim();
      markDirty();
      scheduleDDL();
    });
  });
  body.querySelectorAll('[data-f-null]').forEach(sel => {
    sel.addEventListener('change', () => {
      DESIGN.spec.columns[Number(sel.dataset.fNull)].nullable = sel.value === '1';
      // 主键列强制非空：否则 DDL 一定编译失败，不如当场纠正。
      const i = Number(sel.dataset.fNull);
      const name = DESIGN.spec.columns[i].name;
      if ((DESIGN.spec.primaryKey || []).includes(name)) DESIGN.spec.columns[i].nullable = false;
      markDirty();
      scheduleDDL();
    });
  });
  body.querySelectorAll('[data-f-pk]').forEach(sel => {
    sel.addEventListener('change', () => {
      const i = Number(sel.dataset.fPk);
      const name = DESIGN.spec.columns[i].name;
      const set = new Set(DESIGN.spec.primaryKey || []);
      if (sel.value === '1') { set.add(name); DESIGN.spec.columns[i].nullable = false; }
      else set.delete(name);
      DESIGN.spec.primaryKey = [...set];
      markDirty();
      // 主键打开会顺手把可空改成非空，输入框得重画一次才对得上
      renderFieldTab(body);
    });
  });
  body.querySelectorAll('[data-f-comment]').forEach(inp => {
    inp.addEventListener('input', () => {
      // 描述不重画整行：一重画光标就丢，边打字边跳是最烦的一种手感
      const i = Number(inp.dataset.fComment);
      DESIGN.spec.columns[i].comment = inp.value;
      markDirty();
      scheduleDDL();
    });
  });
  body.querySelectorAll('[data-f-del]').forEach(btn => {
    btn.addEventListener('click', () => {
      const i = Number(btn.dataset.fDel);
      const c = DESIGN.spec.columns[i];
      ConfirmDanger({
        title: '删除字段',
        danger: `字段 ${c.name} 将被删除，本操作不可撤销。`,
        resource: c.name,
        reason: false,
        onOK: async () => {
          DESIGN.spec.columns.splice(i, 1);
          DESIGN.spec.primaryKey = (DESIGN.spec.primaryKey || []).filter(k => k !== c.name);
          markDirty();
          renderFieldTab(body);
        },
      });
    });
  });
  enableRowDrag(body.querySelector('#f-list'), (from, to) => {
    const arr = DESIGN.spec.columns;
    const [item] = arr.splice(from, 1);
    arr.splice(to, 0, item);
    markDirty();
    renderFieldTab(body);
    refreshDDL();
  });
}

/**
 * 字段类型候选：只列编译器认识的逻辑类型。
 *
 * 原来这里是 input + datalist——datalist 只是「建议」，手打 `varchar(64)` 这类物理类型
 * 照样能提交，然后在类型映射矩阵里报「未知逻辑类型」，报错还指不到是哪一列。
 * 全下拉之后，输入即合法；历史值不在候选里时补一条「自定义」选项，不静默改掉。
 */
const FIELD_TYPES = [
  'string', 'text',
  'int', 'bigint', 'double',
  // decimal 不带参数时编译器按 decimal(20,4) 处理，属于合法写法，不该落到「自定义」
  'decimal', 'decimal(18,2)', 'decimal(38,10)',
  'bool',
  'date', 'datetime',
  'json', 'array<string>', 'array<int>', 'array<bigint>',
];

function fieldTypeOptions(cur) {
  const list = FIELD_TYPES.slice();
  const custom = cur && !list.includes(cur) ? cur : '';
  return list.map(t => `<option value="${esc(t)}" ${t === cur ? 'selected' : ''}>${esc(t)}</option>`).join('')
    + (custom ? `<option value="${esc(custom)}" selected>${esc(custom)}（自定义）</option>` : '');
}

function fieldRowHTML(c, i, isPk) {
  return `<div class="field-row" draggable="true" data-frow="${i}">
    <span class="fh">⋮⋮</span>
    <input data-f-name="${i}" value="${esc(c.name)}" placeholder="字段名">
    <select data-f-type="${i}">${fieldTypeOptions(c.type)}</select>
    <select data-f-null="${i}">
      <option value="1" ${c.nullable ? 'selected' : ''}>可空</option>
      <option value="0" ${c.nullable ? '' : 'selected'}>非空</option>
    </select>
    <select data-f-pk="${i}">
      <option value="0" ${isPk ? '' : 'selected'}>否</option>
      <option value="1" ${isPk ? 'selected' : ''}>是</option>
    </select>
    <input data-f-comment="${i}" value="${esc(c.comment || '')}" placeholder="描述（可选）">
    <button class="sm ghost danger" data-f-del="${i}" title="删除字段">×</button>
  </div>`;
}

/** 通用行拖拽：字段排序与看板卡片都用它，避免各写一套。 */
function enableRowDrag(container, onDrop) {
  if (!container) return;
  let from = -1;
  container.querySelectorAll('[data-frow]').forEach(row => {
    row.addEventListener('dragstart', () => {
      from = Number(row.dataset.frow);
      row.classList.add('dragging');
    });
    row.addEventListener('dragend', () => row.classList.remove('dragging'));
    row.addEventListener('dragover', (e) => {
      e.preventDefault();
      row.classList.add('drop-target');
    });
    row.addEventListener('dragleave', () => row.classList.remove('drop-target'));
    row.addEventListener('drop', (e) => {
      e.preventDefault();
      row.classList.remove('drop-target');
      const to = Number(row.dataset.frow);
      if (from >= 0 && from !== to) onDrop(from, to);
    });
  });
}

function renderDDLTab(body) {
  body.innerHTML = `<div id="m-ddl">${DESIGN.ddl ? '' : UI.skeleton(8)}</div>`;
  if (DESIGN.ddl) SqlEditor(body.querySelector('#m-ddl'), { value: DESIGN.ddl, readonly: true, height: 300 });
  else refreshDDL();
}

function renderVersionTab(body, name) {
  body.innerHTML = UI.skeleton(6);
  api('/models/' + encodeURIComponent(name) + '/versions').then(list => {
    if (!list || !list.length) { body.innerHTML = UI.empty({ title: '还没有版本', desc: '发布后才会产生版本快照' }); return; }
    body.innerHTML = `<div class="table-wrap"><table class="dt">
      <thead><tr><th>版本</th><th>发布时间</th><th>发布人</th><th>DDL 行数</th><th></th></tr></thead>
      <tbody>${list.map(v => `<tr>
        <td class="mono">v${v.version}</td>
        <td class="small">${fmtTime(v.createdAt)}</td>
        <td class="small dim">${esc(v.createdBy || '—')}</td>
        <td class="mono small">${(v.ddl || '').split('\n').length}</td>
        <td class="row">
          <button class="sm ghost" data-act="v-diff" data-v="${v.version}">与当前对比</button>
          <button class="sm ghost" data-act="v-rollback" data-v="${v.version}">回滚到此版本</button>
        </td>
      </tr>`).join('')}</tbody></table></div>`;

    body.querySelectorAll('[data-act="v-diff"]').forEach(b => {
      b.addEventListener('click', () => showVersionDiff(name, Number(b.dataset.v)));
    });
    body.querySelectorAll('[data-act="v-rollback"]').forEach(b => {
      b.addEventListener('click', () => {
        const ver = Number(b.dataset.v);
        ConfirmDanger({
          title: '回滚模型版本',
          danger: `将把 ${name} 回滚到 v${ver}，线上表结构按新版本重新对齐。`,
          resource: name,
          onOK: async (reason) => {
            await api('/models/' + encodeURIComponent(name) + '/rollback', {
              method: 'POST', body: { version: ver, reason },
            });
            toast('已回滚到 v' + ver, 'ok');
            await loadModels(document.getElementById('view'));
            openModel(document.getElementById('view'), name);
          },
        });
      });
    });
  }).catch(e => { body.innerHTML = UI.error(e); });
}

/** 版本 diff：走 rollback-preview，拿到的是「回滚会发生什么」而非简单文本比较。 */
async function showVersionDiff(name, version) {
  let d;
  try {
    d = await api('/models/' + encodeURIComponent(name) + '/rollback-preview?version=' + version);
  } catch (e) { toast(e.message, 'err'); return; }
  const changes = (d && d.changes) || [];
  modal(`v${version} 与当前版本的差异`, `
    <div class="hint" style="margin-bottom:12px">
      共 ${changes.length} 处变更${d && d.breaking ? '，其中包含 <b style="color:#ff7b72">breaking</b> 变更' : ''}。
    </div>
    ${changes.length ? `<div class="table-wrap"><table class="dt">
      <thead><tr><th>类型</th><th>对象</th><th>说明</th><th>风险</th></tr></thead>
      <tbody>${changes.map(c => `<tr>
        <td><span class="pill ${c.breaking ? 'err' : 'mute'}">${esc(c.kind)}</span></td>
        <td class="mono">${esc(c.column || c.object || '—')}</td>
        <td class="small wrap">${esc(c.detail || c.message || '')}</td>
        <td>${c.breaking ? '<span class="pill err">breaking</span>' : '<span class="pill ok">safe</span>'}</td>
      </tr>`).join('')}</tbody></table></div>`
      : UI.empty({ title: '没有结构差异' })}
  `);
}

/**
 * 模型血缘 tab。
 *
 * /lineage/focus 返回的是节点对象（{id,type,layer}），而 drawLineage 要的是
 * 纯 id 数组 + {source,target} 边，这里做一次归一。图上以当前模型为中心双向展开，
 * 上游/下游按「从中心可达的节点数」算，不是只数一跳。
 */
function renderModelLineage(body, name) {
  body.innerHTML = `<div id="m-lin">${UI.skeleton(6)}</div>`;
  api('/lineage/focus?table=' + encodeURIComponent(name) + '&depth=5').then(g => {
    const box = body.querySelector('#m-lin');
    const all = ((g && g.nodes) || []).map(n => (typeof n === 'string' ? n : n && n.id)).filter(Boolean);
    const idSet = new Set(all);
    const edges = ((g && g.edges) || []).filter(e => e && idSet.has(e.source) && idSet.has(e.target));
    if (!all.includes(name)) all.push(name);
    const nodes = Array.from(new Set(all));

    const reach = dir => {
      const seen = new Set([name]);
      let frontier = [name], count = 0;
      while (frontier.length) {
        const next = [];
        frontier.forEach(cur => edges.forEach(e => {
          const nxt = dir === 'up'
            ? (e.target === cur ? e.source : null)
            : (e.source === cur ? e.target : null);
          if (!nxt || seen.has(nxt)) return;
          seen.add(nxt); count++; next.push(nxt);
        }));
        frontier = next;
      }
      return count;
    };
    const up = reach('up'), down = reach('down');

    const head = `<div class="row" style="margin-bottom:10px">
      <span class="hint">上游 ${up} 个，下游 ${down} 个${g && g.truncated ? ' · 已达深度上限' : ''}</span>
      <div class="spacer"></div>
      <a class="btn sm ghost" href="#/lineage">在血缘图中打开</a>
    </div>`;

    if (!edges.length) {
      box.innerHTML = head + UI.empty({
        title: '暂无血缘记录',
        desc: eventsHint(name),
      });
      return;
    }
    box.innerHTML = head + `<div class="dag-wrap"><svg class="dag" id="m-lin-svg"></svg></div>`;
    drawLineage(document.getElementById('m-lin-svg'), { nodes, edges }, name);
  }).catch(e => { body.innerHTML = UI.error(e); });
}

/** 空态里说清「为什么空」，别让人以为页面坏了。 */
function eventsHint(name) {
  const spec = DESIGN.spec || {};
  if ((spec.layer || '').toLowerCase() === 'ods') {
    return 'ODS 层是链路源头，只有接入它的数据源，没有上游模型；下游来自引用它的管道 depends_on。';
  }
  return '发布带 depends_on 的管道后，上下游会自动出现在这里。';
}

function renderModelQuality(body, name) {
  body.innerHTML = `<div id="m-q">${UI.skeleton(5)}</div>`;
  // 质量结果按规则名聚合，没有「规则 → 模型」外键，因此按命名前缀匹配。
  // 这里如实标注，避免让人误以为有强关联。
  api('/quality/overview?limit=300').then(list => {
    const box = body.querySelector('#m-q');
    const mine = (list || []).filter(r => String(r.rule_name || r.ruleName || '').includes(name));
    if (!mine.length) {
      box.innerHTML = UI.empty({ title: '没有关联的质量规则', desc: '按规则名包含模型名的约定匹配，暂无命中' });
      return;
    }
    box.innerHTML = `<div class="table-wrap"><table class="dt">
      <thead><tr><th>规则</th><th>通过</th><th>失败</th><th>通过率</th><th>最近</th></tr></thead>
      <tbody>${mine.map(r => {
        const total = (r.passed || 0) + (r.failed || 0);
        const rate = total ? ((r.passed || 0) * 100 / total).toFixed(1) + '%' : '—';
        return `<tr>
          <td class="mono small">${esc(r.rule_name || r.ruleName)}</td>
          <td class="mono">${r.passed || 0}</td>
          <td class="mono" style="color:#ff7b72">${r.failed || 0}</td>
          <td class="mono">${rate}</td>
          <td class="small dim">${fmtTime(r.last_at || r.lastAt)}</td>
        </tr>`;
      }).join('')}</tbody></table></div>`;
  }).catch(e => { body.innerHTML = UI.error(e); });
}

function renderModelSide(view) {
  const s = DESIGN.spec;
  const side = view.querySelector('#m-side');
  side.innerHTML = `
    <div class="card-title" style="margin-bottom:12px">属性</div>
    <div id="m-form"></div>`;

  // 数据源：填的是数据源名（逻辑别名），由执行器按环境解析成物理连接，
  // 因此候选项来自已接入的数据源，手写容易拼错还看不出。
  const dsNames = (DESIGN.datasources || []).map(d => d.name).filter(Boolean);
  const dsOpts = [{ value: '', label: '（未指定）' }]
    .concat(dsNames.map(n => ({ value: n, label: n })));
  if (s.datasource && !dsNames.includes(s.datasource)) dsOpts.push({ value: s.datasource, label: s.datasource + '（未接入）' });

  // 分区列：候选就是本模型的字段。选一个不存在的列，DDL 必然编译失败，
  // 与其让用户在预览区读报错，不如从源头只给存在的列。
  const colNames = (s.columns || []).map(c => c.name).filter(Boolean);

  // TTL：常用档位直接选；历史值不在档位里时补进去，不静默清空。
  const TTL = [
    { value: '', label: '永久保留' },
    { value: '7d', label: '7 天' },
    { value: '30d', label: '30 天' },
    { value: '90d', label: '90 天' },
    { value: '180d', label: '180 天' },
    { value: '1y', label: '1 年' },
    { value: '3y', label: '3 年' },
  ];
  if (s.ttl && !TTL.some(o => o.value === s.ttl)) TTL.push({ value: s.ttl, label: s.ttl + '（自定义）' });

  const form = SchemaForm(side.querySelector('#m-form'), {
    fields: [
      { key: 'name', label: '模型名', required: true, desc: '必须带层前缀，如 dwd_order' },
      { key: 'layer', label: '分层', type: 'select', required: true,
        options: ['ods', 'dim', 'dwd', 'dws', 'ads'] },
      { key: 'datasource', label: '数据源', type: 'select', options: dsOpts,
        desc: '逻辑别名，由环境决定物理指向' },
      { key: 'description', label: '描述', type: 'textarea', rows: 2 },
      { key: 'partitionBy', label: '分区列', type: 'multiselect', options: colNames,
        emptyText: '先在「字段」tab 添加列', desc: '按选择顺序写入 DDL，首列通常为 dt' },
      { key: 'ttl', label: 'TTL', type: 'select', options: TTL,
        desc: '到期由 retention 策略清理，留空表示永久' },
      { key: 'materialization', label: '物化方式', type: 'select',
        options: [{ value: '', label: '（默认）' }, 'table', 'view', 'mv', 'incremental'] },
      { key: 'allowLossy', label: '允许有损映射', type: 'switch',
        desc: '开启后跨引擎类型可能丢精度' },
    ],
  }, {
    name: s.name, layer: s.layer, datasource: s.datasource || '',
    description: s.description || '',
    partitionBy: Array.isArray(s.partitionBy) ? s.partitionBy : [],
    ttl: s.ttl || '', materialization: s.materialization || '',
    allowLossy: !!s.allowLossy,
  });

  side.querySelectorAll('[data-f]').forEach(el => {
    el.addEventListener('change', () => {
      const v = form.value();
      s.name = v.name; s.layer = v.layer; s.datasource = v.datasource;
      s.description = v.description;
      s.partitionBy = Array.isArray(v.partitionBy)
        ? v.partitionBy
        : String(v.partitionBy || '').split(',').map(x => x.trim()).filter(Boolean);
      s.ttl = v.ttl; s.materialization = v.materialization; s.allowLossy = v.allowLossy;
      markDirty();
      scheduleDDL();
    });
  });
}

function markDirty() {
  DESIGN.dirty = true;
  const el = document.getElementById('m-dirty');
  if (el) el.textContent = '有未保存改动';
}

function scheduleDDL() {
  clearTimeout(DESIGN.ddlTimer);
  DESIGN.ddlTimer = setTimeout(refreshDDL, 320);
}

/** 实时编译 DDL：失败不弹窗，直接把错误写进预览区，边改边看。 */
async function refreshDDL() {
  const host = document.getElementById('m-ddl');
  if (!DESIGN.spec) return;
  try {
    const r = await api('/models/preview', { method: 'POST', body: { spec: DESIGN.spec } });
    DESIGN.ddl = r.ddl || '';
  } catch (e) {
    DESIGN.ddl = '-- 编译失败：' + e.message;
  }
  if (host) {
    host.innerHTML = '';
    SqlEditor(host, { value: DESIGN.ddl, readonly: true, height: 300 });
  }
}

async function checkCurrentModel() {
  if (!DESIGN.spec) return toast('先选一个模型', 'err');
  let r;
  try {
    r = await api('/models/check', { method: 'POST', body: { spec: DESIGN.spec } });
  } catch (e) { return toast(e.message, 'err'); }
  const v = r.violations || [];
  modal('校验结果', `
    ${r.valid ? '<div class="ok-box" style="margin-bottom:12px">校验通过，可以发布</div>'
      : `<div class="err-box" style="margin-bottom:12px">发现 ${v.length} 处问题</div>`}
    ${v.length ? `<div class="table-wrap"><table class="dt">
      <thead><tr><th>规则</th><th>字段</th><th>说明</th></tr></thead>
      <tbody>${v.map(x => `<tr>
        <td class="mono small">${esc(x.rule)}</td>
        <td class="mono small">${esc(x.field || '—')}</td>
        <td class="small wrap">${esc(x.message)}</td>
      </tr>`).join('')}</tbody></table></div>` : ''}
    ${r.diff && (r.diff.changes || []).length ? `
      <div class="card-title" style="margin:16px 0 8px">与线上版本的差异</div>
      <div class="table-wrap"><table class="dt">
        <thead><tr><th>类型</th><th>对象</th><th>风险</th></tr></thead>
        <tbody>${r.diff.changes.map(c => `<tr>
          <td class="mono small">${esc(c.kind)}</td>
          <td class="mono small">${esc(c.column || c.object || '—')}</td>
          <td>${c.breaking ? '<span class="pill err">breaking</span>' : '<span class="pill ok">safe</span>'}</td>
        </tr>`).join('')}</tbody></table></div>` : ''}
  `);
}

async function publishCurrentModel() {
  if (!DESIGN.spec) return toast('先选一个模型', 'err');
  let chk;
  try {
    chk = await api('/models/check', { method: 'POST', body: { spec: DESIGN.spec } });
  } catch (e) { return toast(e.message, 'err'); }
  if (!chk.valid) {
    toast('校验未通过，先修问题再发布', 'err');
    return checkCurrentModel();
  }
  const breaking = ((chk.diff && chk.diff.changes) || []).filter(c => c.breaking);
  const doPublish = async (reason) => {
    // 直接提交 spec：设计器里的对象就是服务端返回的那份，转成 YAML 再解析回来
    // 只会丢掉序列化没覆盖的字段（unique_key / indexes / tags / bucket_by）。
    await api('/models', { method: 'POST', body: { spec: DESIGN.spec, publish: true, force: breaking.length > 0, reason } });
    toast('已发布', 'ok');
    DESIGN.dirty = false;
    await loadModels(document.getElementById('view'));
    openModel(document.getElementById('view'), DESIGN.spec.name);
  };
  if (breaking.length) {
    // breaking 变更：按钮直接禁用「直接发布」，只能走危险确认。
    ConfirmDanger({
      title: '存在 breaking 变更',
      danger: `本次包含 ${breaking.length} 处破坏性变更：${breaking.map(c => c.kind + ' ' + (c.column || c.object || '')).join('；')}。
        发布会重建或改写线上表结构，历史数据可能丢失。`,
      resource: DESIGN.spec.name,
      onOK: doPublish,
    });
    return;
  }
  try { await doPublish('常规发布'); } catch (e) { toast(e.message, 'err'); }
}

function newModelModal(view) {
  // 空表单已被「选来源」向导取代：先决定从哪张表/哪个模型来，再填字段。
  return newModelWizard();
}


/* ============================== 打标签与采集 ============================== */

async function tagModal(type, name) {
  let tags = [];
  try { tags = await api('/tags'); } catch (e) { /* 忽略 */ }
  modal('打标签 · ' + name, `
    <div class="hint" style="margin-bottom:10px">标签需先在设置中创建；owner: 前缀表示责任人。</div>
    <div class="stack" id="tag-list">
      ${(tags || []).map(tg => `<label style="display:flex;align-items:center;gap:8px;margin:0">
        <input type="checkbox" value="${esc(tg.name)}" style="width:auto"> ${esc(tg.name)}
        <span class="mute small">${tg.count} 处使用</span>
      </label>`).join('') || '<div class="hint">还没有标签</div>'}
    </div>
    <div class="row" style="margin-top:14px">
      <input id="tag-new" placeholder="新建标签名">
      <button class="sm ghost" id="tag-create">创建</button>
    </div>
    <div class="row" style="justify-content:flex-end;margin-top:14px">
      <button class="ghost" data-act="close-modal">取消</button>
      <button class="primary" id="tag-save">保存</button>
    </div>`);

  document.getElementById('tag-create').addEventListener('click', async () => {
    const v = val('tag-new');
    if (!v) return;
    try {
      await api('/tags', { method: 'POST', body: { name: v } });
      toast('已创建', 'ok');
      tagModal(type, name);
    } catch (e) { toast(e.message, 'err'); }
  });
  document.getElementById('tag-save').addEventListener('click', async () => {
    const picked = [...document.querySelectorAll('#tag-list input:checked')].map(x => x.value);
    try {
      await api('/tags/attach', { method: 'POST', body: { type, name, tags: picked } });
      closeModal();
      toast('已保存', 'ok');
      render(document.getElementById('view'));
    } catch (e) { toast(e.message, 'err'); }
  });
}

function collectModal() {
  if (!DESIGN.datasources.length) return toast('先去数据源页接入一个数据源', 'err');
  modal('采集元数据', `
    <div class="stack">
      <div class="field"><label>数据源</label>
        <select id="cl-ds">${DESIGN.datasources.map(d => `<option value="${esc(d.name)}">${esc(d.name)}</option>`).join('')}</select>
      </div>
      <div class="hint">采集会比对上次快照，输出新增/删除/类型变更的清单。</div>
    </div>
    <div class="row" style="justify-content:flex-end;margin-top:14px">
      <button class="ghost" data-act="close-modal">取消</button>
      <button class="primary" id="cl-go">开始采集</button>
    </div>`);
  document.getElementById('cl-go').addEventListener('click', async (e) => {
    e.target.disabled = true;
    e.target.textContent = '采集中…';
    try {
      const r = await api('/catalog/collect', { method: 'POST', body: { datasource: val('cl-ds') } });
      closeModal();
      const ch = r.changes || [];
      toast(`采集完成：${r.tables} 张表，${ch.length} 处变更`, 'ok');
      if (ch.length) {
        modal('元数据变更', `<div class="table-wrap"><table class="dt">
          <thead><tr><th>表</th><th>变更</th><th>说明</th></tr></thead>
          <tbody>${ch.map(c => `<tr>
            <td class="mono">${esc(c.table)}</td>
            <td><span class="pill ${c.kind === 'drop_table' || c.kind === 'drop_column' ? 'err' : 'warn'}">${esc(c.kind)}</span></td>
            <td class="small">${esc(c.detail || '')}</td>
          </tr>`).join('')}</tbody></table></div>`);
      }
      render(document.getElementById('view'));
    } catch (err) {
      toast(err.message, 'err');
      e.target.disabled = false;
      e.target.textContent = '开始采集';
    }
  });
}

function piiModal() {
  if (!DESIGN.datasources.length) return toast('先去数据源页接入一个数据源', 'err');
  modal('识别敏感字段', `
    <div class="stack">
      <div class="field"><label>数据源</label>
        <select id="pii-ds">${DESIGN.datasources.map(d => `<option value="${esc(d.name)}">${esc(d.name)}</option>`).join('')}</select>
      </div>
      <div class="field"><label>采样行数</label><input id="pii-limit" type="number" value="200" min="1" max="200"></div>
      <div class="hint">按列名规则 + 采样值正则双重判定。识别不会改数据，采纳才写回标注。</div>
    </div>
    <div id="pii-result" style="margin-top:14px"></div>
    <div class="row" style="justify-content:flex-end;margin-top:14px">
      <button class="ghost" data-act="close-modal">取消</button>
      <button class="primary" id="pii-go">开始识别</button>
    </div>`);
  document.getElementById('pii-go').addEventListener('click', async (e) => {
    e.target.disabled = true;
    e.target.textContent = '识别中…';
    try {
      const r = await api('/pii/detect', {
        method: 'POST',
        body: { datasource: val('pii-ds'), sampleLimit: Number(val('pii-limit')) || 200 },
      });
      const items = r.items || [];
      const box = document.getElementById('pii-result');
      if (!items.length) {
        box.innerHTML = '<div class="ok-box">没有发现敏感字段</div>';
        e.target.textContent = '重新识别';
        e.target.disabled = false;
        return;
      }
      box.innerHTML = `<div class="hint" style="margin-bottom:8px">
          共 ${r.total} 处：高 ${r.byLevel.high} / 中 ${r.byLevel.medium} / 低 ${r.byLevel.low}</div>
        <div class="table-wrap" style="max-height:260px;overflow:auto"><table class="dt">
          <thead><tr><th>表</th><th>级别</th><th>理由</th></tr></thead>
          <tbody>${items.map((f, i) => `<tr>
            <td class="mono small">${esc(f.table)}${f.column ? '.' + esc(f.column) : ''}</td>
            <td><span class="pill ${f.level === 'high' ? 'err' : (f.level === 'medium' ? 'warn' : 'info')}">${esc(f.level)}</span></td>
            <td class="small wrap">${esc(f.reason)}</td>
          </tr>`).join('')}</tbody></table></div>
        <button class="sm primary" id="pii-apply" style="margin-top:10px">采纳全部标注</button>`;
      document.getElementById('pii-apply').addEventListener('click', async () => {
        try {
          const w = await api('/pii/apply', {
            method: 'POST', body: { datasource: val('pii-ds'), findings: items },
          });
          toast(`已标注 ${w.updated} 张表`, 'ok');
          closeModal();
          render(document.getElementById('view'));
        } catch (err) { toast(err.message, 'err'); }
      });
      e.target.textContent = '重新识别';
      e.target.disabled = false;
    } catch (err) {
      toast(err.message, 'err');
      e.target.disabled = false;
      e.target.textContent = '开始识别';
    }
  });
}
