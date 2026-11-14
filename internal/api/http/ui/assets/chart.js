/* GoDW 控制台 · 交互式折线图
 *
 * 三件事参照 Metabase / Superset 的底线体验，原实现一条都没有：
 *   1) hover 十字参考线 + 一次显示该 x 上全部系列的值（不是只在单点上给 <title>）
 *   2) 图例可点击，隐藏/恢复某个系列（多系列比较时不隐藏就没法看清）
 *   3) 拖拽刷选 x 区间缩放，双击复位
 *
 * 尺寸说明：viewBox 直接等于像素宽高（W = 容器实际宽度），这样 font-size:11px 就是真 11px。
 * 早先 W 固定 960 + 容器缩放，导致 700px 宽时字号被压到 7.7px —— 放大看都糊。
 */
'use strict';

// 系列色兜底：正常走 CSS 变量 --chart-1..8，这组只在变量取不到时使用。
// 亮色主题会整套覆盖 --chart-*，所以图表不必自己判断主题。
const CHART_COLORS = ['#4f8cff', '#3fb950', '#d29922', '#f85149', '#a371f7', '#39c5cf', '#db6d28', '#8b949e'];
const CHART_REGISTRY = new Set();

/** 图表统一入口。points: [{ts, value, dim}]，unit 只用于展示。 */
function drawLineChart(svg, points, unit, opts) {
  if (!svg) return;
  ensureChartShell(svg);
  const st = svg.__chart;
  st.points = points || [];
  st.unit = unit || '';
  if (opts) st.opts = Object.assign({}, st.opts, opts);
  renderChart(svg);
  CHART_REGISTRY.add(svg);
}

function ensureChartShell(svg) {
  if (svg.__chart) return;
  const h = Number(svg.getAttribute('height')) || 300;
  svg.__chart = {
    height: Math.max(160, Math.min(480, h)),
    points: [],
    unit: '',
    hidden: new Set(),
    range: null,   // [起始索引, 结束索引]，null 表示全量
    drag: null,
    opts: {},
    ro: null,
  };

  // 包一层定位容器：tooltip 与「重置缩放」按钮都用绝对定位挂在这里
  if (!svg.parentElement.classList.contains('chart-wrap')) {
    const wrap = document.createElement('div');
    wrap.className = 'chart-wrap';
    svg.parentNode.insertBefore(wrap, svg);
    wrap.appendChild(svg);
    const tip = document.createElement('div');
    tip.className = 'chart-tip';
    tip.style.display = 'none';
    wrap.appendChild(tip);
    const reset = document.createElement('button');
    reset.className = 'sm ghost chart-reset';
    reset.textContent = '重置缩放';
    reset.style.display = 'none';
    reset.addEventListener('click', () => {
      svg.__chart.range = null;
      renderChart(svg);
    });
    wrap.appendChild(reset);
  }
  svg.style.display = 'block';
  svg.style.width = '100%';

  bindChartEvents(svg);

  // 容器宽度变化就重画：响应式布局下不重画会出现线被拉伸、字变形
  if (typeof ResizeObserver !== 'undefined') {
    let t = null;
    const ro = new ResizeObserver(() => {
      clearTimeout(t);
      t = setTimeout(() => { if (svg.isConnected) renderChart(svg); }, 120);
    });
    ro.observe(svg.parentElement);
    svg.__chart.ro = ro;
  }
  CHART_REGISTRY.add(svg);
}

// 主题切换后重画一遍：颜色取自 CSS 变量，不重画就会停在旧主题的配色上
document.addEventListener('godw:theme', () => {
  CHART_REGISTRY.forEach(svg => { if (svg.isConnected) renderChart(svg); else CHART_REGISTRY.delete(svg); });
});

