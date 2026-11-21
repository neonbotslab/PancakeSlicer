import assert from 'node:assert/strict';
import test, { type TestContext } from 'node:test';
import { setImmediate } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { build } from 'vite';

const path = '/api/preferences';
async function loadServiceDocument(): Promise<typeof import('../src/persist/service-document.ts')> {
  const bundles = await build({
    configFile: false,
    publicDir: false,
    logLevel: 'silent',
    build: {
      write: false,
      lib: { entry: fileURLToPath(new URL('../src/persist/service-document.ts', import.meta.url)), formats: ['es'] },
    },
  });
  const bundle = Array.isArray(bundles) ? bundles[0] : bundles;
  assert.ok(bundle && 'output' in bundle);
  const entry = bundle.output.find((output) => output.type === 'chunk' && output.isEntry);
  assert.ok(entry?.type === 'chunk');
  const Channel = globalThis.BroadcastChannel;
  globalThis.BroadcastChannel = class extends Channel {
    constructor(name: string) {
      super(name);
      if ('unref' in this && typeof this.unref === 'function') this.unref();
    }
  };
  try { return await import(`data:text/javascript;base64,${Buffer.from(entry.code).toString('base64')}`); }
  finally { globalThis.BroadcastChannel = Channel; }
}

const { createServiceDocument } = await loadServiceDocument();
const remote = (revision: number, document: Record<string, string>) => Response.json({ revision, document });

function workspace(t: TestContext) {
  const pending: Promise<Response>[] = [];
  const requests: RequestInit[] = [];
  t.mock.method(globalThis, 'fetch', async (input: RequestInfo | URL, init: RequestInit = {}) => {
    assert.equal(input, path);
    requests.push(init);
    const response = pending.shift();
    assert.ok(response, `Missing fixture for ${init.method ?? 'GET'}`);
    return response;
  });
  t.after(() => assert.equal(pending.length, 0));
  return {
    store: createServiceDocument<Record<string, string>>(path, 'PUT', 'preferences'),
    reply: (response: Response | Promise<Response>) => { pending.push(Promise.resolve(response)); },
    methods: () => requests.map(({ method }) => method ?? 'GET'),
    saved: () => requests.filter(({ method }) => method === 'PUT').map(({ body }) => {
      assert.ok(typeof body === 'string');
      const value: unknown = JSON.parse(body);
      assert.ok(value && typeof value === 'object' && 'expectedRevision' in value && 'document' in value);
      return { revision: value.expectedRevision, document: value.document };
    }),
  };
}

test('live refresh skips stale revisions and adopts the newest announced document', async (t) => {
  const { store, reply, methods } = workspace(t);
  reply(remote(1, { printer: 'old' }));
  await store.hydrate();
  const held = Promise.withResolvers<Response>();
  reply(held.promise);
  store.followRevision(2);
  store.followRevision(3);
  store.followRevision(1);
  reply(remote(3, { printer: 'new' }));
  held.resolve(remote(2, { printer: 'stale' }));
  await setImmediate();
  assert.deepEqual(store.snapshot().document, { printer: 'new' });
  assert.equal(store.snapshot().revision, 3);
  assert.deepEqual(methods(), ['GET', 'GET', 'GET']);
});

test('an edit during live refresh survives and saves over a separate remote change', async (t) => {
  const { store, reply, methods, saved } = workspace(t);
  reply(remote(1, { theme: 'dark', printer: 'old' }));
  await store.hydrate();
  const held = Promise.withResolvers<Response>();
  reply(held.promise);
  store.followRevision(2);
  store.patch({ theme: 'light' });
  const changed = { theme: 'dark', printer: 'new' };
  held.resolve(remote(2, changed));
  await setImmediate();
  assert.deepEqual(store.snapshot().document, { theme: 'light', printer: 'old' });

  reply(Response.json({ revision: 2, document: changed, code: 'revision_conflict' }, { status: 409 }));
  reply(Response.json({ revision: 3 }));
  await store.flush();
  assert.deepEqual(store.snapshot().document, { theme: 'light', printer: 'new' });
  assert.deepEqual(saved(), [
    { revision: 1, document: { theme: 'light', printer: 'old' } },
    { revision: 2, document: { theme: 'light', printer: 'new' } },
  ]);
  assert.deepEqual(methods(), ['GET', 'GET', 'PUT', 'PUT']);
});

test('a live revision waits for a local write before refreshing', async (t) => {
  const { store, reply, methods } = workspace(t);
  reply(remote(1, { theme: 'dark', printer: 'old' }));
  await store.hydrate();
  store.patch({ theme: 'light' });
  store.followRevision(3);
  assert.deepEqual(methods(), ['GET']);

  const held = Promise.withResolvers<Response>();
  reply(held.promise);
  const writing = store.flush();
  await setImmediate();
  store.followRevision(4);
  assert.deepEqual(methods(), ['GET', 'PUT']);
  reply(remote(4, { theme: 'light', printer: 'new' }));
  held.resolve(Response.json({ revision: 2 }));
  await writing;
  await setImmediate();
  assert.deepEqual(store.snapshot().document, { theme: 'light', printer: 'new' });
  assert.deepEqual(methods(), ['GET', 'PUT', 'GET']);
});

test('reconnect can retry a failed refresh without disturbing the saved document', async (t) => {
  const { store, reply, methods } = workspace(t);
  reply(remote(1, { printer: 'old' }));
  await store.hydrate();
  reply(Response.json({}, { status: 502 }));
  store.followRevision(2);
  await setImmediate();
  assert.deepEqual(store.snapshot().document, { printer: 'old' });
  assert.equal(store.snapshot().state, 'ready');

  reply(remote(2, { printer: 'new' }));
  store.followRevision();
  await setImmediate();
  assert.deepEqual(store.snapshot().document, { printer: 'new' });
  assert.deepEqual(methods(), ['GET', 'GET', 'GET']);
});
