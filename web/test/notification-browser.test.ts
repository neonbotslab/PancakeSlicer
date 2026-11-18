import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test, { type TestContext } from 'node:test';
import { runInNewContext } from 'node:vm';
import type { BrowserNotice, Destination } from '../src/notifications/policy.ts';

const origin = 'https://slicer.example';
const notice: BrowserNotice = {
  id: 'slice-j2', title: 'Slice ready', body: 'Plate 2', tag: 'orca-project:p1',
  destination: { kind: 'preview', projectId: 'p1', plate: 2, jobId: 'j2', signature: 'signature-2' },
};

let moduleId = 0;
async function loadBrowser(): Promise<typeof import('../src/notifications/browser.ts')> {
  const url = new URL('../src/notifications/browser.ts', import.meta.url);
  url.search = `test=${++moduleId}`;
  return import(url.href);
}

type WorkerRegistration = {
  active: { scriptURL: string; postMessage(message: unknown, ports: MessagePort[]): void };
  showNotification(title: string, options: NotificationOptions): Promise<void>;
};

function fakeBrowser(t: TestContext, permission: NotificationPermission = 'default') {
  const storage = new Map<string, string>();
  const calls: string[] = [];
  const location = new URL('/', origin);
  const window = Object.assign(new EventTarget(), {
    location,
    isSecureContext: true,
    focus: () => { calls.push('focus'); },
    history: { state: {}, replaceState: (_state: unknown, _title: string, url: URL) => { location.href = url.href; } },
    localStorage: {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: (key: string, value: string) => storage.set(key, value),
    },
  });
  const document = Object.assign(new EventTarget(), { hidden: false });
  Object.defineProperty(document, 'visibilityState', { get: () => document.hidden ? 'hidden' : 'visible' });
  const permissionStatus = new EventTarget();
  const serviceWorker = Object.assign(new EventTarget(), {
    getRegistration: async (): Promise<WorkerRegistration | undefined> => undefined,
  });
  const prompt = Promise.withResolvers<NotificationPermission>();
  const displayed: FakeNotification[] = [];
  class FakeNotification {
    static permission = permission;
    static requestPermission(): Promise<NotificationPermission> {
      calls.push('permission prompt');
      return prompt.promise;
    }
    onclick: (() => void) | null = null;
    readonly title: string;
    readonly options: NotificationOptions;
    constructor(title: string, options: NotificationOptions) {
      this.title = title;
      this.options = options;
      displayed.push(this);
    }
    close(): void { calls.push('close'); }
  }
  const navigator = { serviceWorker, permissions: { query: async () => permissionStatus } };
  for (const [name, value] of Object.entries({ window, document, location, navigator, Notification: FakeNotification })) {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, name);
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    t.after(() => {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else Reflect.deleteProperty(globalThis, name);
    });
  }
  return { storage, window, document, serviceWorker, permissionStatus, FakeNotification, prompt, calls, displayed };
}

test('permission is requested in the enabling click and the opt-in survives reload', async (t) => {
  const env = fakeBrowser(t);
  const browser = await loadBrowser();
  const initial = browser.notificationSettingsSnapshot();
  assert.equal(initial.enabled, false);
  assert.equal(initial, browser.notificationSettingsSnapshot());
  browser.offerNotifications('slice');
  assert.deepEqual(env.calls, []);
  browser.setNotificationsEnabled(true);
  assert.deepEqual(env.calls, ['permission prompt']);
  assert.equal(browser.notificationSettingsSnapshot().enabled, false);
  assert.equal(browser.notificationSettingsSnapshot().offerId, null);

  env.FakeNotification.permission = 'granted';
  env.prompt.resolve('granted');
  await env.prompt.promise;
  assert.equal(browser.notificationSettingsSnapshot().enabled, true);
  assert.equal((await loadBrowser()).notificationSettingsSnapshot().enabled, true);

  browser.setNotificationsEnabled(false);
  assert.equal((await loadBrowser()).notificationSettingsSnapshot().enabled, false);
  assert.deepEqual(env.calls, ['permission prompt']);
});

test('the offer is kept until dismissed while its shown marker survives reload', async (t) => {
  const env = fakeBrowser(t);
  env.document.hidden = true;
  const browser = await loadBrowser();
  browser.offerNotifications('first');
  browser.offerNotifications('second');
  assert.equal(browser.notificationSettingsSnapshot().offerId, 'second');
  env.document.hidden = false;
  env.document.dispatchEvent(new Event('visibilitychange'));
  assert.equal(browser.notificationSettingsSnapshot().hidden, false);
  browser.markNotificationOfferShown('second');
  assert.equal(browser.notificationSettingsSnapshot().offerId, 'second');
  const reloaded = await loadBrowser();
  reloaded.offerNotifications('third');
  assert.equal(reloaded.notificationSettingsSnapshot().offerId, null);
  browser.dismissNotificationOffer();
  assert.equal(browser.notificationSettingsSnapshot().offerId, null);
  assert.deepEqual(env.calls, []);
});

