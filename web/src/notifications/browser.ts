import { shouldNotify, validateDestination, type BrowserNotice, type Destination } from './policy.ts';

const ENABLED = 'orca.notifications.enabled';
const ASKED = 'orca.notifications.asked';
const CLIENT_REQUEST = 'orca:notification-client';
const NAVIGATE = 'orca:notification-navigate';
const ROUTE_PARAM = 'orca-notification';
const WORKER_PATH = '/orca-sw.js';
const ICON = '/notification-icon.png';

type Permission = NotificationPermission | 'unsupported';
type Settings = Readonly<{
  enabled: boolean;
  permission: Permission;
  disabled: boolean;
  hidden: boolean;
  offerId: string | null;
}>;

const listeners = new Set<() => void>();
const pending = new Set<string>();
const sent = new Set<string>();
const latestByTag = new Map<string, string>();
let enabled = readFlag(ENABLED);
let asked = readFlag(ASKED);
let offerId: string | null = null;
let permissionRequest: 'idle' | 'pending' | 'canceled' = 'idle';
let navigate: ((destination: Destination) => void) | undefined;
let snapshot = readSettings();

function readFlag(key: string): boolean {
  try { return window.localStorage.getItem(key) === '1'; } catch { return false; }
}

function writeFlag(key: string, value: boolean): void {
  try { window.localStorage.setItem(key, value ? '1' : '0'); } catch { }
}

function browserPermission(): Permission {
  if (typeof window === 'undefined' || !window.isSecureContext || typeof Notification !== 'function') return 'unsupported';
  return Notification.permission;
}

function readSettings(): Settings {
  const permission = browserPermission();
  if (permission !== 'default' || enabled) offerId = null;
  return {
    enabled,
    permission,
    disabled: permission === 'unsupported' || permission === 'denied',
    hidden: typeof document !== 'undefined' && document.visibilityState === 'hidden',
    offerId,
  };
}

function refresh(delivered = false): void {
  const next = readSettings();
  if (!delivered && next.enabled === snapshot.enabled && next.permission === snapshot.permission
      && next.hidden === snapshot.hidden && next.offerId === snapshot.offerId) return;
  snapshot = next;
  for (const listener of listeners) {
    try { listener(); } catch { }
  }
}

function rememberOffer(): void {
  asked = true;
  writeFlag(ASKED, true);
}

export function notificationSettingsSnapshot(): Settings {
  return snapshot;
}

