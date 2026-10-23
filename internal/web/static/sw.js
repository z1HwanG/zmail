/* zMail Service Worker：缓存应用外壳，离线时给出兜底页。
 * 说明：API 与 SSE 一律走网络，不缓存邮件数据，避免读到旧邮件。
 */
'use strict';

var VERSION = 'zmail-v9';
// app.css/app.js 带 ?v=<构建指纹>，由浏览器按版本缓存；这里只预缓存不常变的外壳资源。
var SHELL = [
  '/static/manifest.webmanifest',
  '/static/icons/icon.svg',
  '/static/icons/icon-192.png',
  '/static/icons/icon-512.png'
];

self.addEventListener('install', function (event) {
  event.waitUntil(
    caches.open(VERSION).then(function (cache) {
      return Promise.all(SHELL.map(function (url) {
        return cache.add(new Request(url, { cache: 'reload' })).catch(function () {});
      }));
    }).then(function () { return self.skipWaiting(); })
  );
});

self.addEventListener('activate', function (event) {
  event.waitUntil(
    caches.keys().then(function (keys) {
      return Promise.all(keys.map(function (key) {
        return key === VERSION ? null : caches.delete(key);
      }));
    }).then(function () { return self.clients.claim(); })
  );
});

function isCacheable(url, request) {
  if (request.method !== 'GET') return false;
  if (url.origin !== self.location.origin) return false;
  if (url.pathname.indexOf('/api/') === 0) return false;
  if (url.pathname.indexOf('/static/') === 0) return true;
  return url.pathname === '/sw.js' || url.pathname === '/static/sw.js';
}

function offlinePage() {
  var html = '<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">' +
    '<meta name="viewport" content="width=device-width,initial-scale=1"><title>zMail · 离线</title>' +
    '<style>body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;' +
    'font:15px/1.6 system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;' +
    'background:#eef1f4;color:#121820}main{max-width:360px;padding:28px;text-align:center}' +
    'h1{font-size:20px;margin:0 0 8px}p{color:#66707a;margin:0 0 20px}' +
    'a{display:inline-block;padding:10px 18px;border-radius:8px;background:#1c4f8a;color:#fff;text-decoration:none}' +
    '@media (prefers-color-scheme:dark){body{background:#0e1318;color:#e7ebef}p{color:#a1acb6}}</style></head>' +
    '<body><main><h1>zMail 暂时离线</h1><p>网络不可用或服务未启动。恢复连接后点击下方按钮重试。</p>' +
    '<a href="/mail">重新连接</a></main></body></html>';
  return new Response(html, {
    status: 200,
    headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' }
  });
}

self.addEventListener('fetch', function (event) {
  var request = event.request;
  if (request.method !== 'GET') return;
  var url;
  try { url = new URL(request.url); } catch (e) { return; }
  if (url.origin !== self.location.origin) return;
  if (url.pathname.indexOf('/api/') === 0) return;

  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request).catch(function () {
        return caches.match(request).then(function (hit) {
          return hit || offlinePage();
        });
      })
    );
    return;
  }

  if (!isCacheable(url, request)) return;
  event.respondWith(
    caches.match(request).then(function (hit) {
      var network = fetch(request).then(function (res) {
        if (res && res.ok) {
          var copy = res.clone();
          caches.open(VERSION).then(function (cache) { cache.put(request, copy); });
        }
        return res;
      }).catch(function () { return hit; });
      return hit || network;
    })
  );
});
