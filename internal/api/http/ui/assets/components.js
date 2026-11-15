/* GoDW 控制台 · 设计系统与通用业务组件（T0901 / T0902）
 *
 * 为什么不用 React/Vite：
 *   控制台必须「一个二进制跑起来就能用」，不能要求用户先装 node 再 build。
 *   所以全部用原生 DOM + 模板字符串，代价是组件要自己写，收益是零构建、零依赖、
 *   升级后端时前端跟着 go:embed 一起走。
 *
 * 本文件提供：
 *   Theme / I18N / UI（三态）      —— T0901
 *   ProTable / SchemaForm / SqlEditor / ConfirmDanger —— T0902
 *   WS（工作空间切换）/ GlobalSearch —— T0901 + T0210
 */
'use strict';

/* ============================== 主题 ============================== */

const Theme = {
  KEY: 'godw.theme',
  get() {
    try { return localStorage.getItem(this.KEY) || 'dark'; } catch (e) { return 'dark'; }
  },
  apply(name) {
    document.documentElement.setAttribute('data-theme', name === 'light' ? 'light' : 'dark');
    try { localStorage.setItem(this.KEY, name); } catch (e) { /* 隐私模式 */ }
    const btn = document.getElementById('theme-btn');
    if (btn) {
      // 按钮内有「图标 + 文字」两段，窄屏时 CSS 只留图标，所以分别更新
      const ico = btn.querySelector('.ico');
      const lbl = btn.querySelector('.lbl');
      if (ico) ico.textContent = name === 'light' ? '☾' : '☀';
      if (lbl) lbl.textContent = name === 'light' ? '暗色' : '亮色';
      btn.title = name === 'light' ? '切换到暗色主题' : '切换到亮色主题';
    }
  },
  toggle() {
    this.apply(this.get() === 'light' ? 'dark' : 'light');
    document.dispatchEvent(new CustomEvent('godw:theme', { detail: this.get() }));
  },
  init() { this.apply(this.get()); },
};

/* ============================== 多语言 ============================== */
/* en 只预留 key，未翻译的回落中文：宁可看到中文，也不要看到裸 key。 */
const I18N = {
  lang: 'zh-CN',
  dict: {
    'nav.overview': ['工作台', 'Overview'],
    'nav.instances': ['运行实例', 'Runs'],
    'nav.sources': ['数据源', 'Data Sources'],
    'nav.datasets': ['表结构', 'Tables'],
    'nav.models': ['模型', 'Models'],
    'nav.pipelines': ['管道', 'Pipelines'],
    'nav.metrics': ['指标', 'Metrics'],
    'nav.dashboards': ['看板', 'Dashboards'],
    'nav.lineage': ['血缘', 'Lineage'],
    'nav.quality': ['质量', 'Quality'],
    'nav.ops': ['运维', 'Operations'],
    'nav.settings': ['设置', 'Settings'],
    'common.search': ['搜索模型、表、管道…', 'Search models, tables, pipelines…'],
    'common.save': ['保存', 'Save'],
    'common.cancel': ['取消', 'Cancel'],
    'common.confirm': ['确认', 'Confirm'],
    'common.delete': ['删除', 'Delete'],
    'common.export': ['导出', 'Export'],
    'common.refresh': ['刷新', 'Refresh'],
    'common.empty': ['暂无数据', 'No data'],
    'common.loading': ['加载中…', 'Loading…'],
    'common.retry': ['重试', 'Retry'],
    'common.columns': ['列设置', 'Columns'],
  },
  t(key) {
    const e = this.dict[key];
    if (!e) return key;
    return this.lang === 'en' ? (e[1] || e[0]) : e[0];
  },
};
const t = k => I18N.t(k);

/* ============================== 三态 ============================== */

const UI = {
  /** 骨架屏：n 行，widths 可选，模拟真实表格的错落宽度，避免像进度条。 */
  skeleton(n, widths) {
    const w = widths || [92, 78, 86, 64, 88, 72];
    let s = '<div class="stack" style="padding:6px 0">';
    for (let i = 0; i < (n || 6); i++) {
      s += `<div class="skel" style="width:${w[i % w.length]}%"></div>`;
    }
    return s + '</div>';
  },

  /** 空态：必须给「为什么空」和「下一步做什么」，否则用户只能干瞪眼。 */
  empty(opt) {
    opt = opt || {};
    return `<div class="empty-state">
      <div class="es-icon">${esc(opt.icon || '◇')}</div>
      <div class="es-title">${esc(opt.title || t('common.empty'))}</div>
      ${opt.desc ? `<div class="es-desc">${esc(opt.desc)}</div>` : ''}
      ${opt.action ? `<button class="primary sm" data-act="${esc(opt.action.act)}">${esc(opt.action.label)}</button>` : ''}
    </div>`;
  },

  /** 错误态：把后端原文完整露出（含 SQL 片段），这是排障的唯一线索。 */
  error(err, retryAct) {
    const msg = (err && err.message) || String(err || '未知错误');
    return `<div class="err-state">
      <div class="es-title">请求失败</div>
      <pre>${esc(msg)}</pre>
      ${retryAct ? `<button class="sm" data-act="${esc(retryAct)}">${t('common.retry')}</button>` : ''}
    </div>`;
  },

  /** 统一包一层：async 渲染函数里少写 try/catch。 */
  async guard(host, fn, retryAct) {
    host.innerHTML = this.skeleton(5);
    try {
      await fn();
    } catch (e) {
      host.innerHTML = this.error(e, retryAct);
      if (retryAct) {
        host.querySelector('[data-act="' + retryAct + '"]')?.addEventListener('click', () => {
          this.guard(host, fn, retryAct);
        });
      }
    }
  },
};

