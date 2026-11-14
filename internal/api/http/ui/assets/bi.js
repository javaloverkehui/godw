/* GoDW 控制台 · BI 图表编辑器（T0909）
 *
 * 与「看板」的分工：
 *   看板（dashboards）= 指标卡片的组合，偏运营日报；
 *   BI 图表（bi）    = 数据集 + 编码槽的图表画布，偏自助分析。
 * 两者共用语义层，但保存单元不同：这里存 Chart（encoding），看板存 Card（指标）。
 *
 * 设计取舍：
 *   - 图表用 SVG 自绘，不引 ECharts：离线可用、零依赖、与主题变量天然一致。
 *   - 编码槽是「单值槽」而不是列表，因为后端 Chart.Encoding 就是一对一映射，
 *     前端若当数组存，保存时必然丢字段。
 *   - 每次改动只重画当前卡片，不整页重渲染，避免输入时丢焦点。
 */
'use strict';

const BIE = {
  charts: [],
  datasets: [],
  cur: '',
  spec: null,
  filters: [],      // 页面级全局筛选，保存时并入每个 chart
  sql: '',
  elapsed: 0,
  error: '',
};

Object.assign(ROUTES, {
  bi: { title: 'BI 图表', sub: '数据集编码槽、实时预览与全局筛选', render: pageBI },
});

/* 自绘图表的注册表：主题切换或尺寸变化后按原入参重放一遍。
 * chart.js 的折线图有自己的重绘机制，这里管的是 bi.js 自绘的 7 种类型。 */
const BI_CHARTS = new Set();
function biReplay() {
  BI_CHARTS.forEach(svg => {
    if (!svg.isConnected) { BI_CHARTS.delete(svg); return; }
    const a = svg.__biArgs;
    if (a) drawChart(svg, a.type, a.series, a.points);
  });
}
document.addEventListener('godw:theme', biReplay);
let biResizeTimer = null;
window.addEventListener('resize', () => {
  clearTimeout(biResizeTimer);
  biResizeTimer = setTimeout(biReplay, 200);
});