export function subscribeNotificationSettings(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

export function offerNotifications(id: string): void {
  if (asked || enabled || browserPermission() !== 'default' || readFlag(ASKED)) return;
  offerId = id;
  refresh();
}

export function markNotificationOfferShown(id: string): void {
  if (offerId !== id || asked) return;
  if (readFlag(ASKED)) {
    asked = true;
    offerId = null;
    refresh();
    return;
  }
  rememberOffer();
}

export function dismissNotificationOffer(): void {
  rememberOffer();
  offerId = null;
  refresh();
}

function saveEnabled(next: boolean): void {
  enabled = next;
  writeFlag(ENABLED, next);
}

function finishPermissionRequest(permission?: NotificationPermission): void {
  if (permissionRequest === 'pending' && permission === 'granted') saveEnabled(true);
  permissionRequest = 'idle';
  refresh();
}

export function setNotificationsEnabled(next: boolean): void {
  if (!next) {
    if (permissionRequest === 'pending') permissionRequest = 'canceled';
    saveEnabled(false);
    refresh();
    return;
  }

  const permission = browserPermission();
  if (permission === 'granted' || permission === 'default') {
    offerId = null;
    rememberOffer();
  }
  if (permission === 'granted') saveEnabled(true);
  if (permission === 'default') {
    const inFlight = permissionRequest !== 'idle';
    permissionRequest = 'pending';
    if (!inFlight) {
      try {
        const request = Notification.requestPermission();
        void request.then(finishPermissionRequest, () => finishPermissionRequest());
      } catch {
        finishPermissionRequest();
      }
    }
  }
  refresh();
}

function canDeliver(notice?: BrowserNotice): boolean {
  if (notice && latestByTag.get(notice.tag) !== notice.id) return false;
  const permission = browserPermission();
  return shouldNotify({
    hidden: typeof document !== 'undefined' && document.visibilityState === 'hidden',
    enabled,
    permission,
    supported: permission !== 'unsupported',
  });
}

function clientIdFor(worker: ServiceWorker): Promise<string | null> {
  return new Promise((resolve) => {
    const { port1, port2 } = new MessageChannel();
    const timeout = setTimeout(() => finish(null), 250);
    function finish(id: string | null): void {
      clearTimeout(timeout);
      port1.close();
      resolve(id);
    }
    port1.onmessage = (event) => {
      const value: unknown = event.data;
      finish(typeof value === 'string' && value ? value : null);
    };
    port1.onmessageerror = () => finish(null);
    try {
      worker.postMessage({ type: CLIENT_REQUEST }, [port2]);
    } catch {
      port2.close();
      finish(null);
    }
  });
}

async function showWithWorker(notice: BrowserNotice, options: NotificationOptions): Promise<boolean> {
  try {
    const registration = await navigator.serviceWorker?.getRegistration();
    const worker = registration?.active;
    if (!registration || !worker || worker.scriptURL !== new URL(WORKER_PATH, location.origin).href || !canDeliver(notice)) return false;
    const clientId = await clientIdFor(worker);
    if (!clientId || !canDeliver(notice)) return false;
    await registration.showNotification(notice.title, {
      ...options,
      data: { type: NAVIGATE, clientId, destination: notice.destination },
    });
    return true;
  } catch {
    return false;
  }
}

function showWithWindow(notice: BrowserNotice, options: NotificationOptions): boolean {
  if (!canDeliver(notice)) return false;
  try {
    const notification = new Notification(notice.title, options);
    notification.onclick = () => {
      window.focus();
      notification.close();
      navigate?.(notice.destination);
    };
    return true;
  } catch {
    return false;
  }
}

export function wasNotificationSent(id: string): boolean {
  return sent.has(id);
}

export async function publishNotice(notice: BrowserNotice): Promise<boolean> {
  if (sent.has(notice.id) || pending.has(notice.id)) return false;
  latestByTag.set(notice.tag, notice.id);
  if (!canDeliver(notice)) return false;
  pending.add(notice.id);
  try {
    const options = { body: notice.body, tag: notice.tag, icon: ICON };
    if (!await showWithWorker(notice, options) && !showWithWindow(notice, options)) return false;
    sent.add(notice.id);
    refresh(true);
    return true;
  } finally {
    pending.delete(notice.id);
  }
}

export function setNotificationNavigator(callback: (destination: Destination) => void): void {
  navigate = callback;
  if (typeof window === 'undefined') return;
  const url = new URL(window.location.href);
  const raw = url.searchParams.get(ROUTE_PARAM);
  if (raw === null) return;
  url.searchParams.delete(ROUTE_PARAM);
  try {
    window.history.replaceState(window.history.state, '', url);
    const destination = validateDestination(JSON.parse(raw));
    if (destination) callback(destination);
  } catch { }
}

function receiveNavigation(event: MessageEvent<unknown>): void {
  const data = event.data;
  if (!data || typeof data !== 'object' || !('type' in data) || data.type !== NAVIGATE || !('destination' in data)) return;
  const destination = validateDestination(data.destination);
  if (destination) navigate?.(destination);
}

async function watchPermission(): Promise<void> {
  if (browserPermission() === 'unsupported') return;
  try {
    const permission = await navigator.permissions?.query({ name: 'notifications' });
    permission?.addEventListener('change', () => refresh());
    refresh();
  } catch { }
}

if (typeof window !== 'undefined') {
  document.addEventListener('visibilitychange', () => refresh());
  window.addEventListener('focus', () => refresh());
  window.addEventListener('storage', (event) => {
    if (event.key === ENABLED || event.key === null) enabled = readFlag(ENABLED);
    if (event.key === ASKED || event.key === null) {
      asked = readFlag(ASKED);
      if (asked) offerId = null;
    }
    refresh();
  });
  navigator.serviceWorker?.addEventListener('message', receiveNavigation);
  void watchPermission();
}