/* ============================== ProTable ============================== */

/**
 * ProTable(host, opts)
 *   columns: [{key, title, width, align, sortable, filter, render(row), exportValue(row), hidden}]
 *   rows: 数据数组
 *   rowKey: 行主键字段名，默认 'id'
 *   selectable / pageSize / empty / toolbar / onSelect / onRowClick
 *   exportName: 导出文件名（不含扩展名）
 *
 * 1 万行不卡的做法：分页默认 50；把 pageSize 设 0 时启用窗口渲染，
 * 只把可视区 ± 12 行写进 DOM，其余用上下两个占位行撑高度。
 */
function ProTable(host, opts) {
  opts = opts || {};
  const state = {
    rows: opts.rows || [],
    sortKey: opts.sortKey || '',
    sortDir: opts.sortDesc ? -1 : 1,
    filters: {},
    page: 1,
    pageSize: opts.pageSize === undefined ? 50 : opts.pageSize,
    hidden: {},
    selected: new Set(),
    winStart: 0,
  };
  (opts.columns || []).forEach(c => { if (c.hidden) state.hidden[c.key] = true; });

  const ROW_H = 33;       // 与 td 行高一致，虚拟滚动靠它算位置
  const WIN_BUFFER = 12;

  function visibleCols() {
    return (opts.columns || []).filter(c => !state.hidden[c.key]);
  }

  function processed() {
    let out = state.rows.filter(r => {
      for (const k in state.filters) {
        const kw = state.filters[k];
        if (!kw) continue;
        const col = (opts.columns || []).find(c => c.key === k);
        const raw = col && col.exportValue ? col.exportValue(r) : r[k];
        if (!String(raw == null ? '' : raw).toLowerCase().includes(kw.toLowerCase())) return false;
      }
      return true;
    });
    if (state.sortKey) {
      const col = (opts.columns || []).find(c => c.key === state.sortKey);
      out = out.slice().sort((a, b) => {
        const va = col && col.exportValue ? col.exportValue(a) : a[state.sortKey];
        const vb = col && col.exportValue ? col.exportValue(b) : b[state.sortKey];
        const na = Number(va), nb = Number(vb);
        const cmp = (!isNaN(na) && !isNaN(nb) && va !== '' && vb !== '')
          ? na - nb
          : String(va == null ? '' : va).localeCompare(String(vb == null ? '' : vb), 'zh-CN');
        return cmp * state.sortDir;
      });
    }
    return out;
  }

  function pageRows(all) {
    if (!state.pageSize) return all;
    const start = (state.page - 1) * state.pageSize;
    return all.slice(start, start + state.pageSize);
  }

  function cellHTML(col, row) {
    if (col.render) return col.render(row);
    const v = row[col.key];
    return v == null || v === '' ? '<span class="mute">—</span>' : esc(v);
  }

  function render() {
    const all = processed();
    const paged = pageRows(all);
    const cols = visibleCols();
    const key = opts.rowKey || 'id';
    const virt = state.pageSize === 0 && paged.length > 120;

    let bodyHTML = '';
    if (!paged.length) {
      bodyHTML = `<tr><td colspan="${cols.length + (opts.selectable ? 1 : 0)}" style="padding:0">
        ${UI.empty(opts.empty)}
      </td></tr>`;
    } else if (virt) {
      const maxStart = Math.max(0, paged.length - 1);
      const start = Math.min(state.winStart, maxStart);
      const end = Math.min(paged.length, start + Math.ceil(560 / ROW_H) + WIN_BUFFER * 2);
      bodyHTML = `<tr style="height:${start * ROW_H}px"><td colspan="${cols.length + (opts.selectable ? 1 : 0)}" style="padding:0;border:none"></td></tr>`;
      for (let i = start; i < end; i++) bodyHTML += rowHTML(paged[i], i, cols, key);
      bodyHTML += `<tr style="height:${(paged.length - end) * ROW_H}px"><td colspan="${cols.length + (opts.selectable ? 1 : 0)}" style="padding:0;border:none"></td></tr>`;
    } else {
      paged.forEach((r, i) => { bodyHTML += rowHTML(r, i, cols, key); });
    }

    const filterCols = (opts.columns || []).filter(c => c.filter);
    const totalPages = state.pageSize ? Math.max(1, Math.ceil(all.length / state.pageSize)) : 1;

    host.innerHTML = `
      <div class="dt-toolbar">
        ${opts.toolbar || ''}
        ${filterCols.map(c => `
          <input data-filter="${esc(c.key)}" placeholder="${esc('筛选 ' + c.title)}"
                 value="${esc(state.filters[c.key] || '')}" style="width:150px">`).join('')}
        <div class="spacer"></div>
        ${opts.selectable ? `<span class="hint" data-selcount>已选 ${state.selected.size} 项</span>` : ''}
        <div style="position:relative">
          <button class="sm ghost" data-pt="cols">${t('common.columns')}</button>
          ${colMenuHTML()}
        </div>
        <button class="sm ghost" data-pt="export">${t('common.export')}</button>
      </div>
      ${virt ? `<div class="virt-note">共 ${all.length} 行，已开启窗口渲染（仅渲染可视区）</div>` : ''}
      <div class="dt-scroll" data-pt="scroll">
        <table class="dt">
          <thead><tr>
            ${opts.selectable ? `<th style="width:34px"><input type="checkbox" data-pt="all"></th>` : ''}
            ${cols.map(c => `<th class="${c.sortable ? 'sortable' : ''} ${state.sortKey === c.key ? 'sorted' : ''}"
                 data-sort="${esc(c.key)}" style="${c.width ? 'width:' + c.width : ''};${c.align ? 'text-align:' + c.align : ''}">
                 ${esc(c.title)}${c.sortable ? `<span class="sort-mark">${state.sortKey === c.key ? (state.sortDir > 0 ? '▲' : '▼') : '⇅'}</span>` : ''}
               </th>`).join('')}
          </tr></thead>
          <tbody>${bodyHTML}</tbody>
        </table>
      </div>
      <div class="dt-foot">
        <div>共 ${all.length} 行${Object.keys(state.filters).some(k => state.filters[k]) ? '（已筛选）' : ''}</div>
        ${state.pageSize ? `<div class="row">
          <button class="sm ghost" data-pt="prev" ${state.page <= 1 ? 'disabled' : ''}>上一页</button>
          <span>${state.page} / ${totalPages}</span>
          <button class="sm ghost" data-pt="next" ${state.page >= totalPages ? 'disabled' : ''}>下一页</button>
          <select data-pt="psize" style="width:auto">
            ${[20, 50, 100, 200, 0].map(n => `<option value="${n}" ${state.pageSize === n ? 'selected' : ''}>${n || '全部'}</option>`).join('')}
          </select>
        </div>` : `<div class="row"><span class="hint">窗口渲染</span>
          <select data-pt="psize" style="width:auto">
            ${[20, 50, 100, 200, 0].map(n => `<option value="${n}" ${state.pageSize === n ? 'selected' : ''}>${n || '全部'}</option>`).join('')}
          </select></div>`}
      </div>`;

    bind();
  }

  function rowHTML(row, i, cols, key) {
    const id = row[key] == null ? String(i) : String(row[key]);
    const sel = state.selected.has(id);
    return `<tr class="${sel ? 'sel' : ''}" data-row="${esc(id)}">
      ${opts.selectable ? `<td><input type="checkbox" data-pt="row" data-id="${esc(id)}" ${sel ? 'checked' : ''}></td>` : ''}
      ${cols.map(c => `<td class="${c.wrap ? 'wrap' : ''}" style="${c.align ? 'text-align:' + c.align : ''}">${cellHTML(c, row)}</td>`).join('')}
    </tr>`;
  }

  function colMenuHTML() {
    const open = host.dataset.colsOpen === '1';
    if (!open) return '';
    return `<div class="dt-menu">${(opts.columns || []).map(c => `
      <label><input type="checkbox" data-col="${esc(c.key)}" ${state.hidden[c.key] ? '' : 'checked'}>${esc(c.title)}</label>`).join('')}
    </div>`;
  }

  function bind() {
    host.querySelectorAll('[data-sort]').forEach(th => {
      th.addEventListener('click', () => {
        const k = th.dataset.sort;
        const col = (opts.columns || []).find(c => c.key === k);
        if (!col || !col.sortable) return;
        if (state.sortKey === k) state.sortDir = -state.sortDir;
        else { state.sortKey = k; state.sortDir = 1; }
        render();
      });
    });
    host.querySelectorAll('[data-filter]').forEach(inp => {
      inp.addEventListener('input', () => {
        state.filters[inp.dataset.filter] = inp.value.trim();
        state.page = 1;
        const pos = inp.selectionStart;
        render();
        const again = host.querySelector(`[data-filter="${CSS.escape(inp.dataset.filter)}"]`);
        if (again) { again.focus(); again.setSelectionRange(pos, pos); }
      });
    });
    const scroll = host.querySelector('[data-pt="scroll"]');
    if (scroll && state.pageSize === 0) {
      let raf = 0;
      scroll.addEventListener('scroll', () => {
        if (raf) return;
        raf = requestAnimationFrame(() => {
          raf = 0;
          const start = Math.max(0, Math.floor(scroll.scrollTop / ROW_H) - WIN_BUFFER);
          if (Math.abs(start - state.winStart) > 2) { state.winStart = start; render(); scroll.scrollTop = scroll.scrollTop; }
        });
      });
    }
    host.querySelector('[data-pt="cols"]')?.addEventListener('click', (e) => {
      e.stopPropagation();
      host.dataset.colsOpen = host.dataset.colsOpen === '1' ? '0' : '1';
      render();
    });
    host.querySelectorAll('[data-col]').forEach(cb => {
      cb.addEventListener('change', () => {
        if (cb.checked) delete state.hidden[cb.dataset.col];
        else state.hidden[cb.dataset.col] = true;
        host.dataset.colsOpen = '1';
        render();
      });
    });
    // 列设置弹层：点空白处收起。render 会重建 DOM，先把上一版监听摘掉，避免叠加多份。
    if (host._colsMenuClose) {
      document.removeEventListener('click', host._colsMenuClose, true);
      host._colsMenuClose = null;
    }
    if (host.dataset.colsOpen === '1') {
      const onOutside = (e) => {
        if (host.contains(e.target)) return;
        host.dataset.colsOpen = '0';
        document.removeEventListener('click', onOutside, true);
        host._colsMenuClose = null;
        render();
      };
      host._colsMenuClose = onOutside;
      document.addEventListener('click', onOutside, true);
    }
    host.querySelector('[data-pt="export"]')?.addEventListener('click', () => exportCSV());
    host.querySelector('[data-pt="prev"]')?.addEventListener('click', () => { state.page--; render(); });
    host.querySelector('[data-pt="next"]')?.addEventListener('click', () => { state.page++; render(); });
    host.querySelector('[data-pt="psize"]')?.addEventListener('change', (e) => {
      state.pageSize = Number(e.target.value);
      state.page = 1; state.winStart = 0;
      render();
    });
    if (opts.selectable) {
      const all = host.querySelector('[data-pt="all"]');
      if (all) all.addEventListener('change', () => {
        const rows = pageRows(processed());
        const key = opts.rowKey || 'id';
        rows.forEach((r, i) => {
          const id = r[key] == null ? String(i) : String(r[key]);
          if (all.checked) state.selected.add(id); else state.selected.delete(id);
        });
        render();
        opts.onSelect && opts.onSelect([...state.selected]);
      });
      host.querySelectorAll('[data-pt="row"]').forEach(cb => {
        cb.addEventListener('change', () => {
          if (cb.checked) state.selected.add(cb.dataset.id); else state.selected.delete(cb.dataset.id);
          render();
          opts.onSelect && opts.onSelect([...state.selected]);
        });
      });
    }
    if (opts.onRowClick) {
      host.querySelectorAll('tbody tr[data-row]').forEach(tr => {
        tr.addEventListener('click', (e) => {
          if (e.target.tagName === 'INPUT' || e.target.tagName === 'BUTTON') return;
          opts.onRowClick(tr.dataset.row, state.rows.find(r => String(r[opts.rowKey || 'id']) === tr.dataset.row));
        });
      });
    }
  }

  function exportCSV() {
    const cols = visibleCols();
    const rows = processed();
    const cell = v => `"${String(v == null ? '' : v).replace(/"/g, '""')}"`;
    const lines = [cols.map(c => cell(c.title)).join(',')];
    rows.forEach(r => {
      lines.push(cols.map(c => cell(c.exportValue ? c.exportValue(r) : r[c.key])).join(','));
    });
    // BOM 不能省：不加 Excel 会把中文按 GBK 解，全成乱码。
    const blob = new Blob(['\uFEFF' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = (opts.exportName || 'export') + '.csv';
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 4000);
  }

  render();
  return {
    refresh(rows) { state.rows = rows || []; state.page = 1; state.winStart = 0; render(); },
    selected() { return [...state.selected]; },
    state,
  };
}

/* ============================== SchemaForm ============================== */

/**
 * SchemaForm(host, schema, value) -> {value(), validate(), setValue(v), fields}
 *   schema.fields: [{key, label, type, required, placeholder, desc, options, rows,
 *                    min, max, pattern, patternMsg, visibleIf(v)}]
 *   type: text | textarea | number | select | multiselect | switch | datetime | password | code
 *
 * multiselect 的原生 <select multiple> 需要 Ctrl 点选，在属性面板里几乎没人能一次选对；
 * 这里用勾选列表，值统一是数组。
 *
 * 与模型/管道/质量规则共用同一套表单：字段描述来自后端 JSON Schema，
 * 前端不再为每个表单写一遍 DOM，改字段只改 schema。
 */
function SchemaForm(host, schema, value) {
  const data = Object.assign({}, value || {});
  const fields = (schema.fields || []).filter(f => !f.visibleIf || f.visibleIf(data));
  const errors = {};

  function fieldHTML(f) {
    const v = data[f.key];
    const invalid = errors[f.key] ? ' invalid' : '';
    let inner = '';
    if (f.type === 'select') {
      inner = `<select data-f="${esc(f.key)}">
        ${(f.options || []).map(o => {
          const ov = typeof o === 'string' ? o : o.value;
          const ol = typeof o === 'string' ? o : (o.label || o.value);
          return `<option value="${esc(ov)}" ${String(v) === String(ov) ? 'selected' : ''}>${esc(ol)}</option>`;
        }).join('')}
      </select>`;
    } else if (f.type === 'multiselect') {
      const cur = Array.isArray(v) ? v.map(String) : String(v || '').split(',').map(s => s.trim()).filter(Boolean);
      const opts = f.options || [];
      inner = `<div class="fmulti" data-f="${esc(f.key)}">
        ${opts.length ? opts.map(o => {
          const ov = typeof o === 'string' ? o : o.value;
          const ol = typeof o === 'string' ? o : (o.label || o.value);
          return `<label class="fmulti-item"><input type="checkbox" value="${esc(ov)}" ${cur.includes(String(ov)) ? 'checked' : ''}><span>${esc(ol)}</span></label>`;
        }).join('') : `<div class="fmulti-empty">${esc(f.emptyText || '暂无可选项')}</div>`}
      </div>`;
    } else if (f.type === 'textarea' || f.type === 'code') {
      inner = `<textarea data-f="${esc(f.key)}" rows="${f.rows || 4}" placeholder="${esc(f.placeholder || '')}"
                style="${f.type === 'code' ? 'font-family:var(--mono)' : ''}">${esc(v == null ? '' : v)}</textarea>`;
    } else if (f.type === 'switch') {
      inner = `<select data-f="${esc(f.key)}">
        <option value="false" ${v ? '' : 'selected'}>否</option>
        <option value="true" ${v ? 'selected' : ''}>是</option>
      </select>`;
    } else {
      const type = f.type === 'number' ? 'number' : (f.type === 'password' ? 'password' : (f.type === 'datetime' ? 'datetime-local' : 'text'));
      inner = `<input type="${type}" data-f="${esc(f.key)}" value="${esc(v == null ? '' : v)}"
                placeholder="${esc(f.placeholder || '')}" ${f.min != null ? 'min=' + f.min : ''} ${f.max != null ? 'max=' + f.max : ''}>`;
    }
    return `<div class="sform-field${invalid}" data-field="${esc(f.key)}">
      <label>${esc(f.label || f.key)}${f.required ? '<span class="req">*</span>' : ''}</label>
      ${inner}
      ${f.desc ? `<div class="sform-desc">${esc(f.desc)}</div>` : ''}
      <div class="sform-err" data-err="${esc(f.key)}">${esc(errors[f.key] || '')}</div>
    </div>`;
  }

  function render() {
    host.innerHTML = `<div class="sform">${fields.map(fieldHTML).join('')}</div>`;
    host.querySelectorAll('[data-f]').forEach(el => {
      const key = el.dataset.f;
      const f = fields.find(x => x.key === key);
      // 多选容器本身没有 value，勾选状态要从内部 checkbox 收集；
      // 事件在容器上监听，靠 change 冒泡统一处理。
      if (f && f.type === 'multiselect') {
        el.addEventListener('change', () => {
          data[key] = [...el.querySelectorAll('input[type="checkbox"]:checked')].map(x => x.value);
          const box = host.querySelector(`[data-field="${CSS.escape(key)}"]`);
          const errBox = host.querySelector(`[data-err="${CSS.escape(key)}"]`);
          if (box && errBox) { box.classList.remove('invalid'); errBox.textContent = ''; }
        });
        return;
      }
      const ev = el.tagName === 'SELECT' ? 'change' : 'input';
      el.addEventListener(ev, () => {
        let v = el.value;
        if (f && f.type === 'number') v = v === '' ? '' : Number(v);
        if (f && f.type === 'switch') v = v === 'true';
        data[key] = v;
        const box = host.querySelector(`[data-field="${CSS.escape(key)}"]`);
        const errBox = host.querySelector(`[data-err="${CSS.escape(key)}"]`);
        if (box && errBox) { box.classList.remove('invalid'); errBox.textContent = ''; }
      });
    });
  }

  function validate() {
    fields.forEach(f => { delete errors[f.key]; });
    fields.forEach(f => {
      const v = data[f.key];
      if (f.required && (v === '' || v == null || (Array.isArray(v) && !v.length))) {
        errors[f.key] = (f.label || f.key) + ' 必填';
        return;
      }
      if (Array.isArray(v)) return; // 多选没有 pattern / 区间语义
      if (v !== '' && v != null && f.pattern && !new RegExp(f.pattern).test(String(v))) {
        errors[f.key] = f.patternMsg || '格式不正确';
      }
      if (v !== '' && v != null && f.min != null && Number(v) < f.min) errors[f.key] = '不能小于 ' + f.min;
      if (v !== '' && v != null && f.max != null && Number(v) > f.max) errors[f.key] = '不能大于 ' + f.max;
    });
    render();
    const first = Object.keys(errors)[0];
    if (first) {
      const box = host.querySelector(`[data-field="${CSS.escape(first)}"]`);
      box?.scrollIntoView({ block: 'center', behavior: 'smooth' });
      box?.querySelector('input,select,textarea')?.focus();
    }
    return Object.keys(errors).length === 0;
  }

  render();
  return {
    value: () => Object.assign({}, data),
    validate,
    setValue(v) { Object.assign(data, v); render(); },
    errors,
  };
}

/* ============================== SQL 编辑器 ============================== */

/**
 * SqlEditor(host, {value, readonly, height, completions, onChange})
 * completions: [{text, kind}]  kind ∈ table/column/func/keyword
 *
 * Monaco 要 build 才能用，代价与「零构建」冲突；这里用 textarea + 行号槽 +
 * 自绘补全弹层。补全词表由调用方从当前工作空间的元数据传入，
 * 保证「补全里只有真实存在的表」。
 */
function SqlEditor(host, opts) {
  opts = opts || {};
  const state = { value: opts.value || '', pop: null, popIdx: 0, popItems: [] };

  host.innerHTML = `<div class="sqled ${opts.readonly ? 'readonly' : ''}">
    <div class="sqled-bar">
      <span>SQL</span>
      <span class="spacer" style="flex:1"></span>
      ${opts.readonly ? '<span>只读</span>' : `<button class="sm ghost" data-sq="fmt">格式化</button>`}
    </div>
    <div class="sqled-body">
      <div class="sqled-gutter" data-sq="gutter">1</div>
      <textarea class="sqled-area" data-sq="area" spellcheck="false"
        style="height:${opts.height || 200}px" ${opts.readonly ? 'readonly' : ''}>${esc(state.value)}</textarea>
    </div>
  </div>`;

  const area = host.querySelector('[data-sq="area"]');
  const gutter = host.querySelector('[data-sq="gutter"]');

  function syncGutter() {
    const n = area.value.split('\n').length;
    let s = '';
    for (let i = 1; i <= n; i++) s += i + '\n';
    gutter.textContent = s;
    gutter.scrollTop = area.scrollTop;
  }

  function currentToken() {
    const pos = area.selectionStart;
    const before = area.value.slice(0, pos);
    const m = before.match(/[A-Za-z_][A-Za-z0-9_.]*$/);
    return m ? m[0] : '';
  }

  function showPop() {
    if (!opts.completions || opts.readonly) return;
    const tok = currentToken();
    if (tok.length < 2) return hidePop();
    const low = tok.toLowerCase();
    state.popItems = opts.completions
      .filter(c => c.text.toLowerCase().startsWith(low) && c.text.toLowerCase() !== low)
      .slice(0, 12);
    if (!state.popItems.length) return hidePop();
    state.popIdx = 0;
    let pop = host.querySelector('.sqled-pop');
    if (!pop) {
      pop = document.createElement('div');
      pop.className = 'sqled-pop';
      host.querySelector('.sqled-body').appendChild(pop);
    }
    pop.innerHTML = state.popItems.map((c, i) =>
      `<div class="${i === state.popIdx ? 'active' : ''}" data-ci="${i}">${esc(c.text)}<span class="kind">${esc(c.kind || '')}</span></div>`).join('');
    // 粗略定位在光标下方：不追求像素级准确，够用即可。
    const line = area.value.slice(0, area.selectionStart).split('\n').length;
    const col = area.value.slice(0, area.selectionStart).split('\n').pop().length;
    pop.style.left = Math.min(44 + col * 7.2, host.clientWidth - 200) + 'px';
    pop.style.top = Math.min(28 + line * 20.5, (opts.height || 200)) + 'px';
    pop.querySelectorAll('[data-ci]').forEach(el => {
      el.addEventListener('mousedown', (e) => { e.preventDefault(); accept(Number(el.dataset.ci)); });
    });
  }

  function hidePop() {
    host.querySelector('.sqled-pop')?.remove();
    state.popItems = [];
  }

  function accept(i) {
    const item = state.popItems[i];
    if (!item) return;
    const pos = area.selectionStart;
    const before = area.value.slice(0, pos);
    const tok = currentToken();
    const start = pos - tok.length;
    area.value = before.slice(0, start) + item.text + area.value.slice(pos);
    area.selectionStart = area.selectionEnd = start + item.text.length;
    state.value = area.value;
    hidePop();
    syncGutter();
    opts.onChange && opts.onChange(area.value);
    area.focus();
  }

  area.addEventListener('input', () => { state.value = area.value; syncGutter(); showPop(); opts.onChange && opts.onChange(area.value); });
  area.addEventListener('scroll', () => { gutter.scrollTop = area.scrollTop; });
  area.addEventListener('blur', () => setTimeout(hidePop, 140));
  area.addEventListener('keydown', (e) => {
    if (!state.popItems.length) return;
    if (e.key === 'ArrowDown') { e.preventDefault(); state.popIdx = (state.popIdx + 1) % state.popItems.length; showPop(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); state.popIdx = (state.popIdx - 1 + state.popItems.length) % state.popItems.length; showPop(); }
    else if (e.key === 'Enter' || e.key === 'Tab') { e.preventDefault(); accept(state.popIdx); }
    else if (e.key === 'Escape') { hidePop(); }
  });
  host.querySelector('[data-sq="fmt"]')?.addEventListener('click', () => {
    area.value = formatSQL(area.value);
    state.value = area.value;
    syncGutter();
    opts.onChange && opts.onChange(area.value);
  });

  syncGutter();
  return {
    value: () => area.value,
    setValue(v) { area.value = v; state.value = v; syncGutter(); },
  };
}

/** 极简 SQL 格式化：关键字大写 + 主句换行。够读即可，不做完整 AST 解析。 */
function formatSQL(sql) {
  const kw = ['select', 'from', 'left join', 'right join', 'inner join', 'join', 'where',
    'group by', 'order by', 'having', 'limit', 'union all', 'union', 'insert into', 'values', 'on'];
  let s = ' ' + String(sql || '').replace(/\s+/g, ' ').trim() + ' ';
  let out = s;
  kw.forEach(k => {
    const re = new RegExp('\\s' + k.replace(/ /g, '\\s+') + '\\s', 'gi');
    out = out.replace(re, '\n' + k.toUpperCase() + ' ');
  });
  return out.replace(/^\n/, '').replace(/\n{2,}/g, '\n').trim();
}

/** 行级 diff：LCS 求最长公共子序列，把新增/删除标出来。 */
function SqlDiff(host, left, right) {
  const a = String(left || '').split('\n');
  const b = String(right || '').split('\n');
  const n = a.length, m = b.length;
  const dp = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const L = [], R = [];
  let i = 0, j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) { L.push(['', a[i]]); R.push(['', b[j]]); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) { L.push(['del', a[i]]); i++; }
    else { R.push(['add', b[j]]); j++; }
  }
  while (i < n) { L.push(['del', a[i]]); i++; }
  while (j < m) { R.push(['add', b[j]]); j++; }

  const col = lines => lines.map(([cls, txt], k) =>
    `<div class="line ${cls}"><span class="ln">${k + 1}</span><span>${esc(txt) || '&nbsp;'}</span></div>`).join('');

  host.innerHTML = `<div class="sqled">
    <div class="sqled-bar"><span>旧</span><span class="spacer" style="flex:1"></span>
      <span>${L.filter(x => x[0] === 'del').length} 行删除</span></div>
    <div class="sqled-diff">
      <div class="dl">${col(L)}</div>
      <div>${col(R)}</div>
    </div>
    <div class="sqled-bar"><span>新</span><span class="spacer" style="flex:1"></span>
      <span>${R.filter(x => x[0] === 'add').length} 行新增</span></div>
  </div>`;
}

