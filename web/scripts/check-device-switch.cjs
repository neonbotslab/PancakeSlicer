const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { gunzipSync } = require('node:zlib');

const [base, output] = process.argv.slice(2);
if (!base || !output) throw new Error('Usage: npm run test:device-switch -- <demo-dev-url> <output-directory>');
const origin = new URL(base).origin;
const fixtures = path.resolve(__dirname, '../src/demo/fixtures');
const { options } = JSON.parse(gunzipSync(fs.readFileSync(path.join(fixtures, 'sample/engine.json.gz'))));
const checkboxName = 'Open Device page after printer confirms job';
const countdown = (page) => page.locator('.device-switch-toast');
const close = (dialog) => dialog.getByRole('button', { name: 'Close', exact: true }).first().click();
fs.mkdirSync(output, { recursive: true });

async function preferences(page) {
  await page.getByRole('button', { name: 'Prepare', exact: true }).focus();
  await page.keyboard.press('Control+p');
  const dialog = page.getByRole('dialog', { name: 'Preferences', exact: true });
  const toggle = dialog.getByRole('checkbox', { name: checkboxName, exact: true });
  const input = dialog.getByRole('spinbutton', { name: 'Delay (seconds)', exact: true });
  await toggle.waitFor();
  return { dialog, toggle, input };
}

async function delay(input, value, expected = value) {
  await input.fill(String(value));
  await input.press('Tab');
  assert.equal(await input.inputValue(), String(expected));
}

async function saved(page, enabled, seconds) {
  await page.waitForFunction(({ enabled, seconds }) => {
    const preferences = JSON.parse(sessionStorage.getItem('pancake.demo.v1') ?? '{}').preferences?.document;
    return preferences?.['orca.web.openDeviceAfterPrint'] === enabled && preferences?.['orca.web.openDeviceDelay'] === seconds;
  }, { enabled, seconds });
}

async function configureFixture(page) {
  await page.waitForFunction(() => window.__plater?.getState().job.state === 'done' && !window.__plater.getState().job.stale);
  return page.evaluate(() => {
    const app = window.__plater;
    const { job, printer } = app.getState();
    const connection = { printer, id: 'device-switch-fixture', name: 'Fixture printer', kind: 'moonraker', has_secret: true };
    const at = new Date().toISOString();
    const status = { printer, link: 'online', online: true, state: 'idle', at };
    app.setState({ printerConnections: { enabled: true, live: true, connections: [connection], status: { [printer]: status }, transfers: [] } });
    const fetchDemo = window.fetch;
    window.fetch = (input, init) => {
      const url = new URL(input instanceof Request ? input.url : input, location.href);
      if (url.origin === location.origin && url.pathname === '/api/printer-sends') {
        const sends = app.getState().printerConnections.transfers.filter((t) => t.printer === url.searchParams.get('printer'));
        return Promise.resolve(Response.json({ sends, total: sends.length }));
      }
      return fetchDemo(input, init);
    };
    return { job: job.id, printer };
  });
}

async function openSend(page) {
  await page.evaluate(() => window.__plater.setState({ appDialog: 'send' }));
  const dialog = page.getByRole('dialog', { name: 'Send to printer', exact: true });
  await dialog.waitFor();
  return dialog;
}

const transfer = (fixture, id, patch = {}) => ({
  ...fixture, id, name: 'Fixture printer', kind: 'moonraker', start: true, filename: 'test.gcode',
  state: 'done', outcome: 'printing', sent: 100, total: 100, started: new Date().toISOString(), ...patch,
});

async function report(page, transfer) {
  await page.evaluate((next) => {
    const app = window.__plater;
    const connections = app.getState().printerConnections;
    app.setState({ printerConnections: { ...connections, transfers: [next, ...connections.transfers.filter(({ id }) => id !== next.id)] } });
  }, transfer);
}