async function pageBI(view) {
  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">BI 图表</h2>
        <p class="page-desc">左侧选图表，中间实时预览，右侧把字段拖进编码槽。预览走真实语义查询，不是占位图。</p>
      </div>
      <div class="row">
        <button class="sm ghost" data-act="bi-new">新建图表</button>
        <button class="sm ghost" data-act="bi-run">重新查询</button>
        <button class="sm primary" data-act="bi-save">保存</button>
      </div>
    </div>
    <div class="split3" style="grid-template-columns:220px 1fr 320px">
      <div class="card" style="padding:12px">
        <input id="bi-search" placeholder="搜索图表" style="margin-bottom:10px">
        <div class="tree" id="bi-list">${UI.skeleton(6)}</div>
      </div>
      <div class="card" id="bi-canvas">${UI.empty({ title: '还没有图表', desc: '点右上角「新建图表」，或从左侧选一个' })}</div>
      <div class="card" id="bi-config">${UI.empty({ title: '配置', desc: '选中图表后配置编码槽' })}</div>
    </div>`;

  view.querySelector('[data-act="bi-new"]').addEventListener('click', () => newChartModal(view));
  view.querySelector('[data-act="bi-run"]').addEventListener('click', () => runChart());
  view.querySelector('[data-act="bi-save"]').addEventListener('click', () => saveChart());
  view.querySelector('#bi-search').addEventListener('input', () => renderChartList(view));

  await Promise.all([loadChartList(view), loadSemanticDatasets()]);

  const want = sessionStorage.getItem('godw.openChart');
  if (want) {
    sessionStorage.removeItem('godw.openChart');
    openChart(view, want);
    return;
  }
  // 左侧已经有图表却停在「还没有图表」的空态，是最常见的「这页是不是坏了」来源。
  // 没有指定打开哪张时，默认打开列表第一张。
  const first = (BIE.charts || [])[0];
  if (first) openChart(view, first.name);
}

async function loadChartList(view) {
  try {
    BIE.charts = await api('/charts');
  } catch (e) {
    view.querySelector('#bi-list').innerHTML = UI.error(e);
    return;
  }
  renderChartList(view);
}

function renderChartList(view) {
  const host = view.querySelector('#bi-list');
  if (!host) return;
  const kw = (view.querySelector('#bi-search').value || '').trim().toLowerCase();
  const list = (BIE.charts || []).filter(c => !kw || c.name.toLowerCase().includes(kw));
  if (!list.length) {
    host.innerHTML = '<div class="hint" style="padding:8px">' + (BIE.charts.length ? '无匹配' : '暂无图表') + '</div>';
    return;
  }
  host.innerHTML = list.map(c => `<div class="tree-node tree-l1 ${BIE.cur === c.name ? 'active' : ''}" data-chart="${esc(c.name)}">
    <span class="tw"></span><span class="ti">▦</span>
    <span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis" title="${esc(c.name)}">${esc(c.name)}</span>
    <span class="tag sm" style="flex:none">${esc(c.type || '')}</span>
  </div>`).join('');
  host.querySelectorAll('[data-chart]').forEach(n => {
    n.addEventListener('click', () => openChart(view, n.dataset.chart));
  });
}

async function loadSemanticDatasets() {
  try { BIE.datasets = await api('/datasets/semantic'); } catch (e) { BIE.datasets = []; }
}

function openChart(view, name) {
  const c = (BIE.charts || []).find(x => x.name === name);
  if (!c) return toast('图表不存在', 'err');
  BIE.cur = name;
  BIE.spec = JSON.parse(JSON.stringify(c));
  BIE.spec.encoding = BIE.spec.encoding || {};
  BIE.spec.dimensions = BIE.spec.dimensions || [];
  BIE.spec.measures = BIE.spec.measures || [];
  BIE.filters = JSON.parse(JSON.stringify(c.filters || []));
  BIE.sql = '';
  BIE.error = '';
  renderChartList(view);
  renderChartConfig(view);
  runChart();
}

/* ---------------- 画布 ---------------- */

async function runChart() {
  const host = document.getElementById('bi-canvas');
  if (!host) return;
  if (!BIE.cur) { host.innerHTML = UI.empty({ title: '还没有图表', desc: '先新建一个' }); return; }
  host.innerHTML = `<div class="card-head"><div class="card-title">${esc(BIE.spec.title || BIE.cur)}</div></div>${UI.skeleton(6)}`;
  let res;
  try {
    res = await api('/charts/' + encodeURIComponent(BIE.cur) + '/query');
  } catch (e) {
    BIE.error = e.message;
    renderCanvasError(host, e.message);
    return;
  }
  BIE.error = '';
  BIE.sql = res.sql || '';
  BIE.elapsed = res.elapsedMs || 0;
  renderCanvas(host, res);
}

function renderCanvasError(host, msg) {
  host.innerHTML = `<div class="card-head"><div class="card-title">${esc(BIE.spec && (BIE.spec.title || BIE.cur))}</div></div>
    ${UI.error({ message: msg })}
    <div class="hint" style="margin-top:10px">检查数据集是否已发布、编码槽字段是否属于该数据集。</div>`;
}

function renderCanvas(host, res) {
  const c = BIE.spec || {};
  const series = res.series || [];
  const points = res.points || [];
  const hasData = series.some(s => (s.data || []).length > 0);

  host.innerHTML = `
    <div class="card-head">
      <div>
        <div class="card-title">${esc(c.title || BIE.cur)}</div>
        <div class="hint">${series.length} 个系列 · ${points.length} 个数据点 · ${BIE.elapsed}ms</div>
      </div>
      <div class="row">
        <span class="tag">${esc(c.type || '')}</span>
        <button class="sm ghost" data-act="bi-sql">查看 SQL</button>
      </div>
    </div>
    ${hasData ? '<div id="bi-chart-host"></div>' : UI.empty({
      title: '查询没有返回数据',
      desc: '数据集可能为空，或全局筛选把数据过滤掉了。可以先去掉筛选再试。',
    })}
    <div id="bi-sqlbox" style="display:none;margin-top:14px"></div>`;

  host.querySelector('[data-act="bi-sql"]').addEventListener('click', () => {
    const box = document.getElementById('bi-sqlbox');
    if (box.style.display === 'none') {
      box.style.display = '';
      box.innerHTML = '';
      SqlEditor(box, { value: BIE.sql || '-- 无 SQL', readonly: true, height: 160 });
    } else {
      box.style.display = 'none';
    }
  });

  if (!hasData) return;
  const chartHost = host.querySelector('#bi-chart-host');
  chartHost.innerHTML = `<svg id="bi-svg" height="320" style="width:100%"></svg>`;
  drawChart(document.getElementById('bi-svg'), c.type, series, points);
}

/** 自绘图表：折线/柱/饼/指标卡/表格。坐标轴与网格跟着主题变量走。 */
function drawChart(svg, type, series, points) {
  if (!svg) return;
  // 主题色取自 CSS 变量：亮色主题只覆盖变量，这里不需要任何分支。
  // 同时记下入参，切换主题时按原数据重放一遍，否则图会停在旧主题的配色上。
  svg.__biArgs = { type, series, points };
  BI_CHARTS.add(svg);
  const C = chartPalette();
  const PAL = C.series;
  // SVG 元素不写宽度时默认只有 300px，viewBox 会按 300 算：
  // 放进整行卡片就是"图缩在左边、字号被压小"。这里先撑满再量真实像素。
  svg.setAttribute('width', '100%');
  const rect = svg.getBoundingClientRect();
  const W = Math.max(320, Math.round(rect.width || svg.clientWidth || 760));
  const H = Number(svg.getAttribute('height')) || 320;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  const PAD = { l: 56, r: 18, t: 18, b: 42 };

  const all = series.flatMap(s => s.data || []);
  let maxY = Math.max(...all, 0);
  let minY = Math.min(...all, 0);
  // 全 0（或单一值）时给一个最小量程，否则刻度会算出 -0.25 这种负数。
  if (maxY === minY) maxY = minY + 1;
  const labels = [...new Set(points.map(p => String(p.x)))];

  if (type === 'kpi') {
    const v = all.length ? all.reduce((a, b) => a + b, 0) : 0;
    svg.innerHTML = `<text x="${W / 2}" y="${H / 2}" text-anchor="middle" font-size="42" fill="${C.text}">${esc(fmtNum(v))}</text>
      <text x="${W / 2}" y="${H / 2 + 30}" text-anchor="middle" font-size="13" fill="${C.axis}">${esc(series[0] ? series[0].name : '')}</text>`;
    return;
  }
  if (type === 'table') {
    const rows = points.slice(0, 14);
    svg.innerHTML = rows.map((p, i) => `<text x="16" y="${28 + i * 20}" font-size="12" fill="${C.text}">${esc(p.x)}</text>
      <text x="${W - 16}" y="${28 + i * 20}" text-anchor="end" font-size="12" fill="${C.accent}">${esc(fmtNum(p.y))}</text>`).join('');
    return;
  }
  if (type === 'pie' || type === 'donut') {
    // 饼图的扇区是「维度取值」，不是「系列」：未设 series 时后端只返回一个系列，
    // 按系列画会退化成一整块。所以这里按 x 聚合 points。
    const byX = new Map();
    points.forEach(p => byX.set(String(p.x), (byX.get(String(p.x)) || 0) + Number(p.y || 0)));
    const items = [...byX.entries()].filter(([, v]) => v > 0);
    if (!items.length) {
      svg.innerHTML = `<text x="${W / 2}" y="${H / 2}" text-anchor="middle" font-size="12" fill="${C.axis}">没有可展示的正值</text>`;
      return;
    }
    const total = items.reduce((a, [, v]) => a + v, 0) || 1;
    const R = Math.min(H, W * 0.6) / 2 - 26;
    // 饼图 + 右侧图例作为一个整体居中。整行卡片里若固定靠左（原来的 min(W*0.42,240)），
    // 右半边会空出一大片，看起来像没画完。
    const legendW = 186;
    const cx = Math.max(R + 8, Math.round((W - (R * 2 + 26 + legendW)) / 2 + R));
    const cy = H / 2;
    let acc = 0;
    const arcs = items.map(([name, v], i) => {
      const a0 = acc / total * Math.PI * 2 - Math.PI / 2;
      acc += v;
      const a1 = acc / total * Math.PI * 2 - Math.PI / 2;
      const large = a1 - a0 > Math.PI ? 1 : 0;
      const x0 = cx + R * Math.cos(a0), y0 = cy + R * Math.sin(a0);
      const x1 = cx + R * Math.cos(a1), y1 = cy + R * Math.sin(a1);
      return `<path d="M ${cx} ${cy} L ${x0} ${y0} A ${R} ${R} 0 ${large} 1 ${x1} ${y1} Z"
        fill="${PAL[i % PAL.length]}" opacity=".88">
        <title>${esc(name || '(空值)')}: ${fmtNum(v)}（${(v / total * 100).toFixed(1)}%）</title></path>`;
    }).join('');
    const hole = type === 'donut' ? `<circle cx="${cx}" cy="${cy}" r="${R * 0.56}" fill="${C.card}"/>` : '';
    // 图例放在右侧，带百分比——饼图不标数值等于没画。
    const legend = items.map(([name, v], i) => `
      <rect x="${cx + R + 26}" y="${28 + i * 20 - 9}" width="9" height="9" rx="2" fill="${PAL[i % PAL.length]}"/>
      <text x="${cx + R + 41}" y="${28 + i * 20}" font-size="11.5" fill="${C.text}">${esc((String(name) || '(空值)').slice(0, 12))}</text>
      <text x="${cx + R + 26 + legendW}" y="${28 + i * 20}" text-anchor="end" font-size="11.5" fill="${C.axis}">${(v / total * 100).toFixed(1)}%</text>`).join('');
    svg.innerHTML = arcs + hole + legend;
    return;
  }

  // 折线 / 柱状：共用坐标系
  const plotW = W - PAD.l - PAD.r;
  const plotH = H - PAD.t - PAD.b;
  const spanY = (maxY - minY) || 1;
  const yOf = v => PAD.t + plotH - ((v - minY) / spanY) * plotH;
  const xOf = i => PAD.l + (labels.length <= 1 ? plotW / 2 : (i / (labels.length - 1)) * plotW);

  let g = '';
  // 网格 + Y 轴刻度
  for (let i = 0; i <= 4; i++) {
    const y = PAD.t + (plotH / 4) * i;
    const val = maxY - (spanY / 4) * i;
    g += `<line x1="${PAD.l}" y1="${y}" x2="${W - PAD.r}" y2="${y}" stroke="${C.grid}" stroke-width="1"/>`;
    g += `<text x="${PAD.l - 8}" y="${y + 4}" text-anchor="end" font-size="10.5" fill="${C.axis}">${esc(fmtNum(val))}</text>`;
  }
  // X 轴标签（最多 8 个，避免重叠）
  const step = Math.max(1, Math.ceil(labels.length / 8));
  labels.forEach((lb, i) => {
    if (i % step) return;
    g += `<text x="${xOf(i)}" y="${H - PAD.b + 16}" text-anchor="middle" font-size="10.5" fill="${C.axis}">${esc(lb.length > 10 ? lb.slice(0, 10) + '…' : lb)}</text>`;
  });

  if (type === 'bar' || type === 'stack_bar') {
    const bw = Math.max(3, (plotW / Math.max(1, labels.length)) * 0.6);
    series.forEach((s, si) => {
      (s.data || []).forEach((v, i) => {
        const x = xOf(i) - bw / 2 + (si * bw * 0.5);
        const y = yOf(Math.max(v, 0));
        const h = Math.abs(yOf(v) - yOf(Math.max(minY, 0)));
        g += `<rect x="${x}" y="${y}" width="${bw * 0.9}" height="${Math.max(1, h)}" rx="2"
          fill="${PAL[si % PAL.length]}" opacity=".88"><title>${esc(s.name)} ${esc(labels[i])}: ${fmtNum(v)}</title></rect>`;
      });
    });
  } else {
    series.forEach((s, si) => {
      const d = (s.data || []).map((v, i) => `${i === 0 ? 'M' : 'L'} ${xOf(i)} ${yOf(v)}`).join(' ');
      g += `<path d="${d}" fill="none" stroke="${PAL[si % PAL.length]}" stroke-width="2" stroke-linejoin="round"/>`;
      (s.data || []).forEach((v, i) => {
        g += `<circle cx="${xOf(i)}" cy="${yOf(v)}" r="2.6" fill="${PAL[si % PAL.length]}"><title>${esc(s.name)} ${esc(labels[i])}: ${fmtNum(v)}</title></circle>`;
      });
    });
  }
  // 图例
  if (series.length > 1) {
    g += series.map((s, i) => `<rect x="${PAD.l + i * 96}" y="4" width="9" height="9" rx="2" fill="${PAL[i % PAL.length]}"/>
      <text x="${PAD.l + i * 96 + 13}" y="12" font-size="10.5" fill="${C.label}">${esc(s.name.slice(0, 10))}</text>`).join('');
  }
  svg.innerHTML = g;
}

function fmtNum(v) {
  if (v == null || isNaN(v)) return '—';
  const n = Number(v);
  if (Math.abs(n) >= 1e8) return (n / 1e8).toFixed(2) + '亿';
  if (Math.abs(n) >= 1e4) return (n / 1e4).toFixed(1) + '万';
  if (Number.isInteger(n)) return n.toLocaleString();
  return n.toFixed(2);
}

/* ---------------- 配置面板 ---------------- */

function renderChartConfig(view) {
  const host = view.querySelector('#bi-config');
  const c = BIE.spec;
  if (!c) { host.innerHTML = UI.empty({ title: '配置', desc: '选中图表后配置' }); return; }

  const ds = (BIE.datasets || []).find(d => d.name === c.dataset);
  const dims = (ds && ds.dimensions || []).map(d => d.name);
  const measures = (ds && ds.measures || []).map(m => m.name);

  host.innerHTML = `
    <div class="card-title" style="margin-bottom:12px">图表配置</div>
    <div id="bi-form"></div>
    <div class="card-title" style="margin:16px 0 8px">编码槽</div>
    <div class="slot-list" id="bi-slots"></div>
    <div class="card-title" style="margin:16px 0 8px">字段</div>
    <div class="hint" style="margin-bottom:6px">拖到对应槽位（维度：${dims.length} 个 / 度量：${measures.length} 个）</div>
    <div class="row wrap" id="bi-pool"></div>
    <div class="card-title" style="margin:16px 0 8px">全局筛选</div>
    <div id="bi-filters"></div>`;

  const form = SchemaForm(host.querySelector('#bi-form'), {
    fields: [
      { key: 'title', label: '标题' },
      { key: 'type', label: '图表类型', type: 'select', options: [
        { value: 'line', label: '折线' }, { value: 'bar', label: '柱状' },
        { value: 'stack_bar', label: '堆叠柱' }, { value: 'pie', label: '饼图' },
        { value: 'donut', label: '环形' }, { value: 'kpi', label: '指标卡' },
        { value: 'table', label: '表格' },
      ] },
      { key: 'dataset', label: '数据集', type: 'select',
        options: [{ value: '', label: '（未选择）' }].concat((BIE.datasets || []).map(d => ({ value: d.name, label: d.name }))) },
    ],
  }, { title: c.title || '', type: c.type || 'line', dataset: c.dataset || '' });

  host.querySelectorAll('[data-f]').forEach(el => {
    el.addEventListener('change', () => {
      const v = form.value();
      c.title = v.title;
      c.type = v.type;
      // 切数据集会清空编码槽：字段名换了，旧映射必然失效，留着只会报错。
      if (v.dataset !== c.dataset) {
        c.dataset = v.dataset;
        c.encoding = {};
        c.dimensions = [];
        c.measures = [];
        renderChartConfig(view);
        return;
      }
      renderChartConfig(view);
    });
  });

  // 编码槽：单值槽，与后端 Chart.Encoding 一一对应
  const slots = [['x', 'X 轴 / 维度'], ['y', 'Y 轴 / 度量'], ['series', '系列 / 颜色']];
  host.querySelector('#bi-slots').innerHTML = slots.map(([k, label]) => `
    <div style="font-size:12px;color:var(--text-mute);margin-bottom:3px">${label}</div>
    <div class="slot" data-slot="${k}">
      ${c.encoding[k] ? `<span class="slot-chip">${esc(c.encoding[k])}<span class="x" data-rm="${k}">×</span></span>` : '<span>拖字段到这里</span>'}
    </div>`).join('');

  host.querySelector('#bi-pool').innerHTML =
    dims.map(d => `<span class="slot-chip" draggable="true" data-fld="${esc(d)}">${esc(d)}</span>`).join('') +
    measures.map(m => `<span class="slot-chip" draggable="true" data-fld="${esc(m)}" data-kind="measure" style="border-color:var(--info-line)">${esc(m)}</span>`).join('') ||
    '<span class="hint">先选择数据集</span>';

  host.querySelectorAll('#bi-pool [data-fld]').forEach(chip => {
    chip.addEventListener('dragstart', (e) => e.dataTransfer.setData('text/plain', chip.dataset.fld));
    // 点选也能入槽：不是所有人都习惯拖。
    chip.addEventListener('click', () => {
      // 用 data-kind 判定维度/度量，别靠「有没有内联边框色」——那是个会随主题改动的副作用。
      const k = chip.dataset.kind === 'measure' ? 'y' : 'x';
      setSlot(c, k, chip.dataset.fld, view);
    });
  });
  host.querySelectorAll('[data-slot]').forEach(slot => {
    slot.addEventListener('dragover', (e) => { e.preventDefault(); slot.classList.add('over'); });
    slot.addEventListener('dragleave', () => slot.classList.remove('over'));
    slot.addEventListener('drop', (e) => {
      e.preventDefault();
      slot.classList.remove('over');
      const f = e.dataTransfer.getData('text/plain');
      if (f) setSlot(c, slot.dataset.slot, f, view);
    });
  });
  host.querySelectorAll('[data-rm]').forEach(x => {
    x.addEventListener('click', () => {
      delete c.encoding[x.dataset.rm];
      syncFields(c);
      renderChartConfig(view);
    });
  });

  // 全局筛选
  host.querySelector('#bi-filters').innerHTML = (BIE.filters || []).map((f, i) => `
    <div class="row" style="margin-bottom:6px">
      <span class="slot-chip">${esc(f.field || '未选字段')} ${esc(f.op || '=')} ${esc(String(f.value == null ? '' : f.value))}
        <span class="x" data-frm="${i}">×</span></span>
    </div>`).join('') + `<button class="sm ghost" data-act="bi-fadd">+ 添加筛选</button>`;

  host.querySelector('[data-act="bi-fadd"]')?.addEventListener('click', () => {
    if (!dims.length) return toast('先选择数据集', 'err');
    const field = prompt('筛选字段（维度名）', dims[0]);
    if (!field) return;
    const value = prompt('筛选值（等于）');
    if (value == null) return;
    BIE.filters.push({ field, op: 'eq', value });
    renderChartConfig(view);
  });
  host.querySelectorAll('[data-frm]').forEach(x => {
    x.addEventListener('click', () => {
      BIE.filters.splice(Number(x.dataset.frm), 1);
      renderChartConfig(view);
    });
  });
}

/**
 * 设置编码槽。
 *
 * 关键点：槽位改动必须同步 dimensions / measures —— 后端 BuildQuery 只认这两个列表，
 * 光改 encoding 会出现「图上有字段、SQL 里没有」的假象。
 */
function setSlot(c, k, field, view) {
  c.encoding[k] = field;
  syncFields(c);
  renderChartConfig(view);
  if (BIE.cur) runChart();
}

function syncFields(c) {
  const ds = (BIE.datasets || []).find(d => d.name === c.dataset);
  const dimNames = new Set(((ds && ds.dimensions) || []).map(d => d.name));
  const measureNames = new Set(((ds && ds.measures) || []).map(m => m.name));
  const used = [c.encoding.x, c.encoding.series].filter(Boolean);
  c.dimensions = used.filter(f => dimNames.has(f));
  const ms = [c.encoding.y].filter(f => f && measureNames.has(f));
  if (!ms.length) {
    // Y 槽为空时退化为计数，避免「拖了维度就报错」的挫败感。
    const first = ((ds && ds.measures) || [])[0];
    if (first) ms.push(first.name);
  }
  c.measures = ms;
}

/* ---------------- 新建与保存 ---------------- */

function newChartModal(view) {
  if (!(BIE.datasets || []).length) {
    return toast('还没有已发布的数据集，先去语义层建一个', 'err');
  }
  modal('新建图表', `<div id="nc-form"></div>
    <div class="row" style="justify-content:flex-end;margin-top:14px">
      <button class="ghost" data-act="close-modal">取消</button>
      <button class="primary" id="nc-go">创建</button>
    </div>`);
  const form = SchemaForm(document.getElementById('nc-form'), {
    fields: [
      { key: 'name', label: '图表名', required: true, placeholder: 'gmv_trend',
        pattern: '^[a-z][a-z0-9_]*$', patternMsg: '小写字母开头，只含小写字母数字下划线' },
      { key: 'title', label: '标题' },
      { key: 'type', label: '类型', type: 'select', required: true,
        options: ['line', 'bar', 'stack_bar', 'pie', 'donut', 'kpi', 'table'] },
      { key: 'dataset', label: '数据集', type: 'select', required: true,
        options: BIE.datasets.map(d => ({ value: d.name, label: d.name })) },
    ],
  }, { name: '', title: '', type: 'line', dataset: BIE.datasets[0].name });

  document.getElementById('nc-go').addEventListener('click', async () => {
    if (!form.validate()) return;
    const v = form.value();
    const ds = BIE.datasets.find(d => d.name === v.dataset);
    const d0 = (ds.dimensions || [])[0];
    const m0 = (ds.measures || [])[0];
    const spec = {
      name: v.name, title: v.title || v.name, type: v.type, dataset: v.dataset,
      dimensions: d0 ? [d0.name] : [],
      measures: m0 ? [m0.name] : [],
      encoding: { x: d0 ? d0.name : '', y: m0 ? m0.name : '' },
      filters: BIE.filters,
    };
    try {
      await api('/charts', { method: 'POST', body: { spec } });
      closeModal();
      toast('已创建', 'ok');
      await loadChartList(view);
      openChart(view, v.name);
    } catch (e) { toast(e.message, 'err'); }
  });
}

async function saveChart() {
  if (!BIE.spec) return toast('先选一个图表', 'err');
  const body = JSON.parse(JSON.stringify(BIE.spec));
  if (!body.dimensions.length && !body.measures.length) {
    return toast('至少配一个维度或度量', 'err');
  }
  body.filters = BIE.filters;
  try {
    await api('/charts', { method: 'POST', body: { spec: body } });
    toast('已保存', 'ok');
    await loadChartList(document.getElementById('view'));
    runChart();
  } catch (e) { toast(e.message, 'err'); }
}

/* ============================== 自助查询 ==============================
 *
 * 与「BI 图表」的区别：图表是先配编码槽再查，这里是直接写 SQL。
 * 后端的边界（只读、单语句、15s、行数上限、行级策略不注入）必须在界面上写明，
 * 否则用户会以为它和数据集查询一样受行级权限保护。
 */

Object.assign(ROUTES, {
  sql: { title: '自助查询', sub: '只读 SQL 控制台', render: pageSQL },
});

const SQL_LIMITS = [100, 500, 2000, 5000];

async function pageSQL(view) {
  let sources = [];
  try { sources = await api('/datasources'); } catch (e) { sources = []; }

  const savedDs = sessionStorage.getItem('godw.sqlDs') || '';
  const cur = sources.some(s => s.name === savedDs) ? savedDs : (sources[0] ? sources[0].name : '');

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">自助查询</h2>
        <p class="page-desc">直接对数据源写 SQL。只读、单语句、超时 15 秒；行级策略不注入，列级脱敏按输出列名匹配。</p>
      </div>
      <div class="row">
        <select id="sql-ds" style="width:180px">
          ${sources.map(s => `<option value="${esc(s.name)}" ${s.name === cur ? 'selected' : ''}>${esc(s.name)} · ${esc(s.type)}</option>`).join('')
            || '<option value="">（还没有数据源）</option>'}
        </select>
        <select id="sql-limit" style="width:110px">
          ${SQL_LIMITS.map(n => `<option value="${n}" ${n === 500 ? 'selected' : ''}>${n} 行</option>`).join('')}
        </select>
        <button class="primary" data-act="sql-run">执行</button>
      </div>
    </div>

    <div class="card" style="padding:10px">
      <div id="sql-editor"></div>
      <div class="hint" style="margin-top:8px">
        Ctrl / ⌘ + Enter 执行。只允许 SELECT 与 WITH；分号后不能再有语句。
      </div>
    </div>

    <div class="card" id="sql-result"></div>`;

  const editor = SqlEditor(view.querySelector('#sql-editor'), {
    value: sessionStorage.getItem('godw.sqlText') || 'select 1 as ok',
    height: 190,
  });

  const run = () => runSQL(view, editor);
  view.querySelector('[data-act="sql-run"]').addEventListener('click', run);
  // 编辑器没有暴露 textarea，这里直接取一次；快捷键是这个页面最高频的操作。
  const area = view.querySelector('#sql-editor textarea');
  if (area) {
    area.addEventListener('keydown', e => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); run(); }
    });
    area.focus();
  }

  renderSqlResult(view.querySelector('#sql-result'), null);
}

