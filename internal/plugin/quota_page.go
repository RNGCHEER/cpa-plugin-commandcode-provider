package plugin

// quotaPageHTML is the plugin's resource page (served under
// /v0/resource/plugins/<pluginID>/quota). It does two jobs:
//
//  1. Import a CommandCode key: the page posts it to inspect-key, which reads
//     the subscription tier from the account API, then writes the key into
//     plugins.configs.<pluginID>.api-keys through the host Management API
//     together with the chosen plan-filter and (optionally) a visible-models
//     allowlist.
//  2. Credit / window overview: every credential from BOTH the plugin's
//     api-keys pool and the host's auths/ records is listed as a card that
//     refreshes its plan, credits and 5h / weekly windows on demand.
//
// Quota numbers never drive routing. The embedded script is built with
// ordinary single/double-quoted strings: an ES-template literal inside a Go
// raw string would terminate the literal.
const quotaPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>CommandCode 密钥与套餐</title>
<style>
:root { color-scheme: light dark; --fg:#1f2328; --muted:#57606a; --bg:#fff; --card:#f6f8fa; --line:#d0d7de; --ok:#1a7f37; --warn:#9a6700; --bad:#cf222e; --accent:#0969da; }
@media (prefers-color-scheme: dark) { :root { --fg:#e6edf3; --muted:#8b949e; --bg:#0d1117; --card:#161b22; --line:#30363d; --ok:#3fb950; --warn:#d29922; --bad:#f85149; --accent:#4493f8; } }
* { box-sizing:border-box; }
body { margin:0; font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; color:var(--fg); background:var(--bg); padding:20px; max-width:1180px; }
h1 { font-size:18px; margin:0 0 4px; }
h2 { font-size:15px; margin:0 0 10px; }
.sub { color:var(--muted); margin:0 0 16px; }
.panel { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:16px; margin-bottom:18px; }
.row { display:flex; gap:8px; align-items:center; flex-wrap:wrap; }
input[type=text], input[type=password], select { font:inherit; color:inherit; background:var(--bg); border:1px solid var(--line); border-radius:6px; padding:6px 9px; }
input[type=password], #keyinput { flex:1 1 320px; min-width:240px; font-family:ui-monospace,Consolas,monospace; }
button { font:inherit; border:1px solid var(--line); background:transparent; color:inherit; border-radius:6px; padding:5px 11px; cursor:pointer; }
button:hover:not(:disabled) { border-color:var(--muted); }
button:disabled { opacity:.55; cursor:default; }
button.primary { background:var(--accent); border-color:var(--accent); color:#fff; font-weight:600; }
button.danger { color:var(--bad); }
.grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(320px,1fr)); gap:12px; }
.card { background:var(--card); border:1px solid var(--line); border-radius:8px; padding:14px; }
.card h3 { font-size:15px; margin:0 0 2px; display:flex; justify-content:space-between; gap:8px; align-items:center; }
.email { color:var(--muted); font-size:12px; margin:0 0 10px; word-break:break-all; }
.badge { font-size:12px; border:1px solid var(--line); border-radius:999px; padding:1px 8px; white-space:nowrap; }
.badge.t0 { border-color:var(--muted); }
.badge.t1 { border-color:var(--accent); color:var(--accent); }
.badge.t2 { border-color:var(--ok); color:var(--ok); }
.badge.t3, .badge.t4 { border-color:var(--warn); color:var(--warn); }
.window { margin:8px 0; }
.window .lbl { display:flex; justify-content:space-between; font-size:12px; color:var(--muted); }
.bar { height:8px; background:var(--line); border-radius:4px; overflow:hidden; margin-top:3px; }
.bar > i { display:block; height:100%; background:var(--ok); }
.bar > i.warn { background:var(--warn); } .bar > i.bad { background:var(--bad); }
.err { color:var(--bad); font-size:12px; white-space:pre-wrap; }
.ok { color:var(--ok); font-size:12px; white-space:pre-wrap; }
.muted { color:var(--muted); font-size:12px; }
.chips { display:flex; gap:6px; flex-wrap:wrap; margin-top:8px; }
.chip { display:inline-flex; align-items:center; gap:6px; border:1px solid var(--line); border-radius:999px; padding:2px 4px 2px 10px; font-family:ui-monospace,Consolas,monospace; font-size:12px; }
.chip button { border:0; padding:0 6px; font-size:12px; }
.tools { display:flex; gap:8px; align-items:center; flex-wrap:wrap; margin:12px 0 8px; }
#modelsearch { flex:1 1 200px; min-width:160px; }
.models { max-height:340px; overflow:auto; border:1px solid var(--line); border-radius:8px; background:var(--bg); padding:6px; }
.model { display:flex; gap:8px; align-items:center; padding:4px 6px; border-radius:5px; cursor:pointer; }
.model:hover { background:var(--card); }
.model .mid { color:var(--muted); font-size:12px; font-family:ui-monospace,Consolas,monospace; flex:1 1 auto; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.model .mname { flex:0 1 auto; }
.bar-line { display:flex; justify-content:space-between; font-size:12px; color:var(--muted); margin-top:6px; }
</style>
</head>
<body>
<h1>CommandCode 密钥与套餐</h1>
<p class="sub">录入密钥 → 自动识别套餐（也可手动指定）→ 按挡位勾选可用模型 → 写入插件配置。页面不参与路由决策。</p>

<div class="panel">
  <h2>① 录入密钥</h2>
  <div class="row">
    <input id="keyinput" type="password" autocomplete="off" spellcheck="false" placeholder="粘贴 CommandCode 密钥（user_... 或 auth.json 中的 apiKey）">
    <button id="btn-inspect">识别套餐</button>
  </div>
  <div id="inspectout" class="muted" style="margin-top:8px"></div>

  <div class="row" style="margin-top:14px">
    <label class="muted" for="plansel">套餐挡位</label>
    <select id="plansel"></select>
    <span id="plansummary" class="muted"></span>
  </div>

  <div class="tools">
    <input id="modelsearch" type="text" placeholder="搜索模型 id / 名称">
    <button id="btn-tier">按挡位勾选</button>
    <button id="btn-all">全选</button>
    <button id="btn-none">清空</button>
    <span id="modelcount" class="muted"></span>
  </div>
  <div id="modellist" class="models"><p class="muted">加载模型目录…</p></div>

  <div class="row" style="margin-top:14px">
    <button id="btn-save" class="primary">导入密钥并保存设置</button>
    <span id="saveout" class="muted"></span>
  </div>
  <p class="muted" style="margin:10px 0 0">密钥会写成宿主的认证文件（auths/commandcode-key-*.json），这是 CPA 真正用于调度上游的凭据；同时把 plan-filter / visible-models 写进 config.yaml 的 plugins.configs.commandcode-provider。删除密钥请到管理面板的「认证文件」。</p>
</div>

<div class="panel">
  <div class="row" style="justify-content:space-between">
    <h2 style="margin:0">② 已配置凭证与额度</h2>
    <button id="btn-reload">刷新列表</button>
  </div>
  <div class="grid" id="grid" style="margin-top:12px"><p class="muted">加载中…</p></div>
</div>

<script>
var PREFIX = 'commandcode-provider';
var TIER = { 0: 'Go', 1: 'GOAT', 2: 'Pro', 3: 'Provider', 4: 'Max' };
var state = { cfg: null, models: [], checked: {}, dirty: false, existingKeys: [] };

function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
function fmt(n) { return (typeof n === 'number' && isFinite(n)) ? (Math.round(n * 100) / 100).toLocaleString() : '-'; }
function when(s) { return s ? new Date(s).toLocaleString() : ''; }
function $(id) { return document.getElementById(id); }
function tierName(w) { return TIER[w] || '未标注'; }

function authHeader() {
  var h = { 'Content-Type': 'application/json' };
  try {
    var raw = localStorage.getItem('cli-proxy-auth');
    if (raw) {
      var decoded = raw;
      if (raw.indexOf('enc::v1::') === 0) {
        var b = atob(raw.slice(9));
        var seed = 'cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent;
        var out = '';
        for (var i = 0; i < b.length; i++) out += String.fromCharCode(b.charCodeAt(i) ^ seed.charCodeAt(i % seed.length));
        decoded = out;
      }
      var parsed = JSON.parse(decoded);
      if (parsed && parsed.state && parsed.state.managementKey) h['Authorization'] = 'Bearer ' + parsed.state.managementKey;
    }
  } catch (e) {}
  return h;
}

function pluginPost(name, body) {
  return fetch('/v0/management/plugins/' + PREFIX + '/' + name, { method: 'POST', headers: authHeader(), body: JSON.stringify(body || {}) })
    .then(function (res) { return res.json(); });
}
function getConfig() {
  return fetch('/v0/management/plugins/' + PREFIX + '/config', { method: 'GET', headers: authHeader() })
    .then(function (res) { if (!res.ok) throw new Error('读取插件配置失败（HTTP ' + res.status + '），请先在管理界面登录'); return res.json(); });
}
function patchConfig(patch) {
  return fetch('/v0/management/plugins/' + PREFIX + '/config', { method: 'PATCH', headers: authHeader(), body: JSON.stringify(patch) })
    .then(function (res) {
      if (!res.ok) return res.text().then(function (t) { throw new Error('保存失败（HTTP ' + res.status + '）：' + t); });
      return res.json().catch(function () { return {}; });
    });
}

function aliasWeight(alias) { return ({ '': -1, go: 0, goat: 1, pro: 2, provider: 3, max: 4 })[alias]; }
function weightAlias(w) { return ({ 0: 'go', 1: 'goat', 2: 'pro', 3: 'provider', 4: 'max' })[w] || ''; }

function planOptions() {
  var sel = $('plansel');
  var opts = [['', '不限制（跟随目录）'], ['go', 'Go'], ['goat', 'GOAT'], ['pro', 'Pro'], ['provider', 'Provider'], ['max', 'Max']];
  sel.innerHTML = opts.map(function (o) { return '<option value="' + o[0] + '">' + o[1] + '</option>'; }).join('');
}

function updateCount() {
  var n = Object.keys(state.checked).filter(function (k) { return state.checked[k]; }).length;
  $('modelcount').textContent = '已勾选 ' + n + ' / ' + state.models.filter(function (m) { return !m.hidden; }).length + ' 个模型';
}

function renderModels() {
  var q = ($('modelsearch').value || '').trim().toLowerCase();
  var rows = state.models.filter(function (m) {
    if (m.hidden) return false;
    if (!q) return true;
    return ((m.id || '') + ' ' + (m.label || '') + ' ' + (m.group || '')).toLowerCase().indexOf(q) >= 0;
  });
  rows.sort(function (a, b) {
    var wa = a.plan_weight < 0 ? 9 : a.plan_weight, wb = b.plan_weight < 0 ? 9 : b.plan_weight;
    if (wa !== wb) return wa - wb;
    return a.id < b.id ? -1 : 1;
  });
  if (!rows.length) { $('modellist').innerHTML = '<p class="muted">没有匹配的模型</p>'; updateCount(); return; }
  $('modellist').innerHTML = rows.map(function (m) {
    var w = m.plan_weight;
    return '<label class="model"><input type="checkbox" data-model="' + esc(m.id) + '"' + (state.checked[m.id] ? ' checked' : '') + '>'
      + '<span class="mname">' + esc(m.label || m.id) + '</span>'
      + '<span class="mid">' + esc(m.id) + '</span>'
      + '<span class="badge t' + (w < 0 ? 'x' : w) + '">' + esc(w < 0 ? '未标注' : tierName(w)) + '</span></label>';
  }).join('');
  updateCount();
}

function checkByTier(alias) {
  var w = aliasWeight(alias);
  state.checked = {};
  state.models.forEach(function (m) {
    if (m.hidden) return;
    if (w < 0) { state.checked[m.id] = true; return; }
    if (m.plan_weight < 0) return;
    if (m.plan_weight <= w) state.checked[m.id] = true;
  });
  state.dirty = false;
  renderModels();
}

function applyConfig(cfg) {
  state.cfg = cfg || {};
  var keys = state.cfg['api-keys'];
  if (!Array.isArray(keys)) keys = keys ? [String(keys)] : [];
  state.existingKeys = keys.filter(function (k) { return typeof k === 'string' && k.trim(); });
  var pf = state.cfg['plan-filter'] || '';
  if (['', 'go', 'goat', 'pro', 'provider', 'max'].indexOf(pf) < 0) {
    pf = weightAlias(({ 'individual-go': 0, 'individual-goat': 1, 'individual-pro': 2, 'individual-provider': 3, 'individual-max': 4 })[pf]);
    if (pf === undefined) pf = '';
  }
  $('plansel').value = pf;
  var visible = state.cfg['visible-models'];
  if (Array.isArray(visible) && visible.length) {
    state.checked = {};
    visible.forEach(function (id) { state.checked[String(id)] = true; });
    state.dirty = true;
  } else {
    checkByTier(pf);
  }
  updatePlanSummary();
}

function updatePlanSummary() {
  var alias = $('plansel').value;
  var w = aliasWeight(alias);
  var allowed = state.models.filter(function (m) { return !m.hidden && (w < 0 || (m.plan_weight >= 0 && m.plan_weight <= w)); }).length;
  $('plansummary').textContent = alias
    ? '挡位 ' + TIER[w] + '：目录中有 ' + allowed + ' 个模型可用'
    : '不按挡位过滤：发布目录中全部模型（隐藏模型除外）';
}

function loadConfig() {
  return getConfig().then(applyConfig).catch(function (e) {
    $('saveout').innerHTML = '<span class="err">' + esc(e.message) + '</span>';
    applyConfig(state.cfg || {});
  });
}

function loadCatalog() {
  return pluginPost('catalog', {}).then(function (data) {
    state.models = Array.isArray(data.models) ? data.models : [];
    $('modellist').innerHTML = '<p class="muted">模型目录为空（插件可能还没拉取到目录，稍后点“刷新列表”）。</p>';
    if (state.models.length) {
      applyConfig(state.cfg || {});
    }
  });
}

function inspect() {
  var key = $('keyinput').value.trim();
  var out = $('inspectout');
  if (!key) { out.innerHTML = '<span class="err">请先粘贴密钥。</span>'; return; }
  out.textContent = '识别中…';
  $('btn-inspect').disabled = true;
  pluginPost('inspect-key', { api_key: key }).then(function (d) {
    $('btn-inspect').disabled = false;
    if (!d.ok) { out.innerHTML = '<span class="err">识别失败：' + esc(d.error || '未知错误') + '</span>'; return; }
    var alias = weightAlias(d.plan_weight);
    $('plansel').value = alias;
    checkByTier(alias);
    updatePlanSummary();
    var bits = [];
    bits.push('套餐：<b>' + esc(d.plan_label || d.plan_id || '未知') + '</b>' + (d.plan_id && d.plan_id !== d.plan_label ? '（' + esc(d.plan_id) + '）' : ''));
    if (d.email) bits.push('账号：' + esc(d.email));
    bits.push('剩余额度：' + fmt(d.credits_left) + (d.plan_credits ? ' / ' + fmt(d.plan_credits) + '（月）' : ''));
    if (d.allowed_models) bits.push('该挡位可用模型：' + d.allowed_models.length + ' 个');
    out.innerHTML = '<span class="ok">' + bits.join(' · ') + '</span>';
  }).catch(function (e) {
    $('btn-inspect').disabled = false;
    out.innerHTML = '<span class="err">识别失败：' + esc(e.message || e) + '</span>';
  });
}

function save() {
  var out = $('saveout');
  var newKey = $('keyinput').value.trim();
  var alias = $('plansel').value;
  var checked = Object.keys(state.checked).filter(function (k) { return state.checked[k]; });
  out.textContent = '保存中…';
  $('btn-save').disabled = true;

  var imported = newKey ? pluginPost('import-key', { api_key: newKey }) : Promise.resolve(null);
  imported.then(function (data) {
    if (data && !data.ok) throw new Error('导入密钥失败：' + (data.error || '未知错误'));
    var patch = { 'plan-filter': alias, 'visible-models': state.dirty ? checked : [] };
    if (newKey) {
      var rest = (state.existingKeys || []).filter(function (k) { return k !== newKey; });
      if (rest.length !== (state.existingKeys || []).length) patch['api-keys'] = rest;
    }
    return patchConfig(patch).then(function () { return data; });
  }).then(function (data) {
    var parts = [];
    if (data && data.plan_label) parts.push('套餐 ' + data.plan_label);
    if (data && data.label) parts.push('账号 ' + data.label);
    parts.push('挡位 ' + (alias ? TIER[aliasWeight(alias)] : '不限制'));
    if (state.dirty) parts.push('白名单 ' + checked.length + ' 个模型');
    out.innerHTML = '<span class="ok">已保存：' + esc(parts.join(' · ')) + (newKey ? '；密钥已写入认证文件' : '') + '</span>';
    $('keyinput').value = '';
    return loadConfig().then(loadCatalog);
  }).then(function () {
    setTimeout(loadList, 1200);
    setTimeout(loadList, 4000);
  }).catch(function (e) {
    out.innerHTML = '<span class="err">' + esc(e.message || e) + '</span>';
  }).then(function () { $('btn-save').disabled = false; });
}

function windowHtml(title, w) {
  if (!w) return '';
  var cls = w.status === 'exceeded' ? 'bad' : (w.percent >= 80 ? 'warn' : '');
  var status = ({ ok: '正常', exceeded: '已用尽', unlimited: '不限' })[w.status] || w.status;
  var resets = w.resets_at ? ' · 重置于 ' + esc(when(w.resets_at)) : '';
  return '<div class="window"><div class="lbl"><span>' + esc(title) + '</span><span>' + fmt(w.used) + ' / ' + fmt(w.cap) + ' · ' + esc(status) + resets + '</span></div>'
    + '<div class="bar"><i class="' + cls + '" style="width:' + Math.min(100, w.percent || 0) + '%"></i></div></div>';
}

function cardId(card) { return card.key_id || card.auth_index || ''; }

function renderCard(card) {
  var usage = card.usage;
  var id = cardId(card);
  var windows = usage ? (windowHtml('月度额度', usage.month) + windowHtml('5 小时窗口', usage.five_hour) + windowHtml('每周窗口', usage.weekly)) : '';
  var plan = (usage && usage.plan) ? '<span class="badge">' + esc(usage.plan) + (usage.plan_credits ? ' · $' + fmt(usage.plan_credits) + '/月' : '') + '</span>' : '';
  var credits = usage ? '<p class="muted">剩余 ' + fmt(usage.credits_left) + ' credits（购买 ' + fmt(usage.purchased_credits) + '） · 刷新于 ' + esc(when(usage.refreshed_at)) + '</p>' : '';
  var err = card.error ? '<p class="err">' + esc(card.error) + '</p>' : '';
  return '<div class="card" data-idx="' + esc(id) + '">'
    + '<h3><span>' + esc(card.label || card.name || id.slice(0, 12)) + '</span><span>' + plan + '</span></h3>'
    + '<p class="email">' + esc(card.email || card.name || '') + '</p>'
    + err + credits + windows
    + '<button data-refresh="' + esc(id) + '">刷新额度</button></div>';
}

function loadList() {
  pluginPost('quota-usage', {}).then(function (data) {
    if (!data.cards || !data.cards.length) {
      $('grid').innerHTML = '<p class="muted">还没有 commandcode 凭证。在上方录入密钥并保存后即可查看套餐与额度。</p>';
      return;
    }
    $('grid').innerHTML = data.cards.map(renderCard).join('');
  }).catch(function (e) { $('grid').innerHTML = '<p class="err">' + esc(e.message || e) + '</p>'; });
}

function refreshCard(id) {
  var btn = document.querySelector('[data-refresh="' + id + '"]');
  if (btn) { btn.disabled = true; btn.textContent = '刷新中…'; }
  pluginPost('quota-usage', { key_id: id, auth_index: id }).then(function (data) {
    var card = document.querySelector('.card[data-idx="' + id + '"]');
    if (card) card.outerHTML = renderCard(data);
  });
}

document.addEventListener('click', function (e) {
  var t = e.target;
  if (!t || !t.dataset) return;
  if (t.dataset.refresh) { refreshCard(t.dataset.refresh); return; }
});
$('modellist').addEventListener('change', function (e) {
  var id = e.target && e.target.dataset && e.target.dataset.model;
  if (!id) return;
  state.checked[id] = e.target.checked;
  state.dirty = true;
  updateCount();
});
$('modelsearch').addEventListener('input', renderModels);
$('plansel').addEventListener('change', function () {
  checkByTier($('plansel').value);
  updatePlanSummary();
});
$('btn-inspect').addEventListener('click', inspect);
$('btn-tier').addEventListener('click', function () { checkByTier($('plansel').value); updatePlanSummary(); });
$('btn-all').addEventListener('click', function () {
  state.models.forEach(function (m) { if (!m.hidden) state.checked[m.id] = true; });
  state.dirty = true; renderModels();
});
$('btn-none').addEventListener('click', function () {
  state.checked = {}; state.dirty = true; renderModels();
});
$('btn-save').addEventListener('click', save);
$('btn-reload').addEventListener('click', function () { loadList(); loadConfig().then(loadCatalog).catch(function () {}); });

planOptions();
loadConfig().then(loadCatalog).catch(function () {});
loadList();
</script>
</body>
</html>
`
