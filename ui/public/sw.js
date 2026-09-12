// No network inventory, HTML session, API response, credential or mutation is
// cached or queued. The installed app always requires the live controller.
self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()))
self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url)
  if (event.request.method !== 'GET' || event.request.mode !== 'navigate' ||
      url.origin !== self.location.origin || url.pathname.startsWith('/api/')) return
  event.respondWith(fetch(event.request).catch(() => new Response(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="dark light"><title>Controller unreachable — oonfeeWRT</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;font:15px/1.7 system-ui;background:#0f1114;color:#f2f4f7}main{max-width:420px;padding:32px}h1{font-size:26px;letter-spacing:-.04em}p{color:#a0a6b0}a{display:inline-block;padding:10px 18px;border-radius:9px;background:#1f6fc8;color:white;text-decoration:none}</style>
<main><h1>Your controller is unreachable</h1><p>Connect to your management network or VPN and check that oonfeeWRT is running. Nothing has been changed or queued while offline.</p><a href="/">Try again</a></main></html>`, {
    status: 503, headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' },
  })))
})