async function send(page, t, { reconnect = false, early = false } = {}) {
  await page.evaluate(async ({ t, reconnect, early }) => {
    const resource = performance.getEntriesByType('resource').find(({ name }) => new URL(name).pathname === '/src/flow/transfer.ts');
    const { printerSend } = await import(resource?.name ?? '/src/flow/transfer.ts');
    const fetchDemo = window.fetch;
    const sendPath = `/api/jobs/${encodeURIComponent(t.job)}/send`;
    const method = reconnect ? 'GET' : 'POST';
    let received = 0;
    window.fetch = (input, options) => {
      const url = new URL(input instanceof Request ? input.url : input, location.href);
      if (url.origin !== location.origin || url.pathname !== sendPath || (options?.method ?? 'GET') !== method) return fetchDemo(input, options);
      received++;
      if (!reconnect) {
        const body = JSON.parse(options.body);
        if (body.printer !== t.printer || body.start !== t.start) throw new Error('Incorrect send POST target or start option');
      }
      if (early) {
        const app = window.__plater;
        const connections = app.getState().printerConnections;
        app.setState({ printerConnections: { ...connections, transfers: [t, ...connections.transfers] } });
      }
      return Promise.resolve(reconnect ? Response.json(t) : Response.json({ ...t, state: 'uploading', outcome: undefined }, { status: 202 }));
    };
    try {
      if (reconnect) await printerSend.reconnect(t.job, t.printer);
      else await printerSend.submit(t.printer, t.job, { start: t.start, filename: t.filename });
      if (received !== 1) throw new Error(`Expected one mocked ${method} send request, received ${received}`);
    } finally { window.fetch = fetchDemo; }
  }, { t, reconnect, early });
}

