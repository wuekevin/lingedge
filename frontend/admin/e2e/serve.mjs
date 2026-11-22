// E2E 静态服务（等价于生产 Nginx 的部署形态：同源静态资源 + /api 反代到 lingedge-server，保留原始 Host——CSRF 的 Origin 校验依赖它）。
// 只服务测试用的构建产物；响应带严格 CSP（default-src 'none'；script-src 'self'；style-src 允许内联样式属性，Arco 使用），任何 CSP 违规都会被 E2E 的控制台监视断言为零。
// 环境变量：LE_E2E_SERVE_DIST（产物目录）、LE_E2E_PORT（监听端口）、LE_E2E_API（真实 server 的 http 地址）。
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

const dist = process.env.LE_E2E_SERVE_DIST;
const port = Number(process.env.LE_E2E_PORT);
const api = new URL(process.env.LE_E2E_API ?? '');
if (!dist || !port || !process.env.LE_E2E_API) throw new Error('LE_E2E_SERVE_DIST / LE_E2E_PORT / LE_E2E_API are required');

const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml', '.woff2': 'font/woff2', '.png': 'image/png', '.ico': 'image/x-icon' };
const CSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'";

function proxy(req, res) {
  const xff = [req.headers['x-forwarded-for'], req.socket.remoteAddress].filter(Boolean).join(', ');
  const up = http.request({ hostname: api.hostname, port: api.port, path: req.url, method: req.method, headers: { ...req.headers, host: req.headers.host, 'x-forwarded-for': xff } }, (r) => {
    res.writeHead(r.statusCode ?? 502, r.headers);
    r.pipe(res);
  });
  up.on('error', () => {
    res.writeHead(502, { 'Content-Type': 'application/json' });
    res.end('{"error":{"code":"proxy.unreachable","message":"upstream unreachable"}}');
  });
  req.pipe(up);
}

function serveStatic(req, res) {
  const u = new URL(req.url, 'http://x');
  let rel = decodeURIComponent(u.pathname);
  let file = path.join(dist, rel);
  if (!file.startsWith(dist + path.sep) && file !== dist) {
    res.writeHead(400).end();
    return;
  }
  const isAsset = path.extname(rel) !== '';
  if (!fs.existsSync(file) || fs.statSync(file).isDirectory()) {
    if (isAsset) {
      res.writeHead(404).end();
      return;
    }
    file = path.join(dist, 'index.html'); // SPA 回退（Nginx：try_files $uri /index.html）
  }
  const ext = path.extname(file);
  const headers = { 'Content-Type': TYPES[ext] ?? 'application/octet-stream', 'X-Content-Type-Options': 'nosniff', 'Referrer-Policy': 'no-referrer', 'X-Frame-Options': 'DENY' };
  if (ext === '.html') {
    headers['Content-Security-Policy'] = CSP;
    headers['Cache-Control'] = 'no-store';
  } else headers['Cache-Control'] = 'public, max-age=31536000, immutable';
  res.writeHead(200, headers);
  fs.createReadStream(file).pipe(res);
}

http
  .createServer((req, res) => (req.url?.startsWith('/api/') ? proxy(req, res) : serveStatic(req, res)))
  .listen(port, '127.0.0.1', () => console.log(`serving ${dist} on ${port}`));
