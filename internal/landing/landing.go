// Package landing serves a public entry page at the gateway root. It is
// intentionally dependency-free: a single embedded HTML document that shows
// service status, the API surface, and how to reach the admin UI.
package landing

import (
	"html/template"
	"net/http"
)

// Config carries the small amount of runtime information the page renders.
type Config struct {
	Version    string
	AdminToken bool // whether an admin token is configured (affects copy)
}

type pageData struct {
	Version    string
	AdminToken bool
}

var tmpl = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<meta name="theme-color" content="#123526">
<title>free-router · 免费模型路由网关</title>
<link rel="icon" href="/admin/favicon.svg" type="image/svg+xml">
<style>
  :root{
    --bg:#f6f7f5; --ink:#123526; --ink-soft:#3d5a4c; --muted:#6b8276;
    --card:#ffffff; --line:#e2e8e4; --accent:#b9efcb; --accent-strong:#3aa76d;
    --danger:#c0392b; --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  }
  *{box-sizing:border-box}
  html,body{margin:0;padding:0}
  body{
    background:var(--bg); color:var(--ink);
    font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;
    -webkit-font-smoothing:antialiased;
  }
  .wrap{max-width:880px;margin:0 auto;padding:48px 24px 64px}
  header.hero{display:flex;align-items:center;gap:16px;margin-bottom:8px}
  .logo{width:44px;height:44px;flex:none}
  h1{font-size:26px;margin:0;letter-spacing:-.01em}
  .tag{color:var(--muted);margin:2px 0 0;font-size:14px}
  .statusbar{
    display:flex;flex-wrap:wrap;gap:10px;align-items:center;
    margin:22px 0 26px;padding:12px 14px;background:var(--card);
    border:1px solid var(--line);border-radius:10px;
  }
  .pill{display:inline-flex;align-items:center;gap:7px;font-size:13px;color:var(--ink-soft)}
  .dot{width:9px;height:9px;border-radius:50%;background:#c9d2cc}
  .dot.ok{background:var(--accent-strong)}
  .dot.bad{background:var(--danger)}
  .dot.pending{background:#d8c36a;animation:pulse 1.2s infinite}
  @keyframes pulse{50%{opacity:.4}}
  .grid{display:grid;grid-template-columns:1fr 1fr;gap:16px}
  @media(max-width:680px){.grid{grid-template-columns:1fr}}
  .card{
    background:var(--card);border:1px solid var(--line);border-radius:12px;
    padding:20px;
  }
  .card h2{margin:0 0 6px;font-size:16px}
  .card p{margin:6px 0;color:var(--ink-soft);font-size:14px}
  .muted{color:var(--muted);font-size:13px}
  a.btn{
    display:inline-block;margin-top:10px;padding:8px 14px;border-radius:8px;
    background:var(--ink);color:#fff;text-decoration:none;font-size:14px;
  }
  a.btn.ghost{background:transparent;color:var(--ink);border:1px solid var(--line)}
  code,.mono{font-family:var(--mono);font-size:13px;background:#eef2ee;padding:1px 6px;border-radius:5px}
  pre{
    background:#0f2a1e;color:#d6f5e0;padding:14px 16px;border-radius:10px;
    overflow:auto;font-family:var(--mono);font-size:12.5px;line-height:1.55;margin:10px 0 0;
  }
  pre .c{color:#7fb89a}
  .note{
    margin-top:22px;padding:14px 16px;border-left:3px solid var(--accent-strong);
    background:#eef8f1;border-radius:0 8px 8px 0;font-size:13.5px;color:var(--ink-soft);
  }
  .note b{color:var(--ink)}
  footer{margin-top:36px;color:var(--muted);font-size:12.5px;text-align:center}
  footer a{color:var(--ink-soft)}
</style>
</head>
<body>
<div class="wrap">

  <header class="hero">
    <svg class="logo" viewBox="0 0 64 64" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
      <rect width="64" height="64" rx="15" fill="#123526"/>
      <path d="M17 32h13m0 0c7 0 7-14 14-14h3M30 32h17m-17 0c7 0 7 14 14 14h3" fill="none" stroke="#b9efcb" stroke-width="5" stroke-linecap="round" stroke-linejoin="round"/>
      <circle cx="16" cy="32" r="5" fill="#f6f7f5"/>
      <circle cx="49" cy="18" r="5" fill="#b9efcb"/>
      <circle cx="49" cy="32" r="5" fill="#b9efcb"/>
      <circle cx="49" cy="46" r="5" fill="#b9efcb"/>
    </svg>
    <div>
      <h1>free-router</h1>
      <p class="tag">OpenAI 兼容的免费模型路由器 · 自动故障切换 · 健康检查隔离</p>
    </div>
  </header>

  <div class="statusbar">
    <span class="pill"><span class="dot pending" id="dot-health"></span><span id="health-text">正在探测 /healthz …</span></span>
    <span class="pill">版本 <code>{{.Version}}</code></span>
    <span class="pill" id="pill-providers" hidden><span class="dot ok"></span><span id="providers-text"></span></span>
  </div>

  <div class="grid">
    <div class="card">
      <h2>API 端点</h2>
      <p>兼容 OpenAI 协议，把 base_url 指向本站即可。</p>
      <p class="muted">Base URL: <code id="base-url">/v1</code></p>
      <pre><span class="c"># 列出可用模型（能力别名）</span>
curl http://localhost:1314/v1/models

<span class="c"># 聊天补全（Bearer 为 FREE_ROUTER_API_TOKEN）</span>
curl http://localhost:1314/v1/chat/completions \
  -H "Authorization: Bearer $FREE_ROUTER_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}]}'</pre>
      <a class="btn ghost" href="/v1/models" target="_blank" rel="noopener">查看 /v1/models</a>
    </div>

    <div class="card">
      <h2>管理控制台</h2>
      <p>配置 Provider Key、查看路由与健康状态。</p>
      <p class="muted" id="admin-hint">默认仅监听本机回环地址，公网访问会被拒绝，这是出于安全考虑。</p>
      <a class="btn" href="/admin/" rel="noopener">打开 /admin/</a>
    </div>
  </div>

  <div class="note">
    <b>公网部署提示：</b>当前你是通过反向代理（如 Traefik）访问本站。
    管理面板默认只允许 localhost 访问；如需远程打开，必须同时设置
    <code>FREE_ROUTER_ADMIN_ALLOW_REMOTE=1</code> 和
    <code>FREE_ROUTER_ADMIN_TOKEN=&lt;强密码&gt;</code>，
    否则 /admin/ 会返回 403。反向代理还应自行加一层访问控制（BasicAuth / IP 白名单）。
    推理端点 <code>/v1/*</code> 在监听非回环地址时同样要求 <code>FREE_ROUTER_API_TOKEN</code>。
  </div>

  <footer>
    free-router {{.Version}} · <a href="/healthz" rel="noopener">/healthz</a> · <a href="/readyz" rel="noopener">/readyz</a> · <a href="/metrics" rel="noopener">/metrics</a>
  </footer>

</div>

<script>
(function(){
  var proto = location.protocol, host = location.host;
  document.getElementById('base-url').textContent = location.origin + '/v1';

  fetch('/healthz',{credentials:'same-origin'})
    .then(function(r){ return r.ok ? r.json() : Promise.reject(r.status); })
    .then(function(j){
      var dot = document.getElementById('dot-health');
      dot.className = 'dot ok';
      document.getElementById('health-text').textContent = '服务正常';
      var pill = document.getElementById('pill-providers');
      pill.hidden = false;
      var parts = [];
      if (typeof j.providers === 'number') parts.push(j.providers + ' 个 Provider');
      if (typeof j.free_models === 'number') parts.push(j.free_models + ' 个可用模型');
      document.getElementById('providers-text').textContent = parts.join(' · ');
    })
    .catch(function(){
      var dot = document.getElementById('dot-health');
      dot.className = 'dot bad';
      document.getElementById('health-text').textContent = '健康检查未通过';
    });
})();
</script>
</body>
</html>`))

// Handler returns an http.Handler that serves the landing page.
func Handler(cfg Config) http.Handler {
	data := pageData{Version: cfg.Version, AdminToken: cfg.AdminToken}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	})
}
