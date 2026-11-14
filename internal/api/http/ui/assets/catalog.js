/* GoDW 控制台 · 数据文档站：一个模型一页，字段、质量、血缘、运行证据全在视野内。 */
'use strict';

Object.assign(ROUTES, {
  catalog: { title: '数据文档', sub: '字段、质量、血缘与运行一页读完', render: pageCatalog },
});

const CATALOG = { data: null, sel: '', q: '', layer: '' };

const CAT_LAYER_LABEL = {
  ods: 'ODS 接入', dim: 'DIM 维度', dwd: 'DWD 明细', dws: 'DWS 汇总', ads: 'ADS 应用',
};

async function pageCatalog(view) {
  const reload = async () => { CATALOG.data = await api('/catalog'); };
  if (!CATALOG.data) await reload();
  if (!CATALOG.sel) {
    // 默认打开「最近跑过」的模型：第一个模型往往是 ODS 接入表，
    // 它没有质量规则也没有运行记录，作为落地页会让人以为文档是空的。
    const list = CATALOG.data.entries || [];
    const withRun = list.find(d => d.lastRun) || list[0];
    CATALOG.sel = withRun ? withRun.name : '';
  }

  view.innerHTML = `
    <div class="page-head">
      <div>
        <h2 class="page-title">数据文档</h2>
        <p class="page-desc">左选模型，中间看字段与质量，右侧追上下游与运行证据。内容全部来自实时元数据，不是一份会过期的文档。</p>
      </div>
      <div class="row">
        <button class="sm ghost" data-act="cat-reload">刷新</button>
        <button class="sm ghost" data-act="cat-export" title="导出单文件 HTML，断网可读，可直接发给外部协作方">导出 HTML</button>
        <button class="sm ghost" data-act="cat-download" title="下载为 catalog.html">下载</button>
      </div>
    </div>
    <div class="doc-layout">
      <aside class="doc-nav card">
        <input id="cat-q" class="doc-search" placeholder="搜索模型 / 字段 / 注释" value="${esc(CATALOG.q)}" />
        <div id="cat-nav"></div>
      </aside>
      <section id="cat-main" class="doc-main"></section>
      <aside id="cat-side" class="doc-side"></aside>
    </div>`;

  const paintNav = () => {
    const host = document.getElementById('cat-nav');
    if (!host) return;
    const q = CATALOG.q.trim().toLowerCase();
    const entries = (CATALOG.data.entries || []).filter(d => {
      if (CATALOG.layer && d.layer !== CATALOG.layer) return false;
      if (!q) return true;
      // 搜字段名也算命中：找「哪张表有 phone」和找表名一样常见。
      return d.name.toLowerCase().includes(q) ||
        (d.table || '').toLowerCase().includes(q) ||
        (d.description || '').toLowerCase().includes(q) ||
        (d.columns || []).some(c => c.name.toLowerCase().includes(q) || (c.comment || '').toLowerCase().includes(q));
    });
    const groups = {};
    entries.forEach(d => (groups[d.layer || ''] = groups[d.layer || ''] || []).push(d));
    const layers = CATALOG.data.layers || Object.keys(groups);
    host.innerHTML = `
      <div class="doc-filter">
        <a href="#" data-layer="" class="${CATALOG.layer ? '' : 'active'}">全部 ${CATALOG.data.entries.length}</a>
        ${layers.map(l => `<a href="#" data-layer="${esc(l)}" class="${CATALOG.layer === l ? 'active' : ''}">${esc((CAT_LAYER_LABEL[l] || l).split(' ')[0])}</a>`).join('')}
      </div>
      ${layers.map(l => {
        const list = groups[l] || [];
        if (!list.length) return '';
        return `<div class="doc-group">
          <div class="doc-group-title">${esc(CAT_LAYER_LABEL[l] || l || '未分层')}<span>${list.length}</span></div>
          ${list.map(d => `<a href="#" class="doc-item ${d.name === CATALOG.sel ? 'active' : ''}" data-model="${esc(d.name)}">
            <i class="doc-dot ${d.freshness.status}"></i><span class="mono">${esc(d.name)}</span>
          </a>`).join('')}
        </div>`;
      }).join('') || '<div class="empty" style="padding:20px 0">没有匹配的模型</div>'}`;

    host.querySelectorAll('[data-model]').forEach(el => {
      el.onclick = ev => {
        ev.preventDefault();
        CATALOG.sel = el.dataset.model;
        paintNav(); paintMain(); paintSide();
      };
    });
    host.querySelectorAll('[data-layer]').forEach(el => {
      el.onclick = ev => {
        ev.preventDefault();
        CATALOG.layer = el.dataset.layer;
        paintNav();
      };
    });
  };

  const paintMain = () => {
    const host = document.getElementById('cat-main');
    if (!host) return;
    const d = (CATALOG.data.entries || []).find(x => x.name === CATALOG.sel);
    if (!d) { host.innerHTML = '<div class="card"><div class="empty">还没有模型文档。先发布一个模型。</div></div>'; return; }
    host.innerHTML = `
      <div class="card">
        <div class="doc-title">
          <h3>${esc(d.name)}</h3>
          <span class="pill">${esc(CAT_LAYER_LABEL[d.layer] || d.layer || '未分层')}</span>
          ${d.version ? `<span class="pill">v${d.version} / 共 ${d.versions || 1} 版</span>` : '<span class="pill warn">未发布</span>'}
          ${(d.tags || []).map(t => `<span class="pill mute">${esc(t)}</span>`).join('')}
          ${catFreshPill(d)}
        </div>
        ${d.description ? `<div class="doc-desc">${esc(d.description)}</div>` : ''}
        <div class="doc-meta">
          <div><b>物理表</b><span class="mono">${esc(d.table || '—')}</span></div>
          <div><b>Owner</b><span>${esc(d.owner || '未指定')}</span></div>
          <div><b>物化</b><span>${esc(d.materialization || 'table')}</span></div>
          <div><b>质量规则</b><span>${(d.quality || []).length} 条</span></div>
        </div>
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">字段（${(d.columns || []).length}）</div>
          <div class="hint">分级与脱敏来自列策略；未标注表示没有命中策略</div></div>
        ${(d.columns || []).length ? `<div class="table-wrap"><table class="doc-table">
          <thead><tr><th style="width:26%">名称</th><th style="width:16%">类型</th><th style="width:8%">可空</th><th>注释</th><th style="width:16%">分级 / 脱敏</th></tr></thead>
          <tbody>${d.columns.map(c => `<tr>
            <td class="mono">${esc(c.name)}</td>
            <td class="mono dim">${esc(c.type || '')}</td>
            <td>${c.nullable ? '是' : '否'}</td>
            <td>${esc(c.comment || '')}</td>
            <td>${c.classification ? `<span class="pill ${c.classification === 'high' ? 'err' : c.classification === 'medium' ? 'warn' : 'mute'}">${esc(c.classification)}</span>` : ''}
                ${c.mask ? `<span class="pill info">${esc(c.mask)}</span>` : ''}</td>
          </tr>`).join('')}</tbody></table></div>`
          : '<div class="empty">没有字段定义</div>'}
      </div>

      <div class="card">
        <div class="card-head"><div class="card-title">质量（${(d.quality || []).length}）</div>
          <div class="hint">规则定义在管道的质量节点里；「未运行」表示还没有任何一次执行结果</div></div>
        ${(d.quality || []).length ? `<div class="table-wrap"><table class="doc-table">
          <thead><tr><th style="width:30%">规则</th><th style="width:16%">类型</th><th style="width:14%">列</th><th style="width:10%">级别</th><th style="width:16%">最近结果</th><th>实测</th></tr></thead>
          <tbody>${d.quality.map(q => `<tr>
            <td class="mono">${esc(q.name)}</td>
            <td class="mono dim">${esc(q.kind)}</td>
            <td class="mono">${esc(q.column || '—')}</td>
            <td><span class="pill ${q.severity === 'warn' ? 'warn' : 'err'}" style="font-size:10.5px">${esc(q.severity)}</span></td>
            <td>${q.lastPassed === true ? '<span class="pill ok">通过</span>' : q.lastPassed === false ? '<span class="pill err">未通过</span>' : '<span class="hint">未运行</span>'}</td>
            <td class="mono dim"><span class="doc-obs" title="${esc(q.observed || '')}">${esc(q.observed || '')}</span></td>
          </tr>`).join('')}</tbody></table></div>`
          : '<div class="empty">还没有质量规则</div>'}
      </div>

      ${d.ddl ? `<div class="card">
        <details class="doc-details">
          <summary>当前版本 DDL</summary>
          <pre class="doc-pre mono">${esc(d.ddl)}</pre>
        </details>
      </div>` : ''}`;
  };

  const paintSide = () => {
    const host = document.getElementById('cat-side');
    if (!host) return;
    const d = (CATALOG.data.entries || []).find(x => x.name === CATALOG.sel);
    if (!d) { host.innerHTML = ''; return; }
    const names = new Set((CATALOG.data.entries || []).map(x => x.name));
    const chip = n => `<a href="#" class="doc-link ${names.has(n) ? '' : 'mute'}" data-jump="${esc(n)}">${esc(n)}</a>`;
    host.innerHTML = `
      <div class="card">
        <div class="card-title" style="margin-bottom:8px">上下游</div>
        <div class="doc-sub">上游 ${(d.upstream || []).length}</div>
        <div class="chips">${(d.upstream || []).map(chip).join('') || '<span class="hint">无</span>'}</div>
        <div class="doc-sub" style="margin-top:10px">下游 ${(d.downstream || []).length}</div>
        <div class="chips">${(d.downstream || []).map(chip).join('') || '<span class="hint">无</span>'}</div>
      </div>

      <div class="card">
        <div class="card-title" style="margin-bottom:8px">最近一次运行</div>
        ${d.lastRun ? `
          <div class="doc-kv"><b>状态</b><span class="pill ${d.lastRun.status === 'success' ? 'ok' : 'err'}">${esc(d.lastRun.status)}</span></div>
          <div class="doc-kv"><b>时间</b><span>${esc(catTime(d.lastRun.finishedAt))}</span></div>
          <div class="doc-kv"><b>业务日期</b><span class="mono">${esc(d.lastRun.bizDate)}</span></div>
          <div class="doc-kv"><b>管道</b><span class="mono">${esc(d.lastRun.pipeline || '—')}</span></div>
          <div class="doc-kv"><b>节点</b><span class="mono">${esc(d.lastRun.node || '—')}</span></div>
          <button class="sm ghost" style="margin-top:8px" data-act="cat-sql">查看当次渲染 SQL</button>`
        : '<div class="empty" style="padding:16px 0">还没有运行记录</div>'}
      </div>

      <div class="card">
        <div class="card-title" style="margin-bottom:8px">版本历史</div>
        <div id="cat-versions" class="hint">加载中…</div>
      </div>`;

    host.querySelectorAll('[data-jump]').forEach(el => {
      el.onclick = ev => {
        ev.preventDefault();
        if (!names.has(el.dataset.jump)) return;
        CATALOG.sel = el.dataset.jump;
        paintNav(); paintMain(); paintSide();
      };
    });

    api('/models/' + encodeURIComponent(d.name) + '/versions').then(list => {
      const box = document.getElementById('cat-versions');
      if (!box) return;
      if (!list || !list.length) { box.innerHTML = '没有版本'; return; }
      box.innerHTML = list.slice(0, 12).map(v => `
        <div class="doc-kv">
          <b>v${v.version}</b>
          <span>${esc(catTime(v.publishedAt))} ${v.version === d.version ? '<span class="pill ok" style="font-size:10px">当前</span>' : ''}</span>
        </div>`).join('') + (list.length > 12 ? `<div class="hint" style="margin-top:6px">还有 ${list.length - 12} 个版本</div>` : '');
    }).catch(err => {
      const box = document.getElementById('cat-versions');
      if (box) box.textContent = err.message;
    });
  };

  paintNav(); paintMain(); paintSide();

  const q = document.getElementById('cat-q');
  if (q) q.oninput = () => { CATALOG.q = q.value; paintNav(); };

  // 导出走 fetch + Blob，而不是 <a href>：下载接口要 Bearer token，
  // 直接跳转不会带 Authorization 头，生产环境会 401（dev 下匿名放行，所以更容易漏掉）。
  const doExport = async download => {
    const resp = await fetch(API + '/catalog/export?format=html' + (download ? '&download=1' : ''),
      { headers: token() ? { Authorization: 'Bearer ' + token() } : {} });
    if (!resp.ok) throw new Error('导出失败：HTTP ' + resp.status);
    const url = URL.createObjectURL(new Blob([await resp.text()], { type: 'text/html;charset=utf-8' }));
    if (download) {
      const a = document.createElement('a');
      a.href = url; a.download = 'catalog.html';
      document.body.appendChild(a); a.click(); a.remove();
      toast('已导出 catalog.html');
    } else {
      window.open(url, '_blank', 'noopener');
    }
    // 立刻 revoke 会让新标签页拿到空文档，给它一分钟。
    setTimeout(() => URL.revokeObjectURL(url), 60000);
  };
  view.querySelector('[data-act="cat-export"]').onclick = () => doExport(false).catch(e => toast(e.message, 'err'));
  view.querySelector('[data-act="cat-download"]').onclick = () => doExport(true).catch(e => toast(e.message, 'err'));

  view.querySelector('[data-act="cat-reload"]').onclick = async () => {
    await reload();
    if (!(CATALOG.data.entries || []).some(x => x.name === CATALOG.sel)) {
      CATALOG.sel = (CATALOG.data.entries[0] || {}).name || '';
    }
    paintNav(); paintMain(); paintSide();
    toast('文档已刷新');
  };

  view.addEventListener('click', async ev => {
    const btn = ev.target.closest('[data-act="cat-sql"]');
    if (!btn) return;
    const d = (CATALOG.data.entries || []).find(x => x.name === CATALOG.sel);
    if (!d || !d.lastRun) return;
    btn.disabled = true;
    try {
      const res = await api('/instances/' + encodeURIComponent(d.lastRun.instanceId) +
        '/sql?node=' + encodeURIComponent(d.lastRun.node || ''));
      const warn = res.warning ? `<div class="warn-box" style="margin-bottom:8px">${esc(res.warning)}</div>` : '';
      modal('渲染 SQL · ' + d.name, `${warn}
        ${res.error ? `<div class="err-box">${esc(res.error)}</div>` : ''}
        <pre class="doc-pre mono" style="max-height:52vh">${esc(res.sql || '—')}</pre>
        <div class="hint" style="margin-top:8px">实例 ${esc(d.lastRun.instanceId)} · 管道版本 v${res.version}${res.fromSnapshot ? '（版本快照）' : '（当前定义）'}</div>`);
    } catch (err) {
      toast(err.message, 'err');
    } finally {
      btn.disabled = false;
    }
  });
}