function chartPalette() {
  const cs = getComputedStyle(document.documentElement);
  const pick = (name, fallback) => (cs.getPropertyValue(name) || '').trim() || fallback;
  return {
    grid: pick('--line-soft', '#1c232c'),
    axis: pick('--text-mute', '#6b7887'),
    label: pick('--text-dim', '#9aa7b4'),
    surface: pick('--bg-elev2', '#1b222b'),
    // 卡片底色：环形图挖洞、面积图底色都要求与所在卡片一致，不能用页面底色。
    card: pick('--bg-elev', '#151a21'),
    line: pick('--line', '#232b36'),
    accent: pick('--accent', '#4f8cff'),
    text: pick('--text', '#e6edf3'),
    // 系列色跟着 --chart-1..8 走；BI 页与看板共用同一组，避免同一种「第二个系列」
    // 在两页是两个颜色。
    series: CHART_COLORS.map((fb, i) => pick('--chart-' + (i + 1), fb)),
  };
}

function chartFmt(v) {
  if (v == null || isNaN(v)) return '—';
  const abs = Math.abs(v);
  if (abs >= 100000000) return (v / 100000000).toFixed(2).replace(/\.?0+$/, '') + '亿';
  if (abs >= 10000) return (v / 10000).toFixed(1).replace(/\.0$/, '') + '万';
  if (abs >= 100) return Math.round(v).toLocaleString('zh-CN');
  if (abs >= 10) return String(Math.round(v * 10) / 10);
  return String(Math.round(v * 100) / 100);
}

function niceStep(range, ticks) {
  if (range <= 0) return 1;
  const raw = range / Math.max(1, ticks);
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const norm = raw / mag;
  const step = norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 5 ? 5 : 10;
  return step * mag;
}