/* ============================== 危险确认 ============================== */

/**
 * ConfirmDanger({title, danger, resource, reason, onOK})
 * 必须手输 resource 名称才放行；可选填「变更原因」，一起写进审计。
 * 交互上刻意不做「回车即确认」，避免误触不可逆操作。
 */
function ConfirmDanger(opt) {
  const reasonLabel = opt.reason === false ? null : (opt.reasonLabel || '变更原因（写入审计）');
  modal(opt.title || '危险操作', `
    <div class="danger-modal">
      <div class="dl-warn">${opt.danger || '此操作不可撤销，请确认影响范围。'}</div>
      <label>请手输 <code>${esc(opt.resource)}</code> 以确认</label>
      <input id="dg-input" autocomplete="off" placeholder="${esc(opt.resource)}" style="margin-bottom:14px">
      ${reasonLabel ? `<label>${esc(reasonLabel)}</label>
        <input id="dg-reason" placeholder="例如：修复上游重复数据，工单 DW-142" style="margin-bottom:14px">` : ''}
      <div class="row">
        <button class="danger" id="dg-ok" disabled>确认执行</button>
        <button class="ghost" data-act="close-modal">取消</button>
      </div>
    </div>`);
  const inp = document.getElementById('dg-input');
  const btn = document.getElementById('dg-ok');
  inp.focus();
  inp.addEventListener('input', () => { btn.disabled = inp.value.trim() !== opt.resource; });
  inp.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !btn.disabled) btn.click(); });
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    btn.textContent = '执行中…';
    try {
      await opt.onOK(document.getElementById('dg-reason')?.value.trim() || '');
      closeModal();
    } catch (e) {
      toast(e.message || '执行失败', 'err');
      btn.disabled = false;
      btn.textContent = '确认执行';
    }
  });
}