function catFreshPill(d) {
  const f = d.freshness || {};
  const byRule = f.basis === 'quality_rule';
  const thr = f.thresholdMin ? `阈值 ${catAge(f.thresholdMin)}` : '未设阈值';
  if (f.status === 'fresh') {
    return `<span class="pill ok" title="${byRule ? 'freshness 规则已通过 · ' + thr : '最近成功距今 ' + catAge(f.ageMinutes)}">${byRule ? '数据新鲜' : '新鲜'}</span>`;
  }
  if (f.status === 'stale') {
    return `<span class="pill err" title="${byRule ? 'freshness 规则未通过 · ' + thr : '最近成功距今 ' + catAge(f.ageMinutes) + '，' + thr}">${byRule ? '数据过期' : '已过期'}</span>`;
  }
  if (f.basis === 'no_run') return '<span class="pill mute">未运行</span>';
  const why = f.ageMinutes ? `最近成功 ${catAge(f.ageMinutes)} 前；未配 freshness 规则，无法判断数据本身` : '未配 freshness 规则';
  return `<span class="pill warn" title="${esc(why)}">新鲜度未知</span>`;
}

function catAge(min) {
  if (min == null || isNaN(min)) return '—';
  if (min < 1) return '不到 1 分钟';
  if (min < 60) return Math.round(min) + ' 分钟';
  if (min < 1440) return (min / 60).toFixed(1) + ' 小时';
  return (min / 1440).toFixed(1) + ' 天';
}

function catTime(s) {
  if (!s) return '—';
  const d = new Date(s);
  if (isNaN(d.getTime())) return s;
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