function renderChart(svg) {
  const st = svg.__chart;
  if (!st) return;
  const ns = 'http://www.w3.org/2000/svg';
  const C = chartPalette();
  const wrap = svg.parentElement;
  const tip = wrap.querySelector('.chart-tip');
  const resetBtn = wrap.querySelector('.chart-reset');

  const W = Math.max(280, Math.round(wrap.clientWidth || 640));
  const H = st.height;
  const padL = 56, padR = 20, padT = 16, padB = 52; // 底部留给 x 轴标签 + 图例

  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('height', H);
  svg.innerHTML = '';
  if (tip) tip.style.display = 'none';

  const points = st.points || [];
  if (!points.length) {
    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', W / 2); t.setAttribute('y', H / 2);
    t.setAttribute('text-anchor', 'middle');
    t.setAttribute('style', `fill:${C.axis};font-size:12px`);
    t.textContent = '暂无数据';
    svg.appendChild(t);
    if (resetBtn) resetBtn.style.display = 'none';
    return;
  }

  // 按维度拆系列，保持首次出现的顺序
  const dims = {};
  const dimOrder = [];
  points.forEach(p => {
    const k = p.dim || '';
    if (!dims[k]) { dims[k] = []; dimOrder.push(k); }
    dims[k].push(p);
  });
  const allKeys = dimOrder;
  const visibleKeys = allKeys.filter(k => !st.hidden.has(k));

  const tsList = Array.from(new Set(points.map(p => p.ts))).sort();
  const r0 = st.range ? Math.max(0, st.range[0]) : 0;
  const r1 = st.range ? Math.min(tsList.length - 1, st.range[1]) : tsList.length - 1;
  const shownTs = tsList.slice(r0, r1 + 1);
  const xs = {};
  shownTs.forEach((t, i) => { xs[t] = i; });

  // y 轴只按「看得见的系列 + 当前区间」取范围，隐藏或缩放后才不会留大片空白
  let maxV = -Infinity, minV = Infinity;
  visibleKeys.forEach(k => dims[k].forEach(p => {
    if (xs[p.ts] === undefined) return;
    maxV = Math.max(maxV, p.value);
    minV = Math.min(minV, p.value);
  }));
  if (!isFinite(maxV) || !isFinite(minV)) { maxV = 1; minV = 0; }
  if (maxV === minV) { maxV = minV + (minV === 0 ? 1 : Math.abs(minV) * 0.1); }

  // 域的选择很关键：
  //  - 波动相对量级很小（KPI 型，比如买家数 3044~3101）→ 不从 0 起，否则线是一条平的，什么都看不出；
  //  - 否则从 0 起，避免「截断纵轴放大波动」这种误导。
  // 早先直接用原始 min/max 算步长、再把 min 归零，导致 57 的波动配 2 的步长 → 1500 条刻度糊成一片。
  const spread = maxV - minV;
  const zeroBase = !(minV > 0 && spread / Math.max(Math.abs(maxV), 1e-9) < 0.5);
  let lo = zeroBase ? 0 : minV - spread * 0.15;
  let hi = zeroBase ? maxV : maxV + spread * 0.15;
  let step = niceStep(hi - lo, 5) || 1;
  lo = Math.floor(lo / step) * step;
  hi = Math.ceil(hi / step) * step;
  if (zeroBase && lo < 0) lo = 0;
  let ticks = Math.round((hi - lo) / step);
  // 兜底：步长过细时刻度会堆成一堵墙，逐步放大直到不超过 6 条
  let guard = 0;
  while (ticks > 6 && guard++ < 16) {
    step *= 2;
    lo = zeroBase ? 0 : Math.floor((minV - spread * 0.15) / step) * step;
    hi = Math.ceil((maxV + spread * 0.15) / step) * step;
    ticks = Math.round((hi - lo) / step);
  }
  if (ticks < 1) ticks = 1;
  maxV = hi;
  minV = lo;

  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const xAt = i => padL + (shownTs.length <= 1 ? plotW / 2 : plotW * i / (shownTs.length - 1));
  const yAt = v => padT + plotH * (1 - (v - minV) / (maxV - minV));

  // ---- 网格与 y 刻度 ----
  for (let i = 0; i <= ticks; i++) {
    const v = minV + step * i;
    const yy = yAt(v);
    const line = document.createElementNS(ns, 'line');
    line.setAttribute('x1', padL); line.setAttribute('x2', W - padR);
    line.setAttribute('y1', yy); line.setAttribute('y2', yy);
    line.setAttribute('stroke', i === 0 ? C.line : C.grid);
    line.setAttribute('stroke-width', '1');
    svg.appendChild(line);

    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', padL - 10); t.setAttribute('y', yy + 4);
    t.setAttribute('text-anchor', 'end');
    t.setAttribute('style', `fill:${C.axis};font-size:11px`);
    t.textContent = chartFmt(v);
    svg.appendChild(t);
  }

  // ---- x 轴标签：按宽度决定密度，别硬写「首/中/尾」 ----
  const maxLabels = Math.max(2, Math.min(8, Math.floor(plotW / 84)));
  const stride = Math.max(1, Math.ceil(shownTs.length / maxLabels));
  shownTs.forEach((ts, i) => {
    if (i % stride !== 0 && i !== shownTs.length - 1) return;
    const t = document.createElementNS(ns, 'text');
    const anchor = i === 0 ? 'start' : (i === shownTs.length - 1 ? 'end' : 'middle');
    t.setAttribute('x', xAt(i)); t.setAttribute('y', H - 30);
    t.setAttribute('text-anchor', anchor);
    t.setAttribute('style', `fill:${C.axis};font-size:11px`);
    t.textContent = shortDate(ts);
    svg.appendChild(t);
  });

  // ---- 系列 ----
  const seriesMeta = [];
  visibleKeys.forEach(k => {
    const idx = allKeys.indexOf(k);
    const color = C.series[idx % C.series.length];
    const arr = dims[k].filter(p => xs[p.ts] !== undefined)
      .sort((a, b) => a.ts < b.ts ? -1 : 1);
    if (!arr.length) return;
    seriesMeta.push({ key: k, color, arr });

    let d = '';
    arr.forEach((p, i) => { d += (i === 0 ? 'M ' : ' L ') + xAt(xs[p.ts]) + ' ' + yAt(p.value); });

    // 面积只在「纵轴从 0 起」时才有意义 —— 轴已经截断到 3040~3120 还画到基线，
    // 那块填充会读成「从 0 涨上来的量」，属于视觉误导。
    if (arr.length > 2 && visibleKeys.length === 1 && zeroBase) {
      const area = document.createElementNS(ns, 'path');
      area.setAttribute('d', d + ` L ${xAt(xs[arr[arr.length - 1].ts])} ${padT + plotH} L ${xAt(xs[arr[0].ts])} ${padT + plotH} Z`);
      area.setAttribute('fill', color);
      area.setAttribute('opacity', '0.10');
      svg.appendChild(area);
    }

    const path = document.createElementNS(ns, 'path');
    path.setAttribute('d', d);
    path.setAttribute('fill', 'none');
    path.setAttribute('stroke', color);
    path.setAttribute('stroke-width', '1.9');
    path.setAttribute('stroke-linejoin', 'round');
    path.setAttribute('stroke-linecap', 'round');
    svg.appendChild(path);

    if (arr.length === 1) {
      const dot = document.createElementNS(ns, 'circle');
      dot.setAttribute('cx', xAt(xs[arr[0].ts])); dot.setAttribute('cy', yAt(arr[0].value));
      dot.setAttribute('r', '3.5'); dot.setAttribute('fill', color);
      svg.appendChild(dot);
    }
  });

  // ---- 图例（可点击开关） ----
  let gx = padL;
  const legendY = H - 12;
  allKeys.forEach((k, idx) => {
    const color = C.series[idx % C.series.length];
    const off = st.hidden.has(k);
    const label = k || '(空值)';
    const g = document.createElementNS(ns, 'g');
    g.setAttribute('class', 'chart-legend-item');
    g.setAttribute('style', 'cursor:pointer');
    g.dataset.key = k;

    const rect = document.createElementNS(ns, 'rect');
    rect.setAttribute('x', gx); rect.setAttribute('y', legendY - 8);
    rect.setAttribute('width', 8); rect.setAttribute('height', 8);
    rect.setAttribute('rx', 2);
    rect.setAttribute('fill', off ? 'transparent' : color);
    rect.setAttribute('stroke', color);
    rect.setAttribute('stroke-width', '1.5');
    g.appendChild(rect);

    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', gx + 13); t.setAttribute('y', legendY);
    t.setAttribute('style', `fill:${off ? C.axis : C.label};font-size:11px${off ? ';text-decoration:line-through' : ''}`);
    t.textContent = label;
    g.appendChild(t);

    const hit = document.createElementNS(ns, 'rect');
    const w = 13 + label.length * 11 + 12;
    hit.setAttribute('x', gx - 4); hit.setAttribute('y', legendY - 14);
    hit.setAttribute('width', w); hit.setAttribute('height', 20);
    hit.setAttribute('fill', 'transparent');
    g.appendChild(hit);

    // 全部隐藏时点回来要能恢复，不要留一个空图
    g.addEventListener('click', () => {
      st.hidden.has(k) ? st.hidden.delete(k) : st.hidden.add(k);
      renderChart(svg);
    });
    svg.appendChild(g);
    gx += 13 + label.length * 11 + 16;
  });

  if (!visibleKeys.length) {
    const t = document.createElementNS(ns, 'text');
    t.setAttribute('x', padL + plotW / 2); t.setAttribute('y', padT + plotH / 2);
    t.setAttribute('text-anchor', 'middle');
    t.setAttribute('style', `fill:${C.axis};font-size:12px`);
    t.textContent = '所有系列都被隐藏了，点图例恢复';
    svg.appendChild(t);
  }

  // ---- 交互层 ----
  const cross = document.createElementNS(ns, 'g');
  cross.setAttribute('class', 'chart-cross');
  cross.style.display = 'none';
  const crossLine = document.createElementNS(ns, 'line');
  crossLine.setAttribute('stroke', C.axis);
  crossLine.setAttribute('stroke-width', '1');
  crossLine.setAttribute('stroke-dasharray', '3 3');
  crossLine.setAttribute('y1', padT); crossLine.setAttribute('y2', padT + plotH);
  cross.appendChild(crossLine);
  svg.appendChild(cross);

  const dots = document.createElementNS(ns, 'g');
  dots.setAttribute('class', 'chart-dots');
  dots.style.display = 'none';
  svg.appendChild(dots);

  const brush = document.createElementNS(ns, 'rect');
  brush.setAttribute('class', 'chart-brush');
  brush.setAttribute('fill', C.accent);
  brush.setAttribute('opacity', '0.16');
  brush.setAttribute('y', padT);
  brush.setAttribute('height', plotH);
  brush.style.display = 'none';
  svg.appendChild(brush);

  const hitArea = document.createElementNS(ns, 'rect');
  hitArea.setAttribute('x', padL); hitArea.setAttribute('y', padT);
  hitArea.setAttribute('width', Math.max(1, plotW)); hitArea.setAttribute('height', plotH);
  hitArea.setAttribute('fill', 'transparent');
  hitArea.style.cursor = 'crosshair';
  svg.appendChild(hitArea);

  if (resetBtn) resetBtn.style.display = st.range ? 'block' : 'none';

  // 把最近一次渲染的几何信息留给事件处理
  st.geom = { padL, padT, plotW, plotH, shownTs, seriesMeta, C, yDomain: [minV, maxV] };
}