test('permission updates disable delivery and missing or insecure APIs disable the switch', async (t) => {
  const env = fakeBrowser(t, 'granted');
  const browser = await loadBrowser();
  env.document.hidden = true;
  assert.equal(await browser.publishNotice(notice), false);
  browser.setNotificationsEnabled(true);
  env.FakeNotification.permission = 'denied';
  env.permissionStatus.dispatchEvent(new Event('change'));
  assert.equal(browser.notificationSettingsSnapshot().disabled, true);
  assert.equal(browser.notificationSettingsSnapshot().permission, 'denied');
  assert.equal(await browser.publishNotice(notice), false);
  env.window.isSecureContext = false;
  env.window.dispatchEvent(new Event('focus'));
  assert.equal(browser.notificationSettingsSnapshot().permission, 'unsupported');
  assert.equal(await browser.publishNotice(notice), false);
  env.window.isSecureContext = true;
  Reflect.deleteProperty(globalThis, 'Notification');
  assert.equal((await loadBrowser()).notificationSettingsSnapshot().disabled, true);
  assert.deepEqual(env.calls, []);
  assert.deepEqual(env.displayed, []);
});

test('an enabled foreground notification never reaches a display API', async (t) => {
  const env = fakeBrowser(t, 'granted');
  const browser = await loadBrowser();
  browser.setNotificationsEnabled(true);
  let lookedUp = false;
  env.serviceWorker.getRegistration = async () => { lookedUp = true; return undefined; };
  assert.equal(await browser.publishNotice(notice), false);
  assert.equal(lookedUp, false);
  assert.deepEqual(env.displayed, []);
});

test('delivery is skipped when the tab becomes visible during the worker handshake', async (t) => {
  const env = fakeBrowser(t, 'granted');
  const browser = await loadBrowser();
  browser.setNotificationsEnabled(true);
  env.document.hidden = true;
  const reply = Promise.withResolvers<MessagePort>();
  const displayed: string[] = [];
  env.serviceWorker.getRegistration = async () => ({
    active: { scriptURL: `${origin}/orca-sw.js`, postMessage: (_message, ports) => reply.resolve(ports[0]) },
    showNotification: async (title) => { displayed.push(title); },
  });
  const delivery = browser.publishNotice(notice);
  const port = await reply.promise;
  env.document.hidden = false;
  port.postMessage('from-tab');
  port.close();
  assert.equal(await delivery, false);
  assert.equal(browser.wasNotificationSent(notice.id), false);
  assert.deepEqual(displayed, []);
  assert.deepEqual(env.displayed, []);
});

test('the worker shows one notice for concurrent duplicate events and publishes the matching id', async (t) => {
  const env = fakeBrowser(t, 'granted');
  const browser = await loadBrowser();
  browser.setNotificationsEnabled(true);
  env.document.hidden = true;
  const delivery = Promise.withResolvers<void>();
  const shown: { title: string; options: NotificationOptions }[] = [];
  env.serviceWorker.getRegistration = async () => ({
    active: { scriptURL: `${origin}/orca-sw.js`, postMessage: (_message, [port]) => { port.postMessage('from-tab'); port.close(); } },
    showNotification: async (title, options) => { shown.push({ title, options }); await delivery.promise; },
  });
  const received: boolean[] = [];
  const unsub = browser.subscribeNotificationSettings(() => received.push(browser.wasNotificationSent(notice.id)));
  const complete = browser.publishNotice(notice);
  assert.equal(await browser.publishNotice(notice), false);
  delivery.resolve();
  assert.equal(await complete, true);
  assert.equal(await browser.publishNotice(notice), false);
  assert.deepEqual(shown, [{ title: notice.title, options: {
    body: notice.body, tag: notice.tag, icon: '/notification-icon.png',
    data: { type: 'orca:notification-navigate', clientId: 'from-tab', destination: notice.destination },
  } }]);
  assert.deepEqual(received, [true]);
  assert.deepEqual(env.displayed, []);
  unsub();
});

test('a rejected worker display falls back to the window and routes after focus and close', async (t) => {
  const env = fakeBrowser(t, 'granted');
  const browser = await loadBrowser();
  browser.setNotificationsEnabled(true);
  env.document.hidden = true;
  env.serviceWorker.getRegistration = async () => ({
    active: { scriptURL: `${origin}/orca-sw.js`, postMessage: (_message, [port]) => { port.postMessage('from-tab'); port.close(); } },
    showNotification: async () => { throw new Error('display failed'); },
  });
  const destinations: Destination[] = [];
  browser.setNotificationNavigator((destination) => { env.calls.push('navigate'); destinations.push(destination); });
  assert.equal(await browser.publishNotice(notice), true);
  assert.equal(env.displayed.length, 1);
  assert.equal(env.displayed[0].title, notice.title);
  assert.equal(env.displayed[0].options.tag, notice.tag);
  env.displayed[0].onclick?.();
  assert.deepEqual(env.calls, ['focus', 'close', 'navigate']);
  assert.deepEqual(destinations, [notice.destination]);
});