/* ============================== 工作空间切换 ============================== */

const WS = {
  KEY: 'godw.ws',
  list: [],
  current: '',
  get() {
    try { return localStorage.getItem(this.KEY) || this.current || ''; } catch (e) { return this.current; }
  },
  set(id) {
    this.current = id;
    try { localStorage.setItem(this.KEY, id); } catch (e) { /* ignore */ }
    // 切换工作空间必须全站刷新：缓存、选中态、列表全都属于旧空间。
    document.dispatchEvent(new CustomEvent('godw:workspace', { detail: id }));
  },
  async load() {
    try {
      const r = await api('/workspaces');
      this.list = (r && (r.items || r)) || [];
      if (!Array.isArray(this.list)) this.list = [];
      const saved = this.get();
      const hit = this.list.find(w => w.id === saved) || this.list[0];
      this.current = hit ? hit.id : '';
      if (hit && hit.id !== saved) this.set(hit.id);
    } catch (e) { /* 未登录时静默，登录后再取 */ }
    return this.list;
  },
  render(host) {
    if (!host) return;
    if (!this.list.length) { host.innerHTML = ''; return; }
    host.innerHTML = `<div class="ws-switch">
      <select id="ws-select" title="切换工作空间">
        ${this.list.map(w => `<option value="${esc(w.id)}" ${w.id === this.current ? 'selected' : ''}>${esc(w.name)}</option>`).join('')}
      </select>
    </div>`;
    host.querySelector('#ws-select')?.addEventListener('change', (e) => {
      this.set(e.target.value);
      toast('已切换工作空间，正在刷新');
      setTimeout(() => location.reload(), 300);
    });
  },
};