function shortDate(ts) {
  const s = String(ts || '');
  // 2026-11-09 → 11-09；不是日期就原样返回
  const m = s.match(/^(\d{4})-(\d{2})-(\d{2})/);
  return m ? `${m[1]}-${m[2]}-${m[3]}` : s;
}

function bindChartEvents(svg) {
  const st = svg.__chart;

  const toIndex = (clientX) => {
    const g = st.geom;
    if (!g) return -1;
    const rect = svg.getBoundingClientRect();
    // viewBox 与像素同比例，直接换算即可
    const px = (clientX - rect.left) * (svg.viewBox.baseVal.width / rect.width);
    const n = g.shownTs.length;
    if (n <= 1) return 0;
    const rel = (px - g.padL) / g.plotW;
    return Math.max(0, Math.min(n - 1, Math.round(rel * (n - 1))));
  };

  const hideOverlay = () => {
    const wrap = svg.parentElement;
    const cross = svg.querySelector('.chart-cross');
    const dots = svg.querySelector('.chart-dots');
    const tip = wrap && wrap.querySelector('.chart-tip');
    if (cross) cross.style.display = 'none';
    if (dots) dots.style.display = 'none';
    if (tip) tip.style.display = 'none';
  };

  svg.addEventListener('mousemove', (ev) => {
    const g = st.geom;
    if (!g || !g.shownTs.length) return;
    const wrap = svg.parentElement;
    const tip = wrap.querySelector('.chart-tip');
    const rect = svg.getBoundingClientRect();
    const scale = svg.viewBox.baseVal.width / rect.width || 1;
    const px = (ev.clientX - rect.left) * scale;

    // 刷选进行中：只更新选区，不弹 tooltip
    if (st.drag) {
      st.drag.x1 = px;
      const brush = svg.querySelector('.chart-brush');
      const x0 = Math.min(st.drag.x0, px), x1 = Math.max(st.drag.x0, px);
      if (brush) {
        brush.style.display = 'block';
        brush.setAttribute('x', x0);
        brush.setAttribute('width', Math.max(1, x1 - x0));
      }
      return;
    }

    const idx = toIndex(ev.clientX);
    if (idx < 0) return;
    const ts = g.shownTs[idx];
    const cx = g.padL + (g.shownTs.length <= 1 ? g.plotW / 2 : g.plotW * idx / (g.shownTs.length - 1));

    const cross = svg.querySelector('.chart-cross');
    const crossLine = cross && cross.firstElementChild;
    if (crossLine) { crossLine.setAttribute('x1', cx); crossLine.setAttribute('x2', cx); }
    if (cross) cross.style.display = 'block';

    // 每个可见系列在这一点上的值：画高亮点 + 组装 tooltip 行
    const dots = svg.querySelector('.chart-dots');
    dots.innerHTML = '';
    const rows = [];
    g.seriesMeta.forEach(s => {
      const p = s.arr.find(x => x.ts === ts);
      if (!p) return;
      const geo = yFor(st, p.value);
      const dot = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
      dot.setAttribute('cx', cx); dot.setAttribute('cy', geo);
      dot.setAttribute('r', '3.5');
      dot.setAttribute('fill', s.color);
      dot.setAttribute('stroke', g.C.surface);
      dot.setAttribute('stroke-width', '1.5');
      dots.appendChild(dot);
      rows.push(`<div class="tip-row"><span class="tip-dot" style="background:${s.color}"></span>
        <span class="tip-name">${esc(s.key || '(空值)')}</span>
        <b class="tip-val">${chartFmt(p.value)}${esc(st.unit || '')}</b></div>`);
    });
    dots.style.display = 'block';

    if (tip && rows.length) {
      tip.innerHTML = `<div class="tip-ts">${esc(ts)}</div>${rows.join('')}`;
      tip.style.display = 'block';
      // 靠近右边缘时翻到左侧，避免被容器裁掉
      const tipW = tip.offsetWidth || 160;
      const localX = px / scale;
      const left = localX + 14 + tipW > rect.width ? localX - tipW - 14 : localX + 14;
      tip.style.left = Math.max(4, left) + 'px';
      tip.style.top = Math.max(4, g.padT + 8) + 'px';
    }
  });

  svg.addEventListener('mouseleave', hideOverlay);

  // 刷选缩放
  svg.addEventListener('mousedown', (ev) => {
    const g = st.geom;
    if (!g || !g.shownTs.length || ev.button !== 0) return;
    const rect = svg.getBoundingClientRect();
    const scale = svg.viewBox.baseVal.width / rect.width || 1;
    const px = (ev.clientX - rect.left) * scale;
    if (px < g.padL || px > g.padL + g.plotW) return;
    st.drag = { x0: px, x1: px };
    ev.preventDefault();
  });

  window.addEventListener('mouseup', () => {
    if (!st.drag) return;
    const g = st.geom;
    const brush = svg.querySelector('.chart-brush');
    if (brush) brush.style.display = 'none';
    const { x0, x1 } = st.drag;
    st.drag = null;
    if (!g || Math.abs(x1 - x0) < 10) return; // 当点击处理，不缩放
    const i0 = toIndexOf(g, Math.min(x0, x1));
    const i1 = toIndexOf(g, Math.max(x0, x1));
    if (i1 - i0 < 1) return;
    // 区间要换算成全局 ts 索引，否则二次刷选会越缩越错
    const base = st.range ? st.range[0] : 0;
    st.range = [base + i0, base + i1];
    renderChart(svg);
  });

  svg.addEventListener('dblclick', () => {
    if (!st.range) return;
    st.range = null;
    renderChart(svg);
  });
}

function toIndexOf(g, px) {
  const n = g.shownTs.length;
  if (n <= 1) return 0;
  const rel = (px - g.padL) / g.plotW;
  return Math.max(0, Math.min(n - 1, Math.round(rel * (n - 1))));
}

function yFor(st, value) {
  const g = st.geom;
  const { padT, plotH } = g;
  // 与 renderChart 里同一套 min/max：重新算一遍比把状态塞进 geom 更稳妥
  const ax = g.yDomain || null;
  if (!ax) return padT + plotH / 2;
  return padT + plotH * (1 - (value - ax[0]) / (ax[1] - ax[0]));
}