async function runSQL(view, editor) {
  const ds = view.querySelector('#sql-ds').value;
  const limit = Number(view.querySelector('#sql-limit').value) || 500;
  const sql = editor.value().trim();
  if (!ds) return toast('先选一个数据源', 'err');
  if (!sql) return toast('SQL 不能为空', 'err');

  // 记住上次的数据源与语句：刷新页面后不该回到 select 1。
  sessionStorage.setItem('godw.sqlDs', ds);
  sessionStorage.setItem('godw.sqlText', sql);

  const host = view.querySelector('#sql-result');
  host.innerHTML = `<div class="card-head"><div class="card-title">结果</div><span class="hint">执行中…</span></div>`;
  try {
    const res = await api('/sql/query', { method: 'POST', body: { datasource: ds, sql, limit } });
    renderSqlResult(host, res);
  } catch (e) {
    host.innerHTML = `<div class="card-head"><div class="card-title">结果</div></div>
      <div class="err-box">${esc(e.message)}</div>`;
  }
}

function renderSqlResult(host, res) {
  if (!res) {
    host.innerHTML = '<div class="empty"><b>还没有结果</b>写一条 SELECT，按 Ctrl + Enter 执行</div>';
    return;
  }
  const truncated = res.truncated ? ` · 已截断到 ${res.limit} 行` : '';
  const head = `<div class="card-head">
    <div class="card-title">结果</div>
    <span class="hint mono">${res.rowCount} 行 · ${res.elapsedMs}ms${truncated}</span>
  </div>`;
  const notes = `<div class="hint sql-notes" style="margin-bottom:10px">${(res.notes || []).map(esc).join('<br>')}</div>`;

  if (!res.rowCount) {
    host.innerHTML = head + notes + '<div class="empty">查询成功，没有返回行</div>';
    return;
  }
  const th = (res.columns || []).map(c => {
    const m = res.masks && res.masks[c];
    return `<th class="mono">${esc(c)}${m ? ` <span class="pill warn" title="脱敏方式：${esc(m)}">${esc(m)}</span>` : ''}</th>`;
  }).join('');
  const tr = res.rows.map(r => `<tr>${r.map(v => `<td class="mono">${sqlCell(v)}</td>`).join('')}</tr>`).join('');

  host.innerHTML = head + notes +
    `<div class="table-wrap sql-result" style="max-height:560px;overflow:auto">
      <table><thead><tr>${th}</tr></thead><tbody>${tr}</tbody></table>
    </div>`;
}

function sqlCell(v) {
  if (v === null || v === undefined) return '<span class="dim">NULL</span>';
  let s = typeof v === 'object' ? JSON.stringify(v) : String(v);
  if (s === '') return '<span class="dim">（空串）</span>';
  // 长文本（JSON、报错堆栈）截断但保留 title，表格才不会被一行撑爆。
  if (s.length > 300) return `<span title="${esc(s.slice(0, 300))}">${esc(s.slice(0, 300))}…</span>`;
  return `<span title="${esc(s)}">${esc(s)}</span>`;
}
