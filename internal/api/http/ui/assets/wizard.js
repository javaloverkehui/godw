/* GoDW 控制台 · 图形化向导
 *
 * 为什么要有这个文件：
 *   模型、管道、指标此前的入口都是「给你一个 YAML 文本框」。对写代码的人这只是麻烦，
 *   对不写代码的人等于没有入口——他既不知道该改哪一行，也不知道改完对不对。
 *   参照 DataWorks 的向导模式、dbt Cloud 的 scaffold、dbt Canvas 的逐节点预览，
 *   这里把同一件事拆成「选 → 看 → 确认」三步，YAML 退到只读的高级视图。
 *
 * 四条硬规则：
 *   1. 能从元数据推断的，绝不让用户回答（名字、类型、主键、分区、写入模式都给默认值）。
 *   2. 每一步都必须有「看得见的东西」：源表真实数据、字段映射、DDL、SQL、DAG。
 *   3. 用户只需要做选择题，输入框只出现在「名字」和「注释」这类真正主观的地方。
 *   4. 走完给下一步入口（建 DWD / 建管道 / 试跑），不把人丢回列表页。
 */
'use strict';

/* ============================== 常量 ============================== */

const WZ_TYPES = ['string', 'text', 'int', 'bigint', 'double', 'decimal', 'bool', 'date', 'datetime', 'json', 'array', 'map'];

// wzTypeOptions 保证「当前值」一定在选择列表里。
// 推断出来的类型可能带精度（decimal(20,4)），不在 12 个逻辑类型里；
// 直接丢进 <select> 会让浏览器回落到第一项 string，界面上显示错误类型，
// 用户一碰这个下拉就把数值列悄悄降级成字符串。
function wzTypeOptions(cur) {
  const list = WZ_TYPES.slice();
  if (cur && !list.includes(cur)) list.unshift(cur);
  return list;
}

const WZ_LAYER = {
  ods: { label: 'ODS', desc: '原样落地，不做业务加工', cls: 'ods' },
  dim: { label: 'DIM', desc: '维度表，一份描述性数据', cls: 'dim' },
  dwd: { label: 'DWD', desc: '清洗后的明细事实', cls: 'dwd' },
  dws: { label: 'DWS', desc: '按主题轻度汇总', cls: 'dws' },
  ads: { label: 'ADS', desc: '面向看板的宽表', cls: 'ads' },
};

const WZ_NEXT_LAYER = { ods: 'dwd', dim: 'dwd', dwd: 'dws', dws: 'ads', ads: 'ads' };

const WZ_AGG_LABEL = {
  sum: '求和', count: '计数', count_distinct: '去重计数',
  avg: '平均', max: '最大值', min: '最小值',
};

const WZ_TTL = [
  ['', '永久保留'],
  ['30d', '30 天'],
  ['90d', '90 天'],
  ['1y', '1 年'],
  ['3y', '3 年'],
];

/* ============================== 小工具 ============================== */

function wzIdentOk(s) {
  return /^[a-z][a-z0-9_]*$/.test(String(s || ''));
}

// qIdent 按引擎加标识符引号。order、user、status 这类保留字不加引号必炸，
// 而全加引号在 MySQL 上又不认双引号——只能按数据源类型分。
function wzQ(engine, name) {
  const e = String(engine || '').toLowerCase();
  if (e === 'mysql' || e === 'clickhouse' || e === 'doris') return '`' + name + '`';
  return '"' + name + '"';
}

function wzFullTable(engine, schema, table) {
  const t = wzQ(engine, table);
  return schema ? wzQ(engine, schema) + '.' + t : t;
}

// wzEngineOf 从已加载的数据源里取引擎类型；未知就按 postgres 处理（最宽松）。
function wzEngineOf(dss, name) {
  const d = (dss || []).find(x => x.name === name);
  return (d && d.type) || 'postgres';
}

function wzLayerPill(layer) {
  const l = WZ_LAYER[layer] || { label: String(layer || '').toUpperCase() };
  return `<span class="pill ${esc(l.cls || 'mute')}">${esc(l.label)}</span>`;
}

/* 分层规范（必填列、是否要求主键）来自后端，前端不复制一份规则。
 * 复制一份的下场是两边改不同步：界面放行、保存被拒，用户只看到一句「校验未通过」。 */
let WZ_RULES = null;
async function wzLayerRules() {
  if (WZ_RULES) return WZ_RULES;
  try {
    const r = await api('/layer-rules');
    WZ_RULES = (r && r.layers) || {};
  } catch (e) {
    WZ_RULES = {};
  }
  return WZ_RULES;
}
function wzRequiredCols(layer) {
  const r = (WZ_RULES || {})[layer];
  return (r && r.requiredColumns) || [];
}

function wzCols(spec) {
  return (spec && spec.columns) || [];
}

function wzDateCol(spec) {
  const cols = wzCols(spec);
  if (spec && (spec.partitionBy || []).length) return spec.partitionBy[0];
  const d = cols.find(c => c.type === 'date' || c.type === 'datetime');
  return d ? d.name : '';
}

function wzGuessWriteMode(spec) {
  if ((spec.partitionBy || []).length) return 'overwrite_partition';
  if ((spec.uniqueKey || []).length) return 'upsert';
  if ((spec.primaryKey || []).length) return 'upsert';
  return 'append';
}

function wzWriteModeLabel(m) {
  return {
    append: '追加', overwrite: '整表覆盖', upsert: '按主键合并',
    overwrite_partition: '按分区覆盖', delete_insert: '先删后插',
  }[m] || m;
}

// wzCron 把「人话」翻译成 cron。非技术用户不该被迫记住 5 段表达式。
function wzCron(kind, hour, minute, weekday) {
  const h = Number(hour), m = Number(minute);
  switch (kind) {
    case 'manual': return '';
    case 'hourly': return `${m} * * * *`;
    case 'daily': return `${m} ${h} * * *`;
    case 'weekly': return `${m} ${h} * * ${weekday}`;
    case 'monthly': return `${m} ${h} 1 * *`;
    default: return '';
  }
}

function wzCronText(cron) {
  if (!cron) return '手动触发';
  const p = String(cron).trim().split(/\s+/);
  if (p.length !== 5) return cron;
  const [mi, h, dom, , dow] = p;
  const pad = n => String(n).padStart(2, '0');
  if (mi !== '*' && h === '*' && dom === '*') return `每小时第 ${mi} 分钟`;
  if (/^\d+$/.test(mi) && /^\d+$/.test(h) && dom === '*' && dow === '*') return `每天 ${pad(h)}:${pad(mi)}`;
  if (/^\d+$/.test(mi) && /^\d+$/.test(h) && dom === '*' && /^\d$/.test(dow)) {
    return `每周${'日一二三四五六'[Number(dow)]} ${pad(h)}:${pad(mi)}`;
  }
  if (/^\d+$/.test(mi) && /^\d+$/.test(h) && dom === '1') return `每月 1 日 ${pad(h)}:${pad(mi)}`;
  return cron;
}

