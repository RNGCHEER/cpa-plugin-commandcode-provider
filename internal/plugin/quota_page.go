package plugin

// quotaPageHTML is the plugin's resource page (served under
// /v0/resource/plugins/<pluginID>/quota). It lists every commandcode
// credential as a card - from BOTH the plugin's config api-keys pool AND the
// host's auths/ records - and refreshes each card's plan / credits / windows
// on demand. The page never polls and quota numbers never drive routing.
//
// The embedded script is built with ordinary single/double-quoted strings: an
// ES-template literal inside a Go raw string would terminate the literal.
const quotaPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>CommandCode Quota</title>
<style>
:root { color-scheme: light dark; --fg:#1f2328; --muted:#57606a; --bg:#fff; --card:#f6f8fa; --line:#d0d7de; --ok:#1a7f37; --warn:#9a6700; --bad:#cf222e; }
@media (prefers-color-scheme: dark) { :root { --fg:#e6edf3; --muted:#8b949e; --bg:#0d1117; --card:#161b22; --line:#30363d; --ok:#3fb950; --warn:#d29922; --bad:#f85149; } }
* { box-sizing:border-box; }
body { margin:0; font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; color:var(--fg); background:var(--bg); padding:20px; }
h1 { font-size:18px; margin:0 0 4px; }
.sub { color:var(--muted); margin:0 0 16px; }
.grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(320px,1fr)); gap:12px; }
.card { background:var(--card); border:1px solid var(--line); border-radius:8px; padding:14px; }
.card h2 { font-size:15px; margin:0 0 2px; display:flex; justify-content:space-between; gap:8px; align-items:center; }
.email { color:var(--muted); font-size:12px; margin:0 0 10px; word-break:break-all; }
.badge { font-size:12px; border:1px solid var(--line); border-radius:999px; padding:1px 8px; white-space:nowrap; }
.window { margin:8px 0; }
.window .row { display:flex; justify-content:space-between; font-size:12px; color:var(--muted); }
.bar { height:8px; background:var(--line); border-radius:4px; overflow:hidden; margin-top:3px; }
.bar > i { display:block; height:100%; background:var(--ok); }
.bar > i.warn { background:var(--warn); } .bar > i.bad { background:var(--bad); }
button { font:inherit; border:1px solid var(--line); background:transparent; color:inherit; border-radius:6px; padding:4px 10px; cursor:pointer; }
button:hover { border-color:var(--muted); }
button:disabled { opacity:.6; cursor:default; }
.err { color:var(--bad); font-size:12px; white-space:pre-wrap; }
.muted { color:var(--muted); font-size:12px; }
</style>
</head>
<body>
<h1>CommandCode Quota</h1>
<p class="sub">按账号查看套餐、剩余额度与 5 小时 / 每周窗口（点击刷新，不参与路由决策）。</p>
<div class="grid" id="grid"><p class="muted">加载中…</p></div>
<script>
var grid = document.getElementById('grid');
function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
function fmt(n) { return (typeof n === 'number' && isFinite(n)) ? (Math.round(n * 100) / 100).toLocaleString() : '-'; }
function when(s) { return s ? new Date(s).toLocaleString() : ''; }
function windowHtml(title, w) {
  if (!w) return '';
  var cls = w.status === 'exceeded' ? 'bad' : (w.percent >= 80 ? 'warn' : '');
  var status = ({ ok: '正常', exceeded: '已用尽', unlimited: '不限' })[w.status] || w.status;
  var resets = w.resets_at ? ' · 重置于 ' + esc(when(w.resets_at)) : '';
  return '<div class="window">'
    + '<div class="row"><span>' + esc(title) + '</span><span>' + fmt(w.used) + ' / ' + fmt(w.cap) + ' · ' + esc(status) + resets + '</span></div>'
    + '<div class="bar"><i class="' + cls + '" style="width:' + Math.min(100, w.percent || 0) + '%"></i></div>'
    + '</div>';
}
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
function post(body) {
  return fetch('/v0/management/plugins/commandcode-provider/quota-usage', { method: 'POST', headers: authHeader(), body: JSON.stringify(body) }).then(function (res) { return res.json(); });
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
    + '<h2><span>' + esc(card.label || card.name || id) + '</span><span>' + plan + '</span></h2>'
    + '<p class="email">' + esc(card.email || card.name || '') + '</p>'
    + err + credits + windows
    + '<button data-refresh="' + esc(id) + '">刷新</button>'
    + '</div>';
}
function loadList() {
  post({})
    .then(function (data) {
      if (!data.cards || !data.cards.length) {
        grid.innerHTML = '<p class="muted">没有找到 commandcode 凭证。请先在 config.yaml 的 plugins.configs.commandcode-provider.api-keys 或 auths/ 目录里配置密钥。</p>';
        return;
      }
      grid.innerHTML = data.cards.map(renderCard).join('');
    })
    .catch(function (e) { grid.innerHTML = '<p class="err">' + esc(e) + '</p>'; });
}
function refresh(id) {
  var btn = document.querySelector('[data-refresh="' + id + '"]');
  if (btn) { btn.disabled = true; btn.textContent = '刷新中…'; }
  post({ key_id: id, auth_index: id })
    .then(function (data) {
      var card = document.querySelector('.card[data-idx="' + id + '"]');
      if (card) card.outerHTML = renderCard(data);
    });
}
grid.addEventListener('click', function (e) { var idx = e.target && e.target.dataset && e.target.dataset.refresh; if (idx) refresh(idx); });
loadList();
</script>
</body>
</html>`