/* ============================== 全局搜索 ============================== */

const GlobalSearch = {
  seq: 0,
  timer: null,

  mount(host) {
    host.innerHTML = `<div class="searchbox">
      <span class="sicon">⌕</span>
      <input id="gs-input" placeholder="${esc(t('common.search'))}" autocomplete="off">
      <div id="gs-pop"></div>
    </div>`;
    const inp = host.querySelector('#gs-input');
    const pop = host.querySelector('#gs-pop');

    inp.addEventListener('input', () => {
      clearTimeout(this.timer);
      const q = inp.value.trim();
      if (!q) { pop.innerHTML = ''; return; }
      // 250ms 防抖：搜索是全文扫描，不能每键一次。
      this.timer = setTimeout(() => this.run(q, pop), 250);
    });
    inp.addEventListener('keydown', (e) => {
      const hits = [...pop.querySelectorAll('.search-hit')];
      if (!hits.length) {
        if (e.key === 'Enter') { this.run(inp.value.trim(), pop, true); }
        return;
      }
      const cur = hits.findIndex(h => h.classList.contains('active'));
      if (e.key === 'ArrowDown') { e.preventDefault(); hits.forEach(h => h.classList.remove('active')); hits[Math.min(hits.length - 1, cur + 1)].classList.add('active'); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); hits.forEach(h => h.classList.remove('active')); hits[Math.max(0, cur - 1)].classList.add('active'); }
      else if (e.key === 'Enter') {
        e.preventDefault();
        (hits[cur] || hits[0]).click();
      } else if (e.key === 'Escape') { pop.innerHTML = ''; inp.blur(); }
    });
    document.addEventListener('click', (e) => {
      if (!host.contains(e.target)) pop.innerHTML = '';
    });
    // Ctrl/Cmd+K 聚焦搜索，减少鼠标往返
    document.addEventListener('keydown', (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); inp.focus(); inp.select(); }
    });
  },

  async run(q, pop, jumpFirst) {
    if (!q) return;
    const seq = ++this.seq;
    pop.innerHTML = '<div style="padding:10px">' + UI.skeleton(3) + '</div>';
    let data;
    try {
      data = await api('/search?q=' + encodeURIComponent(q) + '&limit=20');
    } catch (e) {
      pop.innerHTML = `<div style="padding:10px" class="err-box">${esc(e.message)}</div>`;
      return;
    }
    if (seq !== this.seq) return; // 丢弃过期响应，避免后发先至的乱序覆盖
    const items = (data && data.items) || [];
    if (!items.length) {
      pop.innerHTML = `<div style="padding:14px" class="hint">没有匹配「${esc(q)}」的元数据。试试表名、模型名或标签。</div>`;
      return;
    }
    pop.innerHTML = `<div class="search-pop">${items.map((h, i) => `
      <div class="search-hit ${i === 0 ? 'active' : ''}" data-type="${esc(h.type)}" data-name="${esc(h.name)}">
        <div class="sh-top">
          <span class="pill mute">${esc(typeLabel(h.type))}</span>
          <span class="sh-name">${h.highlight || esc(h.name)}</span>
          ${h.layer ? `<span class="pill info">${esc(h.layer.toUpperCase())}</span>` : ''}
          ${(h.tags || []).slice(0, 3).map(tg => `<span class="tag sm">${esc(tg)}</span>`).join('')}
        </div>
        ${h.comment && !h.highlight ? `<div class="sh-hl">${esc(h.comment)}</div>` : ''}
      </div>`).join('')}</div>`;
    pop.querySelectorAll('.search-hit').forEach(el => {
      el.addEventListener('click', () => {
        pop.innerHTML = '';
        openEntity(el.dataset.type, el.dataset.name);
      });
    });
    if (jumpFirst) pop.querySelector('.search-hit')?.click();
  },
};