function wzYmd(d) {
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

/* ============================== 向导骨架 ============================== */

/**
 * wizOpen 打开一个多步向导。
 *
 * opt = {
 *   title, sub,          标题与副标题
 *   steps: [{ label, hint, render(bodyEl, st), next(st) -> bool|Promise<bool> }]
 *   state,               跨步骤共享的状态（初始值）
 *   finishText,          最后一步主按钮文案
 *   finish(st) -> any    提交动作；抛错则停在当前步并提示
 *   done(result, st)     成功后替换正文的成功页（返回 HTML 字符串）
 * }
 *
 * 返回 { repaint(), state } —— 异步数据回来后可主动重画当前步。
 */
function wizOpen(opt) {
  const root = document.getElementById('modal-root');
  const st = opt.state || {};
  const steps = opt.steps || [];
  let idx = 0;
  let busy = false;
  let paintSeq = 0;

  root.innerHTML = `<div class="modal-mask" data-act="mask-close">
    <div class="modal wiz" role="dialog" aria-modal="true" tabindex="-1" aria-label="${esc(opt.title)}">
      <div class="wiz-head">
        <div class="wiz-titlebar">
          <div>
            <h3>${esc(opt.title)}</h3>
            ${opt.sub ? `<div class="hint">${esc(opt.sub)}</div>` : ''}
          </div>
          <button class="sm ghost" data-act="close-modal" title="关闭（Esc）">关闭</button>
        </div>
        <div class="wiz-steps" id="wiz-steps"></div>
      </div>
      <div class="wiz-body" id="wiz-body"></div>
      <div class="wiz-foot">
        <div class="hint" id="wiz-hint" style="flex:1;min-width:0"></div>
        <button class="ghost" id="wiz-prev">上一步</button>
        <button class="primary" id="wiz-next">下一步</button>
      </div>
    </div>
  </div>`;

  const bodyEl = document.getElementById('wiz-body');
  const stepsEl = document.getElementById('wiz-steps');
  const hintEl = document.getElementById('wiz-hint');
  const prevBtn = document.getElementById('wiz-prev');
  const nextBtn = document.getElementById('wiz-next');

  function paintSteps(done) {
    stepsEl.innerHTML = steps.map((s, i) => {
      const state = done || i < idx ? 'done' : i === idx ? 'on' : '';
      return `<div class="wiz-step ${state}">
        <span class="wiz-step-n">${done || i < idx ? '✓' : i + 1}</span>
        <span class="wiz-step-t">${esc(s.label)}</span>
      </div>`;
    }).join('<span class="wiz-step-line"></span>');
  }

  async function paint() {
    const seq = ++paintSeq;
    const s = steps[idx];
    paintSteps();
    bodyEl.scrollTop = 0;
    bodyEl.innerHTML = '<div class="wiz-loading">加载中…</div>';
    hintEl.innerHTML = esc(s.hint || '');
    prevBtn.disabled = idx === 0;
    nextBtn.textContent = idx === steps.length - 1 ? (opt.finishText || '完成') : '下一步';
    nextBtn.disabled = false;
    try {
      await s.render(bodyEl, st);
    } catch (e) {
      if (seq === paintSeq) bodyEl.innerHTML = `<div class="err-box">${esc(e.message || e)}</div>`;
    }
  }

  prevBtn.addEventListener('click', () => {
    if (idx === 0 || busy) return;
    idx--;
    paint();
  });

  nextBtn.addEventListener('click', async () => {
    if (busy) return;
    const s = steps[idx];
    busy = true;
    nextBtn.disabled = true;
    const old = nextBtn.textContent;
    try {
      if (s.next) {
        const ok = await s.next(st);
        if (ok === false) return;
      }
      if (idx < steps.length - 1) {
        idx++;
        await paint();
        return;
      }
      nextBtn.textContent = '提交中…';
      const res = await (opt.finish ? opt.finish(st) : null);
      if (opt.done) {
        paintSteps(true);
        bodyEl.innerHTML = opt.done(res, st);
        hintEl.innerHTML = '';
        prevBtn.style.display = 'none';
        nextBtn.style.display = 'none';
        if (opt.bindDone) opt.bindDone(bodyEl, res, st);
      } else {
        closeModal();
      }
    } catch (e) {
      toast(e.message || String(e), 'err');
    } finally {
      busy = false;
      if (nextBtn.isConnected) {
        nextBtn.disabled = false;
        if (nextBtn.textContent === '提交中…') nextBtn.textContent = old;
      }
    }
  });

  paint();
  return { repaint: paint, state: st };
}

/* 字段行：向导里所有「列级」信息都走这个结构，保证三处视觉一致 */
function wzChip(text, cls) {
  return `<span class="wz-chip ${cls || ''}">${esc(text)}</span>`;
}

/* ============================== 零、新建模型入口 ============================== */

/**
 * newModelWizard 是「新建模型」的总入口：先问数据从哪来，再进对应的向导。
 *
 * 之前这个按钮直接弹一个空表单（名字、分层、数据源、描述），
 * 建完得到一个只有 dt/id 两列的空壳——真正的工作还在后面。
 * 现在先选来源：源表 → ODS 向导；已有模型 → 派生向导；都不合适才落空白。
 */
async function newModelWizard() {
  let tables = DESIGN.tables || [];
  if (!tables.length) {
    try {
      const [ds, dss] = await Promise.all([api('/datasets?pageSize=200'), api('/datasources')]);
      tables = (ds && ds.items) || [];
      DESIGN.tables = tables;
      DESIGN.datasources = dss || [];
      DESIGN.dsMap = {};
      (dss || []).forEach(d => { DESIGN.dsMap[d.id] = d.name; });
    } catch (e) { tables = []; }
  }
  const models = (typeof DESIGN !== 'undefined' && DESIGN.models) || [];

  const st = { mode: '', tableId: '', modelName: '', showSys: false };

  wizOpen({
    title: '新建模型',
    sub: '先选数据从哪来，字段与 SQL 由向导带出来',
    state: st,
    finishText: '开始',
    steps: [{
      label: '选择起点',
      hint: '绝大多数情况选前两项；手写定义是给已经想清楚结构的人',
      render: (body, s) => {
        const render = () => {
          const pickTable = s.mode === 'table';
          const pickModel = s.mode === 'model';
          const kw = (body.querySelector('#wz-nm-q') || {}).value || '';
          // 系统表（平台自己的元数据表）默认不出现在候选里：
          // 建 ODS 时翻过 43 张 audit_log/alert_event 才找到业务表，是纯粹的干扰。
          const shownTables = tables.filter(t => {
            if (!s.showSys && typeof isSystemTable === 'function' && isSystemTable(t)) return false;
            return !kw || t.name.toLowerCase().includes(kw.toLowerCase());
          });
          body.innerHTML = `
            <div class="wiz-choice-row">
              <div class="wiz-choice ${pickTable ? 'sel' : ''}" data-mode="table">
                <div class="wiz-choice-t">从源表接入</div>
                <div class="wiz-choice-d">把已采集的业务表原样落成 ODS</div>
              </div>
              <div class="wiz-choice ${pickModel ? 'sel' : ''}" data-mode="model">
                <div class="wiz-choice-t">从上游模型派生</div>
                <div class="wiz-choice-d">基于已有模型做清洗、汇总，生成 DWD/DWS/ADS</div>
              </div>
              <div class="wiz-choice ${s.mode === 'blank' ? 'sel' : ''}" data-mode="blank">
                <div class="wiz-choice-t">手写定义</div>
                <div class="wiz-choice-d">直接编辑 YAML，适合从别处导入</div>
              </div>
            </div>
            ${pickTable ? `
              <div class="wiz-bar" style="margin-top:16px">
                <input id="wz-nm-q" placeholder="搜索表名" value="${esc(kw)}" style="max-width:240px">
                <label class="wiz-pk ${s.showSys ? 'sel' : ''}" style="margin:0">
                  <input type="checkbox" id="wz-nm-sys" ${s.showSys ? 'checked' : ''}>显示系统表
                </label>
                <span class="hint">${shownTables.length} 张可选</span>
              </div>
              <div class="wiz-pick">
                ${shownTables.slice(0, 200).map(t => `
                  <label class="wiz-pick-item ${s.tableId === t.id ? 'sel' : ''}">
                    <input type="radio" name="wz-nm-t" value="${esc(t.id)}" ${s.tableId === t.id ? 'checked' : ''}>
                    <span class="mono">${esc(t.schema ? t.schema + '.' : '')}${esc(t.name)}</span>
                    <span class="hint">${(t.columns || []).length} 字段 · ${(t.rowCount || 0).toLocaleString()} 行</span>
                  </label>`).join('') || '<div class="empty">没有匹配的表，先去「表结构」页采集元数据</div>'}
              </div>` : ''}
            ${pickModel ? `
              <div class="wiz-bar" style="margin-top:16px"><span class="hint">选一个上游模型，下一步可以改分层和字段</span></div>
              <div class="wiz-pick">
                ${models.length ? models.map(m => `
                  <label class="wiz-pick-item ${s.modelName === m.name ? 'sel' : ''}">
                    <input type="radio" name="wz-nm-m" value="${esc(m.name)}" ${s.modelName === m.name ? 'checked' : ''}>
                    ${wzLayerPill(m.layer)}
                    <span class="mono">${esc(m.name)}</span>
                    <span class="hint">${wzCols(m.spec).length} 列 · v${m.activeVersion || 0}</span>
                  </label>`).join('') : '<div class="empty">还没有模型，先从源表接入一个 ODS</div>'}
              </div>` : ''}
            ${s.mode === 'blank' ? `<div class="wiz-note">
              YAML 里要写清 kind/name/layer/datasource 和每一列，写错会在发布时被拦下。
              不确定就回到上面两项。
            </div>` : ''}`;

          body.querySelectorAll('[data-mode]').forEach(el => el.addEventListener('click', () => {
            s.mode = el.dataset.mode;
            render();
          }));
          const q = body.querySelector('#wz-nm-q');
          if (q) {
            q.addEventListener('input', () => {
              const pos = q.selectionStart;
              render();
              const q2 = body.querySelector('#wz-nm-q');
              if (q2) { q2.focus(); q2.setSelectionRange(pos, pos); }
            });
          }
          const sys = body.querySelector('#wz-nm-sys');
          if (sys) sys.addEventListener('change', () => { s.showSys = sys.checked; render(); });
          body.querySelectorAll('input[name="wz-nm-t"]').forEach(el => el.addEventListener('change', () => { s.tableId = el.value; }));
          body.querySelectorAll('input[name="wz-nm-m"]').forEach(el => el.addEventListener('change', () => { s.modelName = el.value; }));
        };
        render();
      },
      next: (s) => {
        if (!s.mode) { toast('先选一种起点', 'err'); return false; }
        if (s.mode === 'table' && !s.tableId) { toast('选一张表', 'err'); return false; }
        if (s.mode === 'model' && !s.modelName) { toast('选一个上游模型', 'err'); return false; }
        return true;
      },
    }],
    finish: async (s) => {
      // 这一步只负责收集选择权：真正要打开的是另一个向导，所以先关自己再开下一个，
      // 否则两个宽弹窗会叠在一起。
      closeModal();
      const next = () => {
        if (s.mode === 'table') {
          const t = (DESIGN.tables || []).find(x => x.id === s.tableId);
          if (t) odsWizard(t);
          return;
        }
        if (s.mode === 'model') { deriveWizard(s.modelName); return; }
        modelEditor('');
      };
      setTimeout(next, 0);
      return s;
    },
  });
}

/* ============================== 一、ODS 接入向导 ============================== */

/**
 * odsWizard 把一张已采集的源表做成 ODS 模型。
 *
 * 旧实现把 scaffold 生成的 YAML 塞进 textarea 就完事了，用户面对 40 行配置无从下手。
 * 这里拆成：看数据 → 挑字段 → 定分区。三步都是选择题，名字已按规范填好。
 */
async function odsWizard(table) {
  if (!table) return;

  let res;
  const dsName = (DESIGN.dsMap && DESIGN.dsMap[table.datasourceId]) || '';
  await wzLayerRules();
  const required = wzRequiredCols('ods');
  try {
    res = await api('/models/scaffold', {
      method: 'POST',
      body: {
        Layer: 'ods',
        Name: 'ods_' + table.name.replace(/[^A-Za-z0-9_]/g, '_'),
        SourceName: table.name,
        Datasource: dsName,
        Schema: table.schema,
        Upstream: table.columns || [],
      },
    });
  } catch (e) {
    toast(e.message, 'err');
    return;
  }


  const spec = res.spec || {};
  const specByCol = {};
  wzCols(spec).forEach(c => { specByCol[c.name] = c; });
  const srcByCol = {};
  (table.columns || []).forEach(c => { srcByCol[String(c.name).replace(/[^A-Za-z0-9_]/g, '_')] = c; });

  const st = {
    table,
    dsName,
    spec,
    warnings: res.warnings || [],
    keyCandidates: res.keyCandidates || [],
    // cols 以 scaffold 的推断结果为准，而不是源表的列清单：
    // 脚手架会补一个分区日期列（源表没有日期列时），它必须可勾选、可选为分区，
    // 否则「分区」下拉里永远选不到它。srcName 为空即表示这一列是平台补的。
    cols: wzCols(spec).filter(c => !/^_godw_/.test(c.name)).map(c => {
      const src = srcByCol[c.name];
      return {
        name: c.name,
        srcName: src ? src.name : '',
        srcType: src ? src.type : '',
        type: c.type,
        nullable: c.nullable,
        comment: c.comment || '',
        pii: (src && src.pii) || '',
        req: required.includes(c.name),
        on: true,
      };
    }),
    // 审计列固定追加，不给勾选——它们不是业务决策，是平台契约
    extraCols: wzCols(spec).filter(c => /^_godw_/.test(c.name)),
    // inferred 保存脚手架的原判，供「恢复推断值」一键撤回手工改动
    inferred: {},
    keepExtra: true,
    rows: null,
    rowErr: '',
    ddl: '',
    ddlErr: '',
    ddlTimer: null,
    schedule: 'daily',
    hour: '02',
    minute: '00',
    weekday: '1',
    // 目标数据源与源表不同才默认复制一份：同库接入时表已经在原地了
    createPipe: (spec.datasource || '') !== dsName,
  };
  // PII 是按表判定的，逐列标出来是为了让人在「全选」前看到风险
  (table.columns || []).forEach(sc => {
    const hit = st.cols.find(c => c.srcName === sc.name);
    if (hit && sc.pii) hit.pii = sc.pii;
  });
  st.cols.forEach(c => { st.inferred[c.name] = { type: c.type, nullable: c.nullable, comment: c.comment }; });

  const wiz = wizOpen({
    title: '接入 ODS 模型',
    sub: `${table.schema ? table.schema + '.' : ''}${table.name} → 原样落地到 ODS 层`,
    state: st,
    finishText: '创建 ODS 模型',
    steps: [
      /* ---- 第一步：确认来源与目标 ---- */
      {
        label: '来源与目标',
        hint: '名字已按 ods_ 前缀填好，可直接下一步',
        render: async (body, s) => {
          body.innerHTML = `
            <div class="wiz-split">
              <div class="wiz-pane">
                <div class="wiz-pane-title">源表</div>
                <div class="wiz-src-card">
                  <div class="wiz-src-name mono">${esc(s.table.schema ? s.table.schema + '.' : '')}${esc(s.table.name)}</div>
                  <div class="wiz-src-meta">
                    ${wzChip(s.dsName || '默认数据源')}
                    ${wzChip((s.table.rowCount || 0).toLocaleString() + ' 行')}
                    ${wzChip((s.table.columns || []).length + ' 字段')}
                    ${s.table.pii && s.table.pii !== 'none' ? wzChip('敏感级别 ' + s.table.pii, 'warn') : ''}
                  </div>
                  <div class="hint" style="margin-top:6px">${esc(s.table.comment || '源表无注释')}</div>
                </div>
                <div class="wiz-pane-title" style="margin-top:14px">前 8 行真实数据
                  <span class="hint" style="font-weight:400">（从这里确认选对了表）</span></div>
                <div id="wz-preview" class="wiz-preview">${s.rows || s.rowErr ? wzPreviewHTML(s) : '<div class="wiz-loading">读取中…</div>'}</div>
              </div>
              <div class="wiz-pane">
                <div class="wiz-pane-title">目标模型</div>
                <label class="field"><span>模型名</span>
                  <input id="wz-name" class="mono" value="${esc(s.spec.name)}" />
                  <em class="hint" id="wz-name-ok"></em>
                </label>
                <label class="field"><span>写入数据源</span>
                  <select id="wz-ds">${(DESIGN.datasources || []).map(d =>
                    `<option value="${esc(d.name)}" ${d.name === s.spec.datasource ? 'selected' : ''}>${esc(d.name)} · ${esc(d.type)}</option>`).join('') ||
                    `<option value="${esc(s.spec.datasource)}">${esc(s.spec.datasource)}</option>`}</select>
                </label>
                <label class="field"><span>库（schema）</span>
                  <input id="wz-schema" class="mono" value="${esc(s.spec.schema || 'ods')}" />
                </label>
                <label class="field"><span>描述（可选）</span>
                  <input id="wz-desc" value="${esc(s.table.comment || (s.table.name + ' 原始表'))}" />
                </label>
                ${s.warnings.length ? `<div class="wiz-note warn">
                  <b>推断时发现 ${s.warnings.length} 处需要留意</b>
                  <ul>${s.warnings.slice(0, 4).map(x => `<li>${esc(x)}</li>`).join('')}</ul>
                </div>` : ''}
                <div class="wiz-note">
                  ODS 层原样保留源数据：不设主键、不做类型转换，重复行也会照原样进来。
                  去重和清洗放在下一步的 DWD。
                </div>
              </div>
            </div>`;

          const nameEl = body.querySelector('#wz-name');
          const okEl = body.querySelector('#wz-name-ok');
          const checkName = () => {
            const v = nameEl.value.trim();
            s.spec.name = v;
            if (!wzIdentOk(v)) { okEl.textContent = '只能用小写字母、数字、下划线，且以字母开头'; okEl.className = 'hint err'; return; }
            if (!v.startsWith('ods_')) { okEl.textContent = '建议以 ods_ 开头（层前缀是硬校验）'; okEl.className = 'hint warn'; return; }
            okEl.textContent = '名称可用';
            okEl.className = 'hint ok';
          };
          nameEl.addEventListener('input', checkName);
          checkName();
          body.querySelector('#wz-schema').addEventListener('input', ev => { s.spec.schema = ev.target.value.trim(); });
          body.querySelector('#wz-ds').addEventListener('change', ev => { s.spec.datasource = ev.target.value; });
          body.querySelector('#wz-desc').addEventListener('input', ev => { s.spec.description = ev.target.value.trim(); });
          s.spec.description = s.spec.description || s.table.comment || (s.table.name + ' 原始表');

          if (!s.rows && !s.rowErr) {
            try {
              s.rows = await api('/table/rows?' + new URLSearchParams({
                datasource: s.dsName, schema: s.table.schema || '', table: s.table.name, limit: 8,
              }));
            } catch (e) {
              s.rowErr = e.message;
            }
            const box = body.querySelector('#wz-preview');
            if (box) box.innerHTML = wzPreviewHTML(s);
          }
        },
        next: (s) => {
          if (!wzIdentOk(s.spec.name)) { toast('模型名不合法', 'err'); return false; }
          if (!s.spec.name.startsWith('ods_')) { toast('ODS 模型名要以 ods_ 开头', 'err'); return false; }
          return true;
        },
      },

      /* ---- 第二步：字段映射 ---- */
      {
        label: '字段',
        hint: '默认全选，取消勾选即可不落这一列',
        render: (body, s) => {
          const renderTable = () => {
            const on = s.cols.filter(c => c.on).length;
            body.innerHTML = `
              <div class="wiz-bar">
                <button class="sm ghost" data-wz="all">全选</button>
                <button class="sm ghost" data-wz="none">全不选</button>
                <button class="sm ghost" data-wz="reset">恢复推断值</button>
                <span class="hint">已选 <b class="mono">${on}</b> / ${s.cols.length} 列，另加 ${s.extraCols.length} 个审计列</span>
              </div>
              <div class="wiz-map">
                <table class="dt">
                  <thead><tr>
                    <th style="width:34px"></th><th>字段</th><th style="width:110px">源类型</th>
                    <th style="width:140px">落库类型</th><th style="width:52px">可空</th><th>注释</th>
                  </tr></thead>
                  <tbody>${s.cols.map((c, i) => `<tr class="${c.on ? '' : 'wz-off'}">
                    <td><input type="checkbox" data-on="${i}" ${c.on ? 'checked' : ''} ${c.req ? 'disabled title="该层必填列，不能去掉"' : ''}></td>
                    <td class="mono"><b>${esc(c.name)}</b>${c.req ? ' ' + wzChip('必填', 'lock') : ''}${c.pii && c.pii !== 'none' ? ' ' + wzChip('PII', 'warn') : ''}${c.srcName ? '' : '<div class="mute small">平台补的分区列</div>'}</td>
                    <td class="mono small dim">${esc(c.srcType || '—')}</td>
                    <td><select data-type="${i}">${wzTypeOptions(c.type).map(t =>
                      `<option value="${esc(t)}" ${t === c.type ? 'selected' : ''}>${esc(t)}</option>`).join('')}</select></td>
                    <td class="center"><input type="checkbox" data-null="${i}" ${c.nullable ? 'checked' : ''}></td>
                    <td><input data-comment="${i}" value="${esc(c.comment)}" placeholder="—"></td>
                  </tr>`).join('')}</tbody>
                </table>
              </div>
              ${s.extraCols.length ? `<div class="wiz-note">
                <b>平台自动追加的审计列</b>：${s.extraCols.map(c => `<span class="mono">${esc(c.name)}</span>`).join('、')}
                —— 用于标记来源和入库时间，不用手填，也不占勾选名额。
              </div>` : ''}`;
            body.querySelector('[data-wz="all"]').addEventListener('click', () => { s.cols.forEach(c => c.on = true); renderTable(); });
            body.querySelector('[data-wz="none"]').addEventListener('click', () => {
              // 必填列不接受「全不选」：它去掉之后一定保存失败，不如当场就不给这个选项
              s.cols.forEach(c => { if (!c.req) c.on = false; });
              renderTable();
            });
            body.querySelector('[data-wz="reset"]').addEventListener('click', () => {
              s.cols.forEach(c => {
                const init = s.inferred[c.name];
                c.on = true;
                c.nullable = init ? init.nullable : c.nullable;
                c.type = init ? init.type : c.type;
                c.comment = init ? init.comment : c.comment;
              });
              renderTable();
            });
            body.querySelectorAll('[data-on]').forEach(el => el.addEventListener('change', () => {
              s.cols[Number(el.dataset.on)].on = el.checked;
              el.closest('tr').classList.toggle('wz-off', !el.checked);
              const n = s.cols.filter(c => c.on).length;
              const badge = body.querySelector('.wiz-bar .hint b');
              if (badge) badge.textContent = n;
            }));
            body.querySelectorAll('[data-type]').forEach(el => el.addEventListener('change', () => {
              s.cols[Number(el.dataset.type)].type = el.value;
            }));
            body.querySelectorAll('[data-null]').forEach(el => el.addEventListener('change', () => {
              s.cols[Number(el.dataset.null)].nullable = el.checked;
            }));
            body.querySelectorAll('[data-comment]').forEach(el => el.addEventListener('input', () => {
              s.cols[Number(el.dataset.comment)].comment = el.value.trim();
            }));
          };
          renderTable();
        },
        next: (s) => {
          if (!s.cols.some(c => c.on)) { toast('至少保留一列', 'err'); return false; }
          return true;
        },
      },

      /* ---- 第三步：物理属性 + DDL ---- */
      {
        label: '分区与预览',
        hint: '不填也能用：默认按日期列分区、永久保留',
        render: (body, s) => {
          const dateCols = s.cols.filter(c => c.on && (c.type === 'date' || c.type === 'datetime')).map(c => c.name);
          const part = (s.spec.partitionBy || [])[0] || dateCols[0] || '';
          // 没有分区列时不能默认「按分区覆盖」：那等于整表反复追加，
          // 第二天数据就翻倍。这种坑不该让人踩过一次才懂。
          const defWrite = part ? 'overwrite_partition' : 'overwrite';
          s.writeMode = s.writeMode || defWrite;
          body.innerHTML = `
            <div class="wiz-split" style="grid-template-columns:minmax(0,1fr) minmax(0,1.1fr)">
              <div class="wiz-pane">
                <div class="wiz-pane-title">物理属性</div>
                <label class="field"><span>分区列</span>
                  <select id="wz-part">
                    <option value="">不分区（整表写入）</option>
                    ${s.cols.filter(c => c.on).map(c => `<option value="${esc(c.name)}" ${c.name === part ? 'selected' : ''}>${esc(c.name)}</option>`).join('')}
                  </select>
                </label>
                <label class="field"><span>数据保留</span>
                  <select id="wz-ttl">${WZ_TTL.map(([v, t]) => `<option value="${v}" ${v === (s.spec.ttl || '') ? 'selected' : ''}>${t}</option>`).join('')}</select>
                </label>
                <label class="field"><span>写入方式</span>
                  <select id="wz-write">
                    <option value="overwrite_partition" ${s.writeMode === 'overwrite_partition' ? 'selected' : ''}>按分区覆盖（可重跑，推荐）</option>
                    <option value="overwrite" ${s.writeMode === 'overwrite' ? 'selected' : ''}>整表覆盖</option>
                    <option value="append" ${s.writeMode === 'append' ? 'selected' : ''}>追加（只增不改）</option>
                  </select>
                </label>
                <label class="field" style="gap:6px">
                  <span>顺便建一条同步管道</span>
                  <span class="row" style="gap:8px;margin:0">
                    <input type="checkbox" id="wz-pipe" ${s.createPipe ? 'checked' : ''} style="width:auto">
                    <span class="hint">每天 02:00 把源表数据写进这张 ODS 表</span>
                  </span>
                </label>
                <div class="wiz-note" id="wz-pipe-note">${s.createPipe
                  ? '管道会按分区重写当天的数据，重跑不会产生重复行。'
                  : '同库接入时表就在原地，通常不需要再复制一份；跨库同步才需要打开。'}</div>
              </div>
              <div class="wiz-pane">
                <div class="wiz-pane-title">将执行的建表语句</div>
                <pre class="code wiz-ddl" id="wz-ddl">编译中…</pre>
              </div>
            </div>`;

          const sync = () => {
            s.spec.partitionBy = body.querySelector('#wz-part').value ? [body.querySelector('#wz-part').value] : [];
            s.spec.ttl = body.querySelector('#wz-ttl').value;
            s.writeMode = body.querySelector('#wz-write').value;
            s.createPipe = body.querySelector('#wz-pipe').checked;
            clearTimeout(s.ddlTimer);
            s.ddlTimer = setTimeout(refreshDDL, 200);
          };
          const refreshDDL = async () => {
            const pre = body.querySelector('#wz-ddl');
            if (!pre) return;
            try {
              const r = await api('/models/preview', { method: 'POST', body: { spec: wzSpecOf(s) } });
              pre.textContent = r.ddl || '（无 DDL）';
            } catch (e) {
              pre.textContent = '编译失败：' + e.message;
            }
          };
          ['#wz-part', '#wz-ttl', '#wz-write', '#wz-pipe'].forEach(sel =>
            body.querySelector(sel).addEventListener('change', sync));
          sync();
        },
      },
    ],

    finish: async (s) => {
      const spec = wzSpecOf(s);
      const m = await api('/models', { method: 'POST', body: { spec, publish: true } });
      // ODS 表本身由采集写入；「顺便建管道」指的是把源表同步进这张表的那条链路
      if (s.createPipe) {
        try {
          await api('/pipelines', {
            method: 'POST', body: {
              spec: wzOdsPipeSpec(s, m),
              publish: true,
            },
          });
        } catch (e) {
          toast('模型已建好，但管道创建失败：' + e.message, 'err');
        }
      }
      return m;
    },

    done: (m, s) => `
      <div class="wiz-done">
        <div class="wiz-done-icon">✓</div>
        <h4>${esc(m.name)} 已创建并发布 v${m.activeVersion || 1}</h4>
        <div class="hint">${s.cols.filter(c => c.on).length} 列 · ${(m.spec.partitionBy || []).length ? '按 ' + esc(m.spec.partitionBy[0]) + ' 分区' : '不分区'}${s.createPipe ? ' · 已生成同步管道' : ''}</div>
        <div class="wiz-done-next">
          <div class="wiz-pane-title">接下来</div>
          <div class="row wrap" style="gap:8px">
            <button class="primary" data-wz-next="dwd">继续做 DWD 明细</button>
            <button class="ghost" data-wz-next="design">打开设计器</button>
            <button class="ghost" data-wz-next="pipe">为它建管道</button>
          </div>
          <div class="hint" style="margin-top:10px">DWD 会从这张 ODS 表出发，自动带好列和 SQL 模板。</div>
        </div>
      </div>`,

    bindDone: (body, m, s) => {
      const go2 = (fn) => body.querySelectorAll('[data-wz-next]').forEach(b =>
        b.addEventListener('click', () => { closeModal(); fn(b.dataset.wzNext, m, s); }));
      go2((kind, model) => {
        if (kind === 'dwd') return deriveWizard(model.name);
        if (kind === 'pipe') return pipelineWizard(model.name);
        sessionStorage.setItem('godw.openModel', model.name);
        go('models');
      });
    },
  });
  return wiz;
}

// wzSpecOf 把「界面上的勾选状态」拼成后端认识的 ModelSpec。
// 只在这里做一次转换，避免每个向导各写一份拼装逻辑。
function wzSpecOf(s) {
  const cols = s.cols.filter(c => c.on).map(c => ({
    name: c.name, type: c.type, nullable: c.nullable, comment: c.comment || undefined,
  }));
  if (s.keepExtra !== false) {
    (s.extraCols || []).forEach(c => cols.push({
      name: c.name, type: c.type, nullable: c.nullable, comment: c.comment || undefined,
    }));
  }
  const names = new Set(cols.map(c => c.name));
  return Object.assign({}, s.spec, {
    columns: cols,
    primaryKey: (s.spec.primaryKey || []).filter(k => names.has(k)),
    uniqueKey: (s.spec.uniqueKey || []).filter(k => names.has(k)),
    partitionBy: (s.spec.partitionBy || []).filter(k => names.has(k)),
  });
}

/**
 * wzOdsPipeSpec 生成「把源表搬进 ODS 表」的同步管道。
 *
 * 关键点：ODS 模型里必然包含源表没有的列——dt（分区日期）和两个审计列
 * （_godw_src / _godw_load_at）。直接 `SELECT <模型全部列>` 会引用不存在的列，
 * 一跑就报错。所以这里按「源列照搬、平台列就地生成」来拼：
 *   - 源表有的列：原样选出（改名过的加 AS）
 *   - dt：写成 ${biz_date}，即本次运行的业务日期
 *   - _godw_src：库.表 的字面量，用于追溯来源
 *   - _godw_load_at：now()
 * 分区条件只在源表真的有这一列时才加，否则整表写入当天分区。
 */
function wzOdsPipeSpec(s, m) {
  const eng = wzEngineOf(DESIGN.datasources, s.spec.datasource);
  const srcByName = {};
  s.cols.filter(c => c.on).forEach(c => { srcByName[c.name] = c.srcName || ''; });

  const selects = wzCols(m.spec).map(c => {
    const src = srcByName[c.name];
    if (src) {
      const col = wzQ(eng, src);
      return src === c.name ? col : `${col} AS ${wzQ(eng, c.name)}`;
    }
    if (c.name === 'dt') return `\${biz_date} AS ${wzQ(eng, 'dt')}`;
    if (c.name === '_godw_src') {
      const full = (s.table.schema ? s.table.schema + '.' : '') + s.table.name;
      return `'${full.replace(/'/g, "''")}' AS ${wzQ(eng, c.name)}`;
    }
    if (c.name === '_godw_load_at') return `now() AS ${wzQ(eng, c.name)}`;
    return `NULL AS ${wzQ(eng, c.name)}`;
  });

  const partCol = (m.spec.partitionBy || [])[0] || '';
  const partInSrc = partCol ? srcByName[partCol] : '';
  const where = partInSrc ? `\nWHERE ${wzQ(eng, partInSrc)} = \${biz_date}` : '';

  return {
    kind: 'Pipeline',
    name: (m.name + '_sync').replace(/^ods_/, ''),
    description: `把 ${s.table.name} 同步到 ${m.name}`,
    schedule: '0 2 * * *',
    timezone: 'Asia/Shanghai',
    params: [{ name: 'biz_date', type: 'date', required: true, description: '业务日期，默认 T-1' }],
    nodes: [{
      id: m.name,
      type: 'sql',
      datasource: s.spec.datasource,
      model: m.name,
      writeMode: s.writeMode || 'overwrite_partition',
      sql: `SELECT ${selects.join(',\n       ')}\nFROM ${wzFullTable(eng, s.spec.schema, s.table.name)}${where}`,
    }],
  };
}

function wzPreviewHTML(s) {
  if (s.rowErr) return `<div class="wiz-note warn">读不到数据：${esc(s.rowErr)}<br/>可能没有查询权限，或表刚接入还没同步。不影响建模。</div>`;
  const r = s.rows || {};
  const cols = r.columns || [];
  const rows = (r.rows || []).slice(0, 8);
  if (!cols.length) return `<div class="wiz-note">表里还没有数据。</div>`;
  const masked = Object.keys(r.masks || {}).length;
  return `
    <div class="wiz-preview-scroll"><table class="dt compact">
      <thead><tr>${cols.map(c => `<th class="mono">${esc(c)}</th>`).join('')}</tr></thead>
      <tbody>${rows.map(row => `<tr>${row.map(v => `<td class="mono small">${v == null ? '<span class="mute">NULL</span>' : esc(String(v).slice(0, 40))}</td>`).join('')}</tr>`).join('')}</tbody>
    </table></div>
    ${masked ? `<div class="hint" style="margin-top:6px">${masked} 个字段已按脱敏策略遮罩</div>` : ''}`;
}

/* ============================== 二、派生模型向导 ============================== */

/**
 * deriveWizard 从上游模型派生一层下游模型（ODS→DWD→DWS→ADS）。
 *
 * 参照 dbt 的「在血缘图上给源表加 scaffold」：继承列、继承主键候选，
 * 只让用户决定「留哪些列、要不要聚合」，SQL 由向导拼好。
 */
async function deriveWizard(upstreamName) {
  const [models, dss] = await Promise.all([
    api('/models').catch(() => []),
    api('/datasources').catch(() => []),
  ]);
  await wzLayerRules();
  const up = (models || []).find(m => m.name === upstreamName);
  if (!up) { toast('找不到上游模型 ' + upstreamName, 'err'); return; }
  const upSpec = up.spec || {};
  const upLayer = (up.layer || 'ods').toLowerCase();
  const nextLayer = WZ_NEXT_LAYER[upLayer] || 'dwd';
  const suffix = up.name.replace(/^(ods|dim|dwd|dws|ads)_/, '');
  const required = wzRequiredCols(nextLayer);

  const st = {
    models: models || [],
    dss: dss || [],
    up,
    upSpec,
    layer: nextLayer,
    name: nextLayer + '_' + suffix,
    datasource: upSpec.datasource || '',
    description: '',
    cols: wzCols(upSpec)
      .filter(c => !/^_godw_/.test(c.name))
      .map(c => ({
        name: c.name, type: c.type, comment: c.comment || '',
        req: required.includes(c.name), on: true, agg: '',
      })),
    primaryKey: (upSpec.primaryKey || []).slice(),
    groupBy: [],
    engine: wzEngineOf(dss, upSpec.datasource),
    ddl: '',
    sql: '',
  };
  if (!st.primaryKey.length) {
    const idc = st.cols.find(c => /(_id|^id)$/.test(c.name));
    if (idc) st.primaryKey = [idc.name];
  }

  const refreshSQL = () => {
    st.sql = wzDeriveSQL(st);
  };

  wizOpen({
    title: '生成下游模型',
    sub: `上游 ${up.name}（${upLayer.toUpperCase()}）→ ${st.layer.toUpperCase()}`,
    state: st,
    finishText: '创建模型',
    steps: [
      {
        label: '目标层',
        hint: '层已按 ODS→DWD→DWS→ADS 自动选到下一层',
        render: (body, s) => {
          body.innerHTML = `
            <div class="wiz-pane-title">上游模型</div>
            <div class="wiz-src-card">
              <div class="row" style="gap:8px;align-items:center">
                ${wzLayerPill(s.up.layer)}
                <b class="mono">${esc(s.up.name)}</b>
                <span class="hint">${wzCols(s.upSpec).length} 列 · ${esc(s.upSpec.datasource || '')}${s.upSpec.schema ? ' · ' + esc(s.upSpec.schema) : ''}</span>
              </div>
              <div class="hint" style="margin-top:4px">${esc(s.upSpec.description || '无描述')}</div>
            </div>

            <div class="wiz-pane-title" style="margin-top:18px">放到哪一层</div>
            <div class="wiz-choice-row">
              ${['dwd', 'dws', 'ads', 'dim'].map(l => `
                <div class="wiz-choice ${s.layer === l ? 'sel' : ''}" data-layer="${l}">
                  <div class="wiz-choice-t">${WZ_LAYER[l].label}</div>
                  <div class="wiz-choice-d">${esc(WZ_LAYER[l].desc)}</div>
                </div>`).join('')}
            </div>

            <div class="wiz-split" style="margin-top:18px;grid-template-columns:1fr 1fr">
              <label class="field"><span>模型名</span>
                <input id="wz-dname" class="mono" value="${esc(s.name)}" />
                <em class="hint" id="wz-dname-ok"></em>
              </label>
              <label class="field"><span>写入数据源</span>
                <select id="wz-dds">${(s.dss || []).map(d => `<option value="${esc(d.name)}" ${d.name === s.datasource ? 'selected' : ''}>${esc(d.name)} · ${esc(d.type)}</option>`).join('')}</select>
              </label>
            </div>
            <label class="field"><span>描述（可选）</span>
              <input id="wz-ddesc" value="${esc(s.description)}" placeholder="这批数据是干什么用的" />
            </label>
            <div class="wiz-note">模型只定义「表长什么样」。真正把数据搬过去的是管道，最后一步会给入口。</div>`;

          const nameEl = body.querySelector('#wz-dname');
          const okEl = body.querySelector('#wz-dname-ok');
          const check = () => {
            const v = nameEl.value.trim();
            s.name = v;
            if (!wzIdentOk(v)) { okEl.textContent = '只能用小写字母、数字、下划线'; okEl.className = 'hint err'; return; }
            if (!v.startsWith(s.layer + '_')) { okEl.textContent = `建议以 ${s.layer}_ 开头`; okEl.className = 'hint warn'; return; }
            okEl.textContent = '名称可用'; okEl.className = 'hint ok';
          };
          nameEl.addEventListener('input', check);
          check();
          body.querySelectorAll('[data-layer]').forEach(el => el.addEventListener('click', () => {
            const l = el.dataset.layer;
            const oldPrefix = s.layer + '_';
            s.layer = l;
            if (s.name.startsWith(oldPrefix)) s.name = l + '_' + s.name.slice(oldPrefix.length);
            nameEl.value = s.name;
            body.querySelectorAll('[data-layer]').forEach(x => x.classList.toggle('sel', x.dataset.layer === l));
            check();
          }));
          body.querySelector('#wz-dds').addEventListener('change', ev => {
            s.datasource = ev.target.value;
            s.engine = wzEngineOf(s.dss, s.datasource);
          });
          body.querySelector('#wz-ddesc').addEventListener('input', ev => { s.description = ev.target.value.trim(); });
        },
        next: (s) => {
          if (!wzIdentOk(s.name)) { toast('模型名不合法', 'err'); return false; }
          if (!s.name.startsWith(s.layer + '_')) { toast(`模型名要以 ${s.layer}_ 开头`, 'err'); return false; }
          return true;
        },
      },

      {
        label: '列与口径',
        hint: '默认带走上游全部业务列；要汇总就选分组列',
        render: (body, s) => {
          const renderBody = () => {
            body.innerHTML = `
              <div class="wiz-bar">
                <button class="sm ghost" data-wz="all">全选</button>
                <button class="sm ghost" data-wz="none">全不选</button>
                <span class="hint">上游 ${s.cols.length} 列 · 已选 <b class="mono">${s.cols.filter(c => c.on).length}</b></span>
                <span class="spacer"></span>
                <span class="hint">主键：</span>
                ${s.cols.filter(c => c.on).slice(0, 12).map(c => `<label class="wiz-pk"><input type="checkbox" data-pk="${esc(c.name)}" ${s.primaryKey.includes(c.name) ? 'checked' : ''}>${esc(c.name)}</label>`).join('')}
              </div>
              <div class="wiz-map" style="max-height:320px">
                <table class="dt">
                  <thead><tr>
                    <th style="width:34px"></th><th>列</th><th style="width:105px">类型</th>
                    <th>注释</th><th style="width:150px">汇总方式</th>
                  </tr></thead>
                  <tbody>${s.cols.map((c, i) => `<tr class="${c.on ? '' : 'wz-off'}">
                    <td><input type="checkbox" data-on="${i}" ${c.on ? 'checked' : ''} ${c.req ? 'disabled title="该层必填列，不能去掉"' : ''}></td>
                    <td class="mono">${esc(c.name)}${c.req ? ' ' + wzChip('必填', 'lock') : ''}</td>
                    <td class="mono small dim">${esc(c.type)}</td>
                    <td class="dim small">${esc(c.comment || '—')}</td>
                    <td><select data-agg="${i}" ${c.req ? 'disabled title="必填列必须作为分组维度"' : ''}>
                      <option value="">直接带过去</option>
                      <option value="sum" ${c.agg === 'sum' ? 'selected' : ''}>SUM 求和</option>
                      <option value="count" ${c.agg === 'count' ? 'selected' : ''}>COUNT 计数</option>
                      <option value="count_distinct" ${c.agg === 'count_distinct' ? 'selected' : ''}>去重计数</option>
                      <option value="avg" ${c.agg === 'avg' ? 'selected' : ''}>AVG 平均</option>
                      <option value="max" ${c.agg === 'max' ? 'selected' : ''}>MAX</option>
                      <option value="min" ${c.agg === 'min' ? 'selected' : ''}>MIN</option>
                    </select></td>
                  </tr>`).join('')}</tbody>
                </table>
              </div>
              <div class="wiz-note">
                选了「汇总方式」的列会变成聚合结果，其余勾选的列自动成为分组维度。
                例如勾 dt + status 直接带过去、amount 选 SUM，就是「按天按状态算 GMV」。
              </div>`;
            body.querySelector('[data-wz="all"]').addEventListener('click', () => { s.cols.forEach(c => c.on = true); renderBody(); });
            body.querySelector('[data-wz="none"]').addEventListener('click', () => { s.cols.forEach(c => { if (!c.req) c.on = false; }); renderBody(); });
            body.querySelectorAll('[data-on]').forEach(el => el.addEventListener('change', () => {
              const i = Number(el.dataset.on);
              s.cols[i].on = el.checked;
              if (!el.checked) { s.cols[i].agg = ''; s.primaryKey = s.primaryKey.filter(k => k !== s.cols[i].name); }
              renderBody();
            }));
            body.querySelectorAll('[data-agg]').forEach(el => el.addEventListener('change', () => {
              const i = Number(el.dataset.agg);
              s.cols[i].agg = el.value;
              if (el.value) s.primaryKey = s.primaryKey.filter(k => k !== s.cols[i].name);
              renderBody();
            }));
            body.querySelectorAll('[data-pk]').forEach(el => el.addEventListener('change', () => {
              const n = el.dataset.pk;
              if (el.checked) { if (!s.primaryKey.includes(n)) s.primaryKey.push(n); }
              else s.primaryKey = s.primaryKey.filter(k => k !== n);
            }));
          };
          renderBody();
        },
        next: (s) => {
          const on = s.cols.filter(c => c.on);
          if (!on.length) { toast('至少留一列', 'err'); return false; }
          if (on.every(c => c.agg)) { toast('不能全部是聚合列，至少留一个分组维度（如 dt）', 'err'); return false; }
          refreshSQL();
          return true;
        },
      },

      {
        label: 'SQL 与确认',
        hint: '这段 SQL 会写进管道的节点里，可随时改',
        render: async (body, s) => {
          refreshSQL();
          body.innerHTML = `
            <div class="wiz-split" style="grid-template-columns:minmax(0,1.15fr) minmax(0,1fr)">
              <div class="wiz-pane">
                <div class="wiz-pane-title">转换 SQL</div>
                <pre class="code wiz-ddl" id="wz-derive-sql">${esc(s.sql)}</pre>
                <div class="hint" style="margin-top:8px">
                  <span class="mono">\${biz_date}</span> 会被替换成运行当天的业务日期（默认 T-1）。
                </div>
              </div>
              <div class="wiz-pane">
                <div class="wiz-pane-title">将创建</div>
                <dl class="kv">
                  <dt>模型</dt><dd class="mono">${esc(s.name)}</dd>
                  <dt>分层</dt><dd>${wzLayerPill(s.layer)}</dd>
                  <dt>写入</dt><dd class="mono">${esc(s.datasource)}${s.upSpec.schema ? ' / ' + esc(s.layer) : ''}</dd>
                  <dt>列数</dt><dd class="mono">${s.cols.filter(c => c.on).length}</dd>
                  <dt>主键</dt><dd class="mono">${esc(s.primaryKey.join(', ') || '未指定')}</dd>
                  <dt>分区</dt><dd class="mono">${esc((s.upSpec.partitionBy || [])[0] || '不分区')}</dd>
                </dl>
                <div class="wiz-pane-title" style="margin-top:14px">建表语句</div>
                <pre class="code wiz-ddl" id="wz-derive-ddl">编译中…</pre>
              </div>
            </div>`;
          try {
            const r = await api('/models/preview', { method: 'POST', body: { spec: wzDeriveSpec(s) } });
            const pre = body.querySelector('#wz-derive-ddl');
            if (pre) pre.textContent = r.ddl || '（无 DDL）';
          } catch (e) {
            const pre = body.querySelector('#wz-derive-ddl');
            if (pre) pre.textContent = '编译失败：' + e.message;
          }
        },
      },
    ],

    finish: async (s) => api('/models', { method: 'POST', body: { spec: wzDeriveSpec(s), publish: true } }),

    done: (m, s) => `
      <div class="wiz-done">
        <div class="wiz-done-icon">✓</div>
        <h4>${esc(m.name)} 已创建并发布 v${m.activeVersion || 1}</h4>
        <div class="hint">${s.cols.filter(c => c.on).length} 列 · ${wzLayerPill(s.layer)} · 上游 ${esc(s.up.name)}</div>
        <div class="wiz-done-next">
          <div class="wiz-pane-title">让它跑起来</div>
          <div class="row wrap" style="gap:8px">
            <button class="primary" data-wz-next="pipe">生成管道并试跑</button>
            <button class="ghost" data-wz-next="derive">继续往下游做一层</button>
            <button class="ghost" data-wz-next="design">打开设计器</button>
          </div>
          <div class="hint" style="margin-top:10px">管道向导会自动带上这条 SQL 和上游依赖。</div>
        </div>
      </div>`,
    bindDone: (body, m, s) => {
      body.querySelectorAll('[data-wz-next]').forEach(b => b.addEventListener('click', () => {
        const kind = b.dataset.wzNext;
        closeModal();
        if (kind === 'pipe') return pipelineWizard(m.name, s.sql);
        if (kind === 'derive') return deriveWizard(m.name);
        sessionStorage.setItem('godw.openModel', m.name);
        go('models');
      }));
    },
  });
}

function wzDeriveSpec(s) {
  const on = s.cols.filter(c => c.on);
  const grouped = on.filter(c => !c.agg);
  const cols = on.map(c => c.agg
    ? { name: c.agg + '_' + c.name, type: c.agg === 'count' || c.agg === 'count_distinct' ? 'bigint' : (c.type === 'int' ? 'bigint' : c.type), nullable: false, comment: c.agg.toUpperCase() + '(' + c.name + ')' }
    : { name: c.name, type: c.type, nullable: false, comment: c.comment || undefined });
  const names = new Set(cols.map(c => c.name));
  return {
    kind: 'Model',
    name: s.name,
    layer: s.layer,
    description: s.description || undefined,
    datasource: s.datasource,
    schema: s.layer,
    columns: cols,
    primaryKey: s.primaryKey.filter(k => names.has(k)),
    uniqueKey: s.primaryKey.filter(k => names.has(k)),
    partitionBy: (s.upSpec.partitionBy || []).filter(k => names.has(k)),
    materialization: 'table',
  };
}

function wzDeriveSQL(s) {
  const on = s.cols.filter(c => c.on);
  const grouped = on.filter(c => !c.agg);
  const aggs = on.filter(c => c.agg);
  const parts = [];
  grouped.forEach(c => parts.push(wzQ(s.engine, c.name)));
  aggs.forEach(c => {
    const f = c.agg === 'count_distinct' ? 'COUNT(DISTINCT ' + wzQ(s.engine, c.name) + ')'
      : c.agg.toUpperCase() + '(' + wzQ(s.engine, c.name) + ')';
    parts.push(f + ' AS ' + wzQ(s.engine, c.agg + '_' + c.name));
  });
  const src = wzFullTable(s.engine, s.upSpec.schema || '', s.upSpec.table || s.up.name);
  const dateCol = wzDateCol(s.upSpec);
  const where = dateCol ? `\nWHERE ${wzQ(s.engine, dateCol)} = \${biz_date}` : '';
  const group = grouped.length && aggs.length
    ? '\nGROUP BY ' + grouped.map(c => wzQ(s.engine, c.name)).join(', ')
    : '';
  return `SELECT ${parts.join(',\n       ')}\nFROM ${src}${where}${group}`;
}

/* ============================== 三、管道向导 ============================== */

/**
 * pipelineWizard 用「选模型 → 选调度」生成一条可运行的管道。
 *
 * 旧入口是一整段 YAML：nodes、depends_on、write_mode、${biz_date} 一个都不能错。
 * 现在节点由所选模型推导，SQL 自动生成，调度用「每天 02:00」这种人话选。
 */
async function pipelineWizard(presetModel, presetSQL) {
  const [models, dss] = await Promise.all([
    api('/models').catch(() => []),
    api('/datasources').catch(() => []),
  ]);
  const usable = (models || []).filter(m => (m.layer || '').toLowerCase() !== 'ods');
  if (!usable.length) {
    toast('还没有可调度的模型，先在模型设计里建一个 DWD/DWS/ADS 模型', 'err');
    return;
  }

  const st = {
    models: models || [],
    dss: dss || [],
    picked: [],
    nodes: [],       // {model, sourceKey, writeMode, sql}
    name: '',
    schedule: 'daily',
    hour: '02',
    minute: '00',
    weekday: '1',
    runNow: false,
  };

  // presetModel 来自「刚建好的模型」，直接把这一条填好，用户只剩调度要选
  if (presetModel) {
    const m = (models || []).find(x => x.name === presetModel);
    if (m) {
      st.picked = [m.name];
      st.nodes = [wzMakeNode(m, st, presetSQL)];
    }
  }

  const sourceOptions = (s) => {
    const out = [];
    (s.models || []).forEach(m => {
      if ((m.spec && m.spec.columns || []).length) {
        out.push({
          key: 'm:' + m.name,
          label: m.name,
          sub: `${(m.layer || '').toUpperCase()} · ${esc((m.spec && m.spec.datasource) || '')}`,
          schema: (m.spec && m.spec.schema) || '',
          table: (m.spec && (m.spec.table || m.name)) || m.name,
          columns: wzCols(m.spec),
          layer: (m.layer || '').toLowerCase(),
        });
      }
    });
    return out;
  };

  wizOpen({
    title: '新建管道',
    sub: '选要产出的模型 → 确认数据来源 → 定调度',
    state: st,
    finishText: '发布管道',
    steps: [
      {
        label: '产出',
        hint: '选中的模型会成为管道里的一个节点',
        render: (body, s) => {
          const render = (kw) => {
            const list = usable.filter(m => !kw || m.name.includes(kw));
            const byLayer = {};
            list.forEach(m => { const l = (m.layer || '').toLowerCase(); (byLayer[l] = byLayer[l] || []).push(m); });
            body.innerHTML = `
              <div class="wiz-bar">
                <input id="wz-pick-q" placeholder="搜索模型名" value="${esc(kw || '')}" style="max-width:240px">
                <span class="hint">已选 <b class="mono">${s.picked.length}</b> 个</span>
              </div>
              <div class="wiz-pick">
                ${['dwd', 'dws', 'ads', 'dim'].filter(l => byLayer[l]).map(l => `
                  <div class="wiz-pick-group">
                    <div class="wiz-pick-head">${wzLayerPill(l)}<span class="hint">${WZ_LAYER[l] ? WZ_LAYER[l].desc : ''}</span></div>
                    ${byLayer[l].map(m => `
                      <label class="wiz-pick-item ${s.picked.includes(m.name) ? 'sel' : ''}">
                        <input type="checkbox" data-pick="${esc(m.name)}" ${s.picked.includes(m.name) ? 'checked' : ''}>
                        <span class="mono">${esc(m.name)}</span>
                        <span class="hint">${wzCols(m.spec).length} 列 · v${m.activeVersion || 0}</span>
                      </label>`).join('')}
                  </div>`).join('') || '<div class="empty">没有匹配的模型</div>'}
              </div>
              <div class="wiz-note">ODS 表由采集同步写入，不用放进管道。要从 ODS 往 DWD 搬数据，把 DWD 模型勾上。</div>`;
            const q = body.querySelector('#wz-pick-q');
            q.addEventListener('input', () => render(q.value.trim().toLowerCase()));
            q.focus();
            q.setSelectionRange(q.value.length, q.value.length);
            body.querySelectorAll('[data-pick]').forEach(el => el.addEventListener('change', () => {
              const name = el.dataset.pick;
              if (el.checked) { if (!s.picked.includes(name)) s.picked.push(name); }
              else s.picked = s.picked.filter(x => x !== name);
              el.closest('.wiz-pick-item').classList.toggle('sel', el.checked);
              s.nodes = s.picked.map(n => s.nodes.find(x => x.model.name === n) || wzMakeNode((s.models || []).find(m => m.name === n), s));
            }));
          };
          render('');
        },
        next: (s) => {
          if (!s.picked.length) { toast('至少选一个要产出的模型', 'err'); return false; }
          s.nodes = s.picked.map(n => s.nodes.find(x => x.model.name === n) || wzMakeNode((s.models || []).find(m => m.name === n), s));
          // 多个节点是「按选择顺序串成链」，那么第 2 个节点起默认就从上一个节点取数——
          // 否则它会各自去猜一个 ODS 源，链就断了，还得用户逐个改回来。
          s.nodes.forEach((n, i) => {
            if (i === 0) return;
            const prev = s.nodes[i - 1];
            n.sourceKey = 'm:' + prev.model.name;
            wzRebuildSQL(n, s);
          });
          return true;
        },
      },

      {
        label: '数据来源',
        hint: '已按层自动猜好上游；不对再改',
        render: (body, s) => {
          const render = () => {
            body.innerHTML = `
              <div class="wiz-map">
                <table class="dt">
                  <thead><tr><th>产出模型</th><th style="width:34%">数据来自</th><th style="width:150px">写入方式</th><th style="width:64px">依赖</th></tr></thead>
                  <tbody>${s.nodes.map((n, i) => `
                    <tr>
                      <td><b class="mono">${esc(n.model.name)}</b><div class="mute small">${esc(n.model.layer || '')}</div></td>
                      <td><select data-src="${i}">
                        ${sourceOptions(s).filter(o => o.label !== n.model.name).map(o =>
                          `<option value="${esc(o.key)}" ${o.key === n.sourceKey ? 'selected' : ''}>${esc(o.label)}${o.schema ? ' · ' + esc(o.schema) : ''}</option>`).join('')}
                      </select></td>
                      <td><select data-wm="${i}">
                        ${['append', 'overwrite_partition', 'upsert', 'overwrite'].map(m =>
                          `<option value="${m}" ${m === n.writeMode ? 'selected' : ''}>${wzWriteModeLabel(m)}</option>`).join('')}
                      </select></td>
                      <td class="center">${i === 0 ? '<span class="mute">—</span>' : `<span class="pill mute">上一步</span>`}</td>
                    </tr>`).join('')}</tbody>
                </table>
              </div>
              <div class="wiz-pane-title" style="margin-top:14px">节点 SQL</div>
              <pre class="code wiz-ddl" style="max-height:200px">${esc(s.nodes.map(n => '-- ' + n.model.name + '\n' + n.sql).join('\n\n'))}</pre>
              ${s.nodes.filter(n => (n.missing || []).length).map(n => `<div class="wiz-note warn">
                <b>${esc(n.model.name)}</b> 有 ${n.missing.length} 列在来源里找不到：<span class="mono">${n.missing.map(esc).join('、')}</span>。
                这段 SQL 只带得过去能对上的列；聚合口径（如 GMV = SUM(amount)）要在「模型设计」里用派生向导生成。
              </div>`).join('')}
              <div class="wiz-note">多个产出模型会按选择顺序串成链：后一个节点默认读前一个节点的结果，可以在上表改。</div>`;
            body.querySelectorAll('[data-src]').forEach(el => el.addEventListener('change', () => {
              const i = Number(el.dataset.src);
              s.nodes[i].sourceKey = el.value;
              wzRebuildSQL(s.nodes[i], s);
              render();
            }));
            body.querySelectorAll('[data-wm]').forEach(el => el.addEventListener('change', () => {
              s.nodes[Number(el.dataset.wm)].writeMode = el.value;
            }));
          };
          render();
        },
      },

      {
        label: '调度与发布',
        hint: '默认每天 02:00 跑一次；随时可改',
        render: (body, s) => {
          if (!s.name) s.name = wzPipeName(s);
          const cron = wzCron(s.schedule, s.hour, s.minute, s.weekday);
          body.innerHTML = `
            <div class="wiz-split" style="grid-template-columns:minmax(0,1fr) minmax(0,1fr)">
              <div class="wiz-pane">
                <label class="field"><span>管道名</span>
                  <input id="wz-pname" class="mono" value="${esc(s.name)}"></label>
                <div class="wiz-pane-title" style="margin-top:14px">多久跑一次</div>
                <div class="wiz-choice-row">
                  ${[['manual', '手动'], ['hourly', '每小时'], ['daily', '每天'], ['weekly', '每周'], ['monthly', '每月']]
                    .map(([k, t]) => `<div class="wiz-choice sm ${s.schedule === k ? 'sel' : ''}" data-sched="${k}"><div class="wiz-choice-t">${t}</div></div>`).join('')}
                </div>
                <div class="wiz-split" style="grid-template-columns:1fr 1fr;margin-top:10px" id="wz-time-row">
                  <label class="field" id="wz-hour-wrap"><span>时间</span>
                    <div class="row" style="margin:0;gap:6px">
                      <select id="wz-hour">${Array.from({ length: 24 }, (_, h) => `<option value="${String(h).padStart(2, '0')}" ${String(h).padStart(2, '0') === s.hour ? 'selected' : ''}>${String(h).padStart(2, '0')} 时</option>`).join('')}</select>
                      <select id="wz-min">${[0, 15, 30, 45].map(m => `<option value="${String(m).padStart(2, '0')}" ${String(m).padStart(2, '0') === s.minute ? 'selected' : ''}>${String(m).padStart(2, '0')} 分</option>`).join('')}</select>
                    </div>
                  </label>
                  <label class="field" id="wz-week-wrap"><span>星期</span>
                    <select id="wz-week">${'日一二三四五六'.split('').map((t, i) => `<option value="${i}" ${String(i) === s.weekday ? 'selected' : ''}>周${t}</option>`).join('')}</select>
                  </label>
                </div>
                <div class="wiz-note" id="wz-cron-note">实际 cron：<span class="mono">${esc(cron || '手动触发，不排期')}</span></div>
                <label class="row" style="margin:14px 0 0;gap:8px">
                  <input type="checkbox" id="wz-runnow" ${s.runNow ? 'checked' : ''} style="width:auto">
                  <span class="hint">发布后立刻试跑一次（业务日期取昨天）</span>
                </label>
              </div>
              <div class="wiz-pane">
                <div class="wiz-pane-title">执行图</div>
                <div class="wiz-dag" id="wz-dag"></div>
                <div class="wiz-pane-title" style="margin-top:12px">节点</div>
                <div class="wiz-nodes">
                  ${s.nodes.map((n, i) => `<div class="wiz-node">
                    <span class="wiz-node-n">${i + 1}</span>
                    <span class="mono">${esc(n.model.name)}</span>
                    <span class="hint">${esc(n.sourceLabel || '')}</span>
                  </div>`).join('')}
                </div>
              </div>
            </div>`;

          const syncCron = () => {
            const note = body.querySelector('#wz-cron-note');
            const hourWrap = body.querySelector('#wz-hour-wrap');
            const weekWrap = body.querySelector('#wz-week-wrap');
            hourWrap.style.display = s.schedule === 'hourly' || s.schedule === 'manual' ? 'none' : '';
            weekWrap.style.display = s.schedule === 'weekly' ? '' : 'none';
            const c = wzCron(s.schedule, s.hour, s.minute, s.weekday);
            note.innerHTML = s.schedule === 'manual'
              ? '手动触发，不排期。'
              : `实际 cron：<span class="mono">${esc(c)}</span> · ${esc(wzCronText(c))}`;
          };
          body.querySelectorAll('[data-sched]').forEach(el => el.addEventListener('click', () => {
            s.schedule = el.dataset.sched;
            body.querySelectorAll('[data-sched]').forEach(x => x.classList.toggle('sel', x.dataset.sched === s.schedule));
            syncCron();
          }));
          body.querySelector('#wz-hour').addEventListener('change', ev => { s.hour = ev.target.value; syncCron(); });
          body.querySelector('#wz-min').addEventListener('change', ev => { s.minute = ev.target.value; syncCron(); });
          body.querySelector('#wz-week').addEventListener('change', ev => { s.weekday = ev.target.value; syncCron(); });
          body.querySelector('#wz-pname').addEventListener('input', ev => { s.name = ev.target.value.trim(); });
          body.querySelector('#wz-runnow').addEventListener('change', ev => { s.runNow = ev.target.checked; });
          syncCron();
          wzDrawDAG(body.querySelector('#wz-dag'), s.nodes);
        },
        next: (s) => {
          if (!wzIdentOk(s.name)) { toast('管道名只能用下划线和小写字母', 'err'); return false; }
          return true;
        },
      },
    ],

    finish: async (s) => {
      const spec = wzPipeSpec(s);
      const p = await api('/pipelines', { method: 'POST', body: { spec, publish: true } });
      if (s.runNow) {
        const d = new Date(Date.now() - 86400000);
        try {
          await api('/pipelines/' + encodeURIComponent(p.name) + '/trigger', {
            method: 'POST', body: { bizDate: wzYmd(d) },
          });
          toast('已触发试跑，业务日期 ' + wzYmd(d), 'ok');
        } catch (e) {
          toast('管道已发布，试跑失败：' + e.message, 'err');
        }
      }
      return p;
    },

    done: (p, s) => `
      <div class="wiz-done">
        <div class="wiz-done-icon">✓</div>
        <h4>${esc(p.name)} 已发布 v${p.activeVersion || 1}</h4>
        <div class="hint">${s.nodes.length} 个节点 · ${esc(wzCronText(wzCron(s.schedule, s.hour, s.minute, s.weekday)))}${s.runNow ? ' · 已触发试跑' : ''}</div>
        <div class="wiz-done-next">
          <div class="wiz-pane-title">接下来</div>
          <div class="row wrap" style="gap:8px">
            <button class="primary" data-wz-next="instances">看运行结果</button>
            <button class="ghost" data-wz-next="canvas">去编排画布微调</button>
            <button class="ghost" data-wz-next="metric">为产出的表建指标</button>
          </div>
        </div>
      </div>`,
    bindDone: (body, p, s) => {
      body.querySelectorAll('[data-wz-next]').forEach(b => b.addEventListener('click', () => {
        const kind = b.dataset.wzNext;
        closeModal();
        if (kind === 'canvas') { S.selected.canvasPipe = p.name; go('canvas'); return; }
        if (kind === 'metric') { metricWizard(); return; }
        S.selected.instQuery = null;
        go('instances');
      }));
    },
  });
}

function wzPipeName(s) {
  const first = s.nodes[0];
  if (!first) return 'new_pipeline';
  if (s.nodes.length === 1) return first.model.name + '_daily';
  const head = first.model.name.replace(/^(ods|dwd|dws|ads)_/, '');
  return head + '_chain_daily';
}

function wzPipeSpec(s) {
  const nodes = s.nodes.map((n, i) => ({
    id: n.model.name,
    type: 'sql',
    name: n.model.spec && n.model.spec.description ? String(n.model.spec.description).slice(0, 40) : '',
    datasource: (n.model.spec && n.model.spec.datasource) || '',
    model: n.model.name,
    writeMode: n.writeMode,
    dependsOn: i > 0 ? [s.nodes[i - 1].model.name] : [],
    sql: n.sql,
  }));
  return {
    kind: 'Pipeline',
    name: s.name,
    description: `${nodes.length} 个模型的生产链路`,
    schedule: wzCron(s.schedule, s.hour, s.minute, s.weekday),
    timezone: 'Asia/Shanghai',
    params: [{ name: 'biz_date', type: 'date', required: true, description: '业务日期，默认 T-1' }],
    nodes,
  };
}

// wzMakeNode 给模型推一个默认上游：同后缀优先（dwd_order ← ods_order），否则取第一个 ODS。
function wzMakeNode(model, st, presetSQL) {
  if (!model) return null;
  const suffix = String(model.name).replace(/^(ods|dim|dwd|dws|ads)_/, '');
  const all = (st.models || []).filter(m => m.name !== model.name && (m.spec && (m.spec.columns || []).length));
  const hit = all.find(m => m.name === 'ods_' + suffix)
    || all.find(m => m.name === suffix)
    || all.find(m => m.name === 'dwd_' + suffix)
    || all.find(m => (m.layer || '') === 'ods')
    || all[0];
  const node = {
    model,
    sourceKey: hit ? 'm:' + hit.name : '',
    sourceLabel: '',
    writeMode: wzGuessWriteMode(model.spec || {}),
    sql: '',
  };
  wzRebuildSQL(node, st, presetSQL);
  return node;
}

function wzRebuildSQL(node, st, presetSQL) {
  if (!node || !node.model) return;
  if (presetSQL && !node.sql) { node.sql = presetSQL; return; }
  const m = node.model;
  const spec = m.spec || {};
  const engine = wzEngineOf(st.dss, spec.datasource);
  const cols = wzCols(spec).map(c => wzQ(engine, c.name));
  const srcKey = node.sourceKey || '';
  let src = '', srcCols = [];
  if (srcKey.startsWith('m:')) {
    const up = (st.models || []).find(x => x.name === srcKey.slice(2));
    if (up) {
      const us = up.spec || {};
      src = wzFullTable(engine, us.schema || '', us.table || up.name);
      srcCols = wzCols(us);
      node.sourceLabel = up.name;
    }
  }
  if (!src) { node.sql = `-- 未找到上游，请在下一步选择数据来源\nSELECT ${cols.join(', ') || '*'}`; return; }
  const dateCol = wzDateCol({ columns: srcCols, partitionBy: [] });
  const where = dateCol ? `\nWHERE ${wzQ(engine, dateCol)} = \${biz_date}` : '';
  // 上游有的列才写进 SELECT：模型加了列但上游还没同步过来时，这里能一眼看出差异
  const usable = wzCols(spec).filter(c => !srcCols.length || srcCols.some(x => x.name === c.name));
  const list = usable.length ? usable.map(c => wzQ(engine, c.name)) : cols;
  // missing 是「模型声明了、但来源里没有」的列。静默丢掉它们等于悄悄改变口径，
  // 所以要在界面上点名——这让「这条 SQL 是不是完整」变成可判断的事。
  node.missing = wzCols(spec).filter(c => srcCols.length && !srcCols.some(x => x.name === c.name)).map(c => c.name);
  node.sql = `SELECT ${list.join(', ')}\nFROM ${src}${where}`;
}

// wzDrawDAG 画管道缩略图。层级由 dependsOn 推导，与管道页的 DAG 保持一致。
function wzDrawDAG(host, nodes) {
  if (!host) return;
  const list = (nodes || []).filter(Boolean);
  if (!list.length) { host.innerHTML = '<div class="hint">还没有节点</div>'; return; }
  const W = 118, H = 42, GAP = 30;
  const total = list.length * W + (list.length - 1) * GAP;
  host.innerHTML = `<svg viewBox="0 0 ${Math.max(total, 260)} ${H + 26}" style="width:100%;height:${H + 26}px">
    <defs><marker id="wz-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto">
      <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--edge)"></path></marker></defs>
    ${list.map((n, i) => {
      const x = i * (W + GAP) + 4;
      const layer = (n.model.layer || '').toLowerCase();
      return `<g>
        ${i > 0 ? `<path d="M ${x - GAP + 2} ${H / 2 + 8} L ${x - 4} ${H / 2 + 8}" stroke="var(--edge)" stroke-width="1.4" marker-end="url(#wz-arrow)" fill="none"/>` : ''}
        <rect x="${x}" y="6" width="${W}" height="${H}" rx="7" fill="var(--bg-elev2)" stroke="var(--layer-${layer || 'other'})" stroke-width="1.2"/>
        <text x="${x + 10}" y="24" fill="var(--text)" font-size="11" font-family="var(--sans)">${esc(String(n.model.name).slice(0, 15))}</text>
        <text x="${x + 10}" y="39" fill="var(--text-mute)" font-size="9.5" font-family="var(--sans)">${(layer || '').toUpperCase()} · ${esc(wzWriteModeLabel(n.writeMode))}</text>
      </g>`;
    }).join('')}
  </svg>`;
}

/* ============================== 四、指标向导 ============================== */

/**
 * metricWizard 用选择题定义一个指标。
 *
 * 指标的核心是「口径」，不是 YAML。这里先让人看到「哪个表、按什么聚合、拆什么维度」，
 * 最后一步直接跑真实数据画曲线——口径错了当场就能发现。
 */
async function metricWizard() {
  const [models, dss] = await Promise.all([
    api('/models').catch(() => []),
    api('/datasources').catch(() => []),
  ]);
  const usable = (models || []).filter(m => (m.spec && wzCols(m.spec).length) && (m.activeVersion || 0) > 0);
  if (!usable.length) {
    toast('还没有已发布的模型，指标必须挂在一个已发布模型上', 'err');
    return;
  }

  const st = {
    models: usable,
    dss: dss || [],
    model: usable[0],
    name: '',
    displayName: '',
    agg: 'sum',
    expr: '',
    timeColumn: '',
    dimensions: [],
    unit: '',
    points: null,
    previewErr: '',
    previewSeq: 0,
  };
  const pickModel = (m) => {
    st.model = m;
    const cols = wzCols(m.spec);
    const num = cols.find(c => ['int', 'bigint', 'double', 'decimal'].includes(c.type));
    st.expr = num ? num.name : (cols[0] ? cols[0].name : '');
    st.timeColumn = wzDateCol(m.spec) || (cols[0] ? cols[0].name : '');
    st.dimensions = [];
    st.name = '';
    st.displayName = '';
  };
  pickModel(st.model);

  const autoName = () => {
    const base = st.agg === 'count' ? 'row' : st.expr;
    return (base + '_' + st.agg).replace(/[^a-z0-9_]/g, '_').toLowerCase();
  };

  wizOpen({
    title: '新建指标',
    sub: '选模型 → 定口径 → 看曲线',
    state: st,
    finishText: '保存指标',
    steps: [
      {
        label: '数据表',
        hint: '指标只能挂在已发布的模型上',
        render: (body, s) => {
          const render = () => {
            const cols = wzCols(s.model.spec);
            body.innerHTML = `
              <div class="wiz-pane-title">从哪个模型取数</div>
              <div class="wiz-choice-row">
                ${s.models.slice(0, 12).map(m => `
                  <div class="wiz-choice ${s.model.name === m.name ? 'sel' : ''}" data-model="${esc(m.name)}">
                    <div class="wiz-choice-t mono">${esc(m.name)}</div>
                    <div class="wiz-choice-d">${(m.layer || '').toUpperCase()} · ${wzCols(m.spec).length} 列</div>
                  </div>`).join('')}
              </div>
              ${s.models.length > 12 ? '<div class="hint" style="margin-top:6px">只列了前 12 个模型，可在「指标」页查看全部</div>' : ''}
              <label class="field" style="margin-top:16px;max-width:320px"><span>时间列</span>
                <select id="wz-mtime">${cols.filter(c => c.type === 'date' || c.type === 'datetime').map(c =>
                  `<option value="${esc(c.name)}" ${c.name === s.timeColumn ? 'selected' : ''}>${esc(c.name)}</option>`).join('') || `<option value="">（没有日期列）</option>`}</select>
              </label>
              <div class="wiz-note">时间列决定趋势图按什么分桶。通常就是分区列 dt。</div>`;
            body.querySelectorAll('[data-model]').forEach(el => el.addEventListener('click', () => {
              const m = s.models.find(x => x.name === el.dataset.model);
              if (!m) return;
              pickModel(m);
              render();
            }));
            const t = body.querySelector('#wz-mtime');
            if (t) t.addEventListener('change', ev => { s.timeColumn = ev.target.value; });
          };
          render();
        },
      },

      {
        label: '口径',
        hint: '聚合方式 + 字段，就是这一列的业务含义',
        render: (body, s) => {
          const render = () => {
            const cols = wzCols(s.model.spec);
            body.innerHTML = `
              <div class="wiz-pane-title">怎么算</div>
              <div class="wiz-choice-row">
                ${Object.keys(WZ_AGG_LABEL).map(k => `
                  <div class="wiz-choice sm ${s.agg === k ? 'sel' : ''}" data-agg="${k}">
                    <div class="wiz-choice-t">${WZ_AGG_LABEL[k]}</div>
                    <div class="wiz-choice-d mono">${k}</div>
                  </div>`).join('')}
              </div>
              ${s.agg !== 'count' ? `
                <label class="field" style="margin-top:14px;max-width:320px"><span>对哪一列</span>
                  <select id="wz-mexpr">${cols.map(c => `<option value="${esc(c.name)}" ${c.name === s.expr ? 'selected' : ''}>${esc(c.name)} · ${esc(c.type)}</option>`).join('')}</select>
                </label>` : '<div class="wiz-note" style="margin-top:12px">计数不需要指定列，统计行数。</div>'}

              <div class="wiz-pane-title" style="margin-top:16px">可以按什么拆分
                <span class="hint" style="font-weight:400">（看板上用来切片的维度）</span></div>
              <div class="wiz-chips">
                ${cols.filter(c => c.type !== 'text' && c.type !== 'json').slice(0, 24).map(c => `
                  <label class="wiz-pk ${s.dimensions.includes(c.name) ? 'sel' : ''}">
                    <input type="checkbox" data-dim="${esc(c.name)}" ${s.dimensions.includes(c.name) ? 'checked' : ''}>
                    ${esc(c.name)}
                  </label>`).join('')}
              </div>
              <div class="wiz-note" style="margin-top:14px">
                口径预览：<b class="mono">${s.agg.toUpperCase()}(${s.agg === 'count' ? '*' : esc(s.expr)})</b>
                ${s.dimensions.length ? ' · 按 ' + s.dimensions.map(esc).join('、') + ' 拆分' : ''}
              </div>`;
            body.querySelectorAll('[data-agg]').forEach(el => el.addEventListener('click', () => {
              s.agg = el.dataset.agg;
              render();
            }));
            const ex = body.querySelector('#wz-mexpr');
            if (ex) ex.addEventListener('change', ev => { s.expr = ev.target.value; render(); });
            body.querySelectorAll('[data-dim]').forEach(el => el.addEventListener('change', () => {
              const n = el.dataset.dim;
              if (el.checked) { if (!s.dimensions.includes(n)) s.dimensions.push(n); }
              else s.dimensions = s.dimensions.filter(x => x !== n);
              el.closest('.wiz-pk').classList.toggle('sel', el.checked);
            }));
          };
          render();
        },
        next: (s) => {
          if (s.agg !== 'count' && !s.expr) { toast('选一个要聚合的列', 'err'); return false; }
          if (!s.name) { s.name = autoName(); s.displayName = ''; }
          return true;
        },
      },

      {
        label: '命名与预览',
        hint: '改口径会立刻重算曲线',
        render: async (body, s) => {
          if (!s.name) s.name = autoName();
          body.innerHTML = `
            <div class="wiz-split" style="grid-template-columns:minmax(0,320px) minmax(0,1fr)">
              <div class="wiz-pane">
                <label class="field"><span>指标名（英文标识）</span>
                  <input id="wz-mname" class="mono" value="${esc(s.name)}">
                  <em class="hint" id="wz-mname-ok"></em></label>
                <label class="field"><span>显示名</span>
                  <input id="wz-mdisp" value="${esc(s.displayName || s.name)}"></label>
                <label class="field"><span>单位</span>
                  <input id="wz-munit" value="${esc(s.unit)}" placeholder="元 / 单 / %"></label>
                <dl class="kv" style="margin-top:14px">
                  <dt>口径</dt><dd class="mono">${s.agg.toUpperCase()}(${s.agg === 'count' ? '*' : esc(s.expr)})</dd>
                  <dt>模型</dt><dd class="mono">${esc(s.model.name)}</dd>
                  <dt>时间列</dt><dd class="mono">${esc(s.timeColumn || '—')}</dd>
                  <dt>维度</dt><dd class="mono">${esc(s.dimensions.join(', ') || '—')}</dd>
                </dl>
              </div>
              <div class="wiz-pane">
                <div class="wiz-pane-title">最近 30 天真实数据</div>
                <div id="wz-mchart" class="wiz-chart">
                  <svg class="chart" id="wz-mchart-svg" height="220"></svg>
                </div>
                <div class="hint" id="wz-mchart-note" style="margin-top:8px">正在试算…</div>
              </div>
            </div>`;

          const nameEl = body.querySelector('#wz-mname');
          const okEl = body.querySelector('#wz-mname-ok');
          const check = () => {
            const v = nameEl.value.trim();
            s.name = v;
            if (!wzIdentOk(v)) { okEl.textContent = '只能用小写字母、数字、下划线'; okEl.className = 'hint err'; return; }
            okEl.textContent = '名称可用'; okEl.className = 'hint ok';
          };
          nameEl.addEventListener('input', check);
          check();
          body.querySelector('#wz-mdisp').addEventListener('input', ev => { s.displayName = ev.target.value.trim(); });
          body.querySelector('#wz-munit').addEventListener('input', ev => { s.unit = ev.target.value.trim(); });

          const seq = ++s.previewSeq;
          const note = () => body.querySelector('#wz-mchart-note');
          try {
            const to = new Date();
            const from = new Date(to.getTime() - 30 * 86400000);
            const r = await api('/metrics/preview', {
              method: 'POST',
              body: {
                metric: wzMetricOf(s),
                query: { from: wzYmd(from), to: wzYmd(to), granularity: 'day', limit: 200 },
              },
            });
            if (seq !== s.previewSeq) return;
            const svg = body.querySelector('#wz-mchart-svg');
            if (r && (r.points || []).length) {
              drawLineChart(svg, r.points, s.unit);
              note().innerHTML = `<span class="ok">${r.points.length} 个数据点</span> · 数据来自 ${esc(s.model.name)}`;
            } else {
              note().innerHTML = '这段时间没有数据。口径没问题的话，先跑一轮管道再看。';
              body.querySelector('#wz-mchart').innerHTML = '<div class="empty" style="padding:40px 0"><b>暂无数据</b>指标已能算，只是区间内没有行</div>';
            }
          } catch (e) {
            if (seq !== s.previewSeq) return;
            note().innerHTML = `<span class="err">试算失败：${esc(e.message)}</span>`;
          }
        },
        next: (s) => {
          if (!wzIdentOk(s.name)) { toast('指标名不合法', 'err'); return false; }
          return true;
        },
      },
    ],

    // 结构化提交：指标接口与模型/管道/看板一致，接受 {metric}。
    // 前端不必再把选好的四个下拉拼回一段 YAML 让服务端解析回来。
    finish: async (s) => api('/metrics', { method: 'POST', body: { metric: wzMetricOf(s) } }),

    done: (m, s) => `
      <div class="wiz-done">
        <div class="wiz-done-icon">✓</div>
        <h4>指标 ${esc(m.displayName || m.name)} 已保存</h4>
        <div class="hint mono">${esc(m.agg)}(${esc(m.expr || '*')}) on ${esc(m.model)}${s.dimensions.length ? ' · 维度 ' + esc(s.dimensions.join(', ')) : ''}</div>
        <div class="wiz-done-next">
          <div class="wiz-pane-title">接下来</div>
          <div class="row wrap" style="gap:8px">
            <button class="primary" data-wz-next="dashboard">放进看板</button>
            <button class="ghost" data-wz-next="metric">再建一个指标</button>
            <button class="ghost" data-wz-next="bi">做一张 BI 图表</button>
          </div>
        </div>
      </div>`,
    bindDone: (body, m, s) => {
      body.querySelectorAll('[data-wz-next]').forEach(b => b.addEventListener('click', () => {
        const kind = b.dataset.wzNext;
        closeModal();
        S.selected.metric = m.name;
        if (kind === 'metric') return metricWizard();
        if (kind === 'bi') return go('bi');
        go('dashboards');
      }));
    },
  });
}

function wzMetricOf(s) {
  return {
    name: s.name,
    displayName: s.displayName || s.name,
    model: s.model.name,
    agg: s.agg,
    expr: s.agg === 'count' ? '' : s.expr,
    timeColumn: s.timeColumn,
    dimensions: s.dimensions,
    unit: s.unit,
    description: '',
  };
}

