// A disposable cache of service responses that never change: a finished job's toolpaths and G-code
// (by job id) and project thumbnails (by version). Only a 200 whose Cache-Control the service marks
// `immutable` is kept, so a URL class listed here still caches nothing the service has not promised
// is final. User state, mutations, the app's modules and the HMR socket never reach this worker:
// browsers with static routing (Chrome 123+) send every other request straight to the network.
//
// Kill switch: open the app with ?sw=off (or set localStorage orca.sw = 'off'); src/sw.ts then
// unregisters the worker and deletes its caches. Bumping VERSION drops every older cache on activate.
const VERSION = 1;
const CACHE = `orca-immutable-v${VERSION}`;
const MAX_ENTRIES = 48;
const IMMUTABLE = [/^\/api\/jobs\/[^/]+\/(?:moves|download)$/, /^\/api\/project-thumbnails\/[^/]+$/];
const JOB = /^\/api\/jobs\/([^/]+)$/;

self.addEventListener('install', (event) => {
  self.skipWaiting();
  if (!event.addRoutes || typeof URLPattern === 'undefined') return;
  const fetchEvent = (pathname) => ({ condition: { urlPattern: new URLPattern({ pathname }) }, source: 'fetch-event' });
  // First match wins: the job and thumbnail routes reach the handler, everything else the network.
  event.waitUntil(event.addRoutes([fetchEvent('/api/jobs/*'), fetchEvent('/api/project-thumbnails/*'),
    { condition: { urlPattern: new URLPattern({ pathname: '/*' }) }, source: 'network' }]).catch(() => {}));
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    for (const name of await caches.keys()) if (name.startsWith('orca-immutable-') && name !== CACHE) await caches.delete(name);
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const request = event.request;
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;
  // A deleted job's files go with it.
  const job = request.method === 'DELETE' && JOB.exec(url.pathname);
  if (job) {
    event.waitUntil(forget(`/api/jobs/${job[1]}/`));
    return;
  }
  if (request.method !== 'GET' || request.headers.has('range') || !IMMUTABLE.some((r) => r.test(url.pathname))) return;
  event.respondWith(cacheFirst(request));
});

async function cacheFirst(request) {
  const cache = await caches.open(CACHE);
  const hit = await cache.match(request);
  if (hit) return hit;
  const response = await fetch(request);
  if (response.status === 200 && /\bimmutable\b/.test(response.headers.get('Cache-Control') ?? '')) {
    cache.put(request, response.clone()).then(() => trim(cache), () => {});
  }
  return response;
}

// Oldest first: Cache Storage keeps insertion order.
async function trim(cache) {
  const keys = await cache.keys();
  for (const key of keys.slice(0, Math.max(0, keys.length - MAX_ENTRIES))) await cache.delete(key);
}

async function forget(prefix) {
  const cache = await caches.open(CACHE);
  for (const key of await cache.keys()) if (new URL(key.url).pathname.startsWith(prefix)) await cache.delete(key);
}

self.addEventListener('message', (event) => {
  if (event.data?.type !== 'orca:notification-client' || event.source?.type !== 'window') return;
  event.ports[0]?.postMessage(event.source.id);
});

self.addEventListener('notificationclick', (event) => {
  const data = event.notification.data;
  if (data?.type !== 'orca:notification-navigate') return;
  const destination = notificationDestination(data.destination);
  if (!destination || !isNotificationId(data.clientId)) {
    event.notification.close();
    return;
  }
  event.waitUntil((async () => {
    const client = await self.clients.get(data.clientId);
    if (client?.type === 'window') {
      try { await client.focus(); } catch { }
      event.notification.close();
      client.postMessage({ type: data.type, destination });
      return;
    }
    event.notification.close();
    const url = new URL('/', self.location.origin);
    if (destination.kind !== 'focus') url.searchParams.set('orca-notification', JSON.stringify(destination));
    await self.clients.openWindow(url.href);
  })().catch(() => event.notification.close()));
});

function isNotificationId(value) {
  return typeof value === 'string' && value.trim().length > 0;
}

function notificationDestination(value) {
  if (!value || typeof value !== 'object') return null;
  if (value.kind === 'focus') return { kind: 'focus' };
  if (value.kind === 'device' && isNotificationId(value.printer)) return { kind: 'device', printer: value.printer };
  const { projectId, plate } = value;
  if (!isNotificationId(projectId) || !Number.isSafeInteger(plate) || plate < 1) return null;
  if (value.kind === 'preview' && isNotificationId(value.jobId) && isNotificationId(value.signature)) return { kind: 'preview', projectId, plate, jobId: value.jobId, signature: value.signature };
  if (value.kind === 'slice-error' && isNotificationId(value.attempt) && typeof value.error === 'string') return { kind: 'slice-error', projectId, plate, attempt: value.attempt, error: value.error };
  return null;
}