function typeLabel(tp) {
  return ({ model: '模型', dataset: '表', pipeline: '管道', chart: '图表' })[tp] || tp;
}

/** 搜索结果跳转：把「搜到了」变成「到了那儿」。 */
function openEntity(type, name) {
  if (type === 'model') { location.hash = '#/models'; sessionStorage.setItem('godw.openModel', name); }
  else if (type === 'dataset') { location.hash = '#/datasets'; sessionStorage.setItem('godw.openTable', name); }
  else if (type === 'pipeline') { location.hash = '#/pipelines'; sessionStorage.setItem('godw.openPipeline', name); }
  else if (type === 'chart') { location.hash = '#/dashboards'; sessionStorage.setItem('godw.openChart', name); }
  else location.hash = '#/overview';
  document.dispatchEvent(new CustomEvent('godw:open', { detail: { type, name } }));
}

/* ============================== 环境条 ============================== */

/**
 * 环境条：prod 红、staging 橙、dev 不显示。
 *
 * 为什么值得单独做：控制台上一个「全量重跑」按钮，在 prod 点错和在 dev 点错
 * 是两回事。颜色条是最便宜、最难被忽略的防线。
 */
const EnvBar = {
  async load() {
    let env = 'dev';
    try {
      const me = await api('/me');
      env = (me && me.env) || 'dev';
    } catch (e) { return; }
    const bar = document.getElementById('env-banner');
    // dev 是默认环境，不加视觉噪音；只有 staging/prod 才顶一条警示带。
    if (env === 'dev') {
      document.body.removeAttribute('data-env');
      if (bar) bar.textContent = '';
      return;
    }
    document.body.setAttribute('data-env', env);
    if (!bar) return;
    bar.innerHTML = `<span>⚠ 当前环境：<b>${env.toUpperCase()}</b>　所有变更会直接作用于该环境的数据，请确认操作对象</span>`;
  },
};

/* ============================== 启动 ============================== */

Theme.init();

document.addEventListener('DOMContentLoaded', () => {
  document.getElementById('theme-btn')?.addEventListener('click', () => Theme.toggle());
  const gsHost = document.getElementById('global-search');
  if (gsHost) GlobalSearch.mount(gsHost);
  const wsHost = document.getElementById('ws-host');
  if (wsHost) {
    WS.load().then(() => WS.render(wsHost));
    document.addEventListener('godw:workspace', () => WS.render(wsHost));
  }
  EnvBar.load();
});