test('a worker without the client handshake yields one window notification', async (t) => {
  const env = fakeBrowser(t, 'granted');
  const browser = await loadBrowser();
  browser.setNotificationsEnabled(true);
  env.document.hidden = true;
  const displayed: string[] = [];
  env.serviceWorker.getRegistration = async () => ({
    active: { scriptURL: `${origin}/orca-sw.js`, postMessage: (_message, [port]) => port.close() },
    showNotification: async (title) => { displayed.push(title); },
  });
  assert.equal(await browser.publishNotice(notice), true);
  assert.deepEqual(displayed, []);
  assert.equal(env.displayed.length, 1);
});

test('navigation accepts a validated worker message or a one-time startup destination', async (t) => {
  const env = fakeBrowser(t);
  env.window.location.hash = `orca-notification=${encodeURIComponent(JSON.stringify(notice.destination))}`;
  const browser = await loadBrowser();
  const destinations: Destination[] = [];
  browser.setNotificationNavigator((destination) => destinations.push(destination));
  assert.equal(env.window.location.hash, '');
  browser.setNotificationNavigator((destination) => destinations.push(destination));
  const post = (destination: unknown) => env.serviceWorker.dispatchEvent(new MessageEvent('message', {
    data: { type: 'orca:notification-navigate', destination },
  }));
  post({ kind: 'slice-error', projectId: 'p1', plate: 2 });
  post({ kind: 'device', printer: 'printer-1', url: 'https://example.net' });
  assert.deepEqual(destinations, [notice.destination, { kind: 'device', printer: 'printer-1' }]);
});

test('a worker notification click focuses only its originating client and opens a same-origin fallback', async () => {
  const script = await readFile(new URL('../public/orca-sw.js', import.meta.url), 'utf8');
  const handlers = new Map<string, (event: object) => void>();
  const calls: unknown[] = [];
  const router = {
    type: 'window',
    focus: async () => { calls.push('focus'); },
    postMessage: (data: unknown) => { calls.push(structuredClone(data)); },
  };
  runInNewContext(script, { URL, self: {
    location: { origin },
    addEventListener: (name: string, handler: (event: object) => void) => handlers.set(name, handler),
    clients: {
      get: async (id: string) => { calls.push(id); return id === 'matching' ? router : undefined; },
      openWindow: async (url: string) => { calls.push(url); },
    },
  } });
  handlers.get('message')?.({
    data: { type: 'orca:notification-client', clientId: 'untrusted' },
    source: { type: 'window', id: 'matching' },
    ports: [{ postMessage: (id: string) => calls.push(id) }],
  });
  assert.deepEqual(calls.splice(0), ['matching']);
  async function click(clientId: string, destination: unknown): Promise<void> {
    let completion: Promise<void> | undefined;
    handlers.get('notificationclick')?.({
      notification: { data: { type: 'orca:notification-navigate', clientId, destination }, close: () => calls.push('close') },
      waitUntil: (task: Promise<void>) => { completion = task; },
    });
    await completion;
  }
  await click('matching', notice.destination);
  assert.deepEqual(calls.splice(0), ['matching', 'focus', 'close', { type: 'orca:notification-navigate', destination: notice.destination }]);
  const expected = new URL('/', origin);
  expected.hash = `orca-notification=${encodeURIComponent(JSON.stringify(notice.destination))}`;
  await click('closed', { ...notice.destination, url: 'https://example.net' });
  assert.deepEqual(calls.splice(0), ['closed', 'close', expected.href]);
  const errorDestination = { kind: 'slice-error', projectId: 'p1', plate: 1, attempt: 'a1', error: 'Private diagnostic detail' };
  await click('closed', errorDestination);
  const fallback = new URL(String(calls.at(-1)));
  assert.equal(fallback.search, '');
  assert.deepEqual(JSON.parse(new URLSearchParams(fallback.hash.slice(1)).get('orca-notification') ?? 'null'), errorDestination);
  calls.splice(0);
  await click('closed', { kind: 'link', url: 'https://example.net' });
  assert.deepEqual(calls, ['close']);
});


test('an older handshake cannot replace a newer notification with the same tag', async (t) => {
  const env = fakeBrowser(t, 'granted');
  const browser = await loadBrowser();
  browser.setNotificationsEnabled(true);
  env.document.hidden = true;
  const first = Promise.withResolvers<MessagePort>();
  let requests = 0;
  const shown: string[] = [];
  env.serviceWorker.getRegistration = async () => ({
    active: { scriptURL: `${origin}/orca-sw.js`, postMessage: (_message, [port]) => {
      if (++requests === 1) first.resolve(port);
      else { port.postMessage('from-tab'); port.close(); }
    } },
    showNotification: async (title) => { shown.push(title); },
  });
  const older = browser.publishNotice(notice);
  const port = await first.promise;
  assert.equal(await browser.publishNotice({ ...notice, id: 'newer', title: 'Export ready' }), true);
  port.postMessage('from-tab');
  port.close();
  assert.equal(await older, false);
  assert.deepEqual(shown, ['Export ready']);
  assert.deepEqual(env.displayed, []);
});