async function noCountdown(page, current) {
  assert.equal(await countdown(page).count(), 0);
  assert.equal(page.url(), current);
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  try {
    const context = await browser.newContext({ viewport: { width: 1360, height: 900 }, serviceWorkers: 'block' });
    await context.routeWebSocket('**/*', (socket) => socket.close());
    await context.route('**/*', (route) => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.origin !== origin || request.method() !== 'GET') return route.abort();
      if (url.pathname === '/api/options') return route.fulfill({ json: { ...options, devReset: false } });
      if (url.pathname === '/api/jobs/demo-sample/moves') {
        return route.fulfill({ body: fs.readFileSync(path.join(fixtures, 'sample/moves.gz')), contentType: 'application/octet-stream' });
      }
      if (url.pathname.startsWith('/api/')) return route.abort();
      const prefix = '/src/demo/fixtures/';
      if (url.pathname.startsWith(prefix) && url.pathname.endsWith('.gz') && !url.searchParams.has('import')) {
        const file = path.join(fixtures, url.pathname.slice(prefix.length));
        return route.fulfill({ body: fs.readFileSync(file), contentType: 'application/octet-stream' });
      }
      return route.continue();
    });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await page.goto(base);
    await page.waitForFunction(() => window.__plater?.getState().profileLibraryLoad.state === 'ready');
    const project = await page.evaluate(async () => (await (await fetch('/api/demo/sample')).json()).project);
    await page.goto(`${base.replace(/#.*$/, '')}#/p/${encodeURIComponent(project)}`);
    const workspace = page.url();
    let fixture = await configureFixture(page);

    let settings = await preferences(page);
    assert.equal(await settings.toggle.isChecked(), true);
    assert.equal(await settings.input.inputValue(), '5');
    await page.screenshot({ path: path.join(output, 'defaults-setting.png') });
    await delay(settings.input, 75, 60);
    await delay(settings.input, -3, 0);
    await delay(settings.input, 2.6, 3);
    await settings.toggle.uncheck();
    assert.equal(await settings.input.isDisabled(), true);
    await close(settings.dialog);
    await saved(page, false, 3);
    await page.reload();
    fixture = await configureFixture(page);
    settings = await preferences(page);
    assert.equal(await settings.toggle.isChecked(), false);
    assert.equal(await settings.input.inputValue(), '3');
    await close(settings.dialog);

    let sent = transfer(fixture, 'disabled-send');
    await send(page, sent);
    await report(page, sent);
    await noCountdown(page, workspace);
    settings = await preferences(page);
    await settings.toggle.check();
    await delay(settings.input, 5);
    await close(settings.dialog);
    await saved(page, true, 5);

    const restored = transfer(fixture, 'restored-send');
    await send(page, restored, { reconnect: true });
    await report(page, restored);
    await noCountdown(page, workspace);

    const upload = transfer(fixture, 'upload-only', { start: false, outcome: 'uploaded' });
    await send(page, upload);
    await report(page, upload);
    await noCountdown(page, workspace);

    let dialog = await openSend(page);
    sent = transfer(fixture, 'confirmed-send');
    await send(page, sent);
    await noCountdown(page, workspace);
    await report(page, transfer(fixture, 'unrelated-send'));
    await noCountdown(page, workspace);
    await report(page, { ...sent, outcome: 'unconfirmed' });
    await dialog.getByRole('heading', { name: 'Not confirmed by Fixture printer' }).waitFor();
    await noCountdown(page, workspace);
    await report(page, sent);
    await dialog.getByText('Opening Device in 5s', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'countdown-toast.png') });
    await dialog.getByText(/^Opening Device in [1-4]s$/).waitFor();
    await countdown(page).getByRole('button', { name: 'Close countdown', exact: true }).focus();
    await page.keyboard.press('Tab');
    assert.equal(await dialog.getByRole('button', { name: 'Close', exact: true }).evaluate((el) => el === document.activeElement), true);
    await dialog.getByRole('button', { name: 'Done', exact: true }).focus();
    await page.keyboard.press('Tab');
    const cancel = countdown(page).getByRole('button', { name: 'Cancel', exact: true });
    assert.equal(await cancel.evaluate((el) => el === document.activeElement), true);
    await page.keyboard.press('Enter');
    await countdown(page).waitFor({ state: 'hidden' });
    await close(dialog);
    await page.waitForTimeout(5_050);
    await noCountdown(page, workspace);

    dialog = await openSend(page);
    sent = transfer(fixture, 'withdrawn-confirmation-send');
    await send(page, sent);
    await report(page, sent);
    await countdown(page).getByText('Opening Device in 5s', { exact: true }).waitFor();
    await report(page, { ...sent, outcome: 'unconfirmed' });
    await countdown(page).waitFor({ state: 'hidden' });
    await close(dialog);
    await page.waitForTimeout(5_050);
    await noCountdown(page, workspace);

    dialog = await openSend(page);
    sent = transfer(fixture, 'closed-dialog-send');
    await send(page, sent);
    await report(page, { ...sent, outcome: 'unconfirmed' });
    await close(dialog);
    await report(page, sent);
    await countdown(page).getByText('Opening Device in 5s', { exact: true }).waitFor();
    await countdown(page).getByRole('button', { name: 'Close countdown', exact: true }).click();
    await countdown(page).waitFor({ state: 'hidden' });

    sent = transfer(fixture, 'left-workspace-send');
    await send(page, sent);
    await report(page, { ...sent, outcome: 'unconfirmed' });
    await page.getByRole('button', { name: 'Device', exact: true }).click();
    await page.waitForURL((url) => url.hash === '#/device');
    await page.goBack();
    await page.waitForURL(workspace);
    await page.waitForFunction(() => window.__plater.getState().tab === 'preview');
    await report(page, sent);
    await noCountdown(page, workspace);

    sent = transfer(fixture, 'pagehide-send');
    await send(page, sent);
    await report(page, sent);
    await countdown(page).waitFor();
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true })));
    await countdown(page).waitFor({ state: 'hidden' });
    await report(page, sent);
    await noCountdown(page, workspace);

    settings = await preferences(page);
    await delay(settings.input, 2);
    await close(settings.dialog);
    dialog = await openSend(page);
    sent = transfer(fixture, 'early-confirmation-send');
    await send(page, sent, { early: true });
    await countdown(page).getByText('Opening Device in 2s', { exact: true }).waitFor();
    await page.waitForURL((url) => url.hash === `#/device/${encodeURIComponent(fixture.printer)}`);
    await dialog.waitFor({ state: 'hidden' });
    await countdown(page).waitFor({ state: 'hidden' });
    await page.locator('.device-printer[aria-current="page"]').getByText('Fixture printer', { exact: true }).waitFor();
    await page.goBack();
    await page.waitForURL(workspace);
    await page.waitForFunction(() => window.__plater.getState().tab === 'preview');
    dialog = await openSend(page);
    await dialog.getByRole('button', { name: 'Print', exact: true }).waitFor();
    await close(dialog);

    sent = transfer(fixture, 'left-document-send');
    await send(page, sent);
    await report(page, sent);
    await countdown(page).waitFor();
    await page.evaluate(async () => {
      const resource = performance.getEntriesByType('resource').find(({ name }) => new URL(name).pathname === '/src/state/history.ts');
      const { leaveDocument } = await import(resource?.name ?? '/src/state/history.ts');
      leaveDocument();
    });
    await countdown(page).waitFor({ state: 'hidden' });
    await report(page, sent);
    await noCountdown(page, workspace);

    assert.deepEqual(errors, []);
    console.log('Verified Defaults and reload persistence, mocked send acceptance and printer confirmation, countdown ticks and cancellation, route changes, and navigation to the target printer.');
  } finally {
    await browser.close();
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
