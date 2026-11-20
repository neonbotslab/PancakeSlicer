const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const [base, output] = process.argv.slice(2);
if (!base || !output) throw new Error('Usage: npm run test:flow-calibration -- <demo-url> <output-directory>');
const origin = new URL(base).origin;
const assets = path.resolve(__dirname, '../dist-demo/assets');
const autoNote = 'Auto skips calibration if the filament was calibrated recently; printers without Auto use On.';
const tpuNote = 'For TPU, Auto/On reuse previous or default values.';
fs.mkdirSync(output, { recursive: true });

const flow = (dialog) => dialog.getByRole('radiogroup', { name: 'Flow calibration', exact: true });
const radio = (dialog, name) => flow(dialog).getByRole('radio', { name, exact: true });
const select = (dialog, name) => radio(dialog, name).click();

async function assertChoices(dialog, selected, supportsAuto = true) {
  await flow(dialog).getByRole('radio', { name: selected, exact: true, checked: true }).waitFor();
  assert.deepEqual(await flow(dialog).getByRole('radio').allTextContents(), supportsAuto ? ['Off', 'Auto', 'On'] : ['Off', 'On']);
  assert.equal(await flow(dialog).getByRole('radio', { checked: true }).count(), 1);
}

async function ready(page) {
  await page.waitForFunction(() => window.__plater?.getState().profileLibraryLoad.state === 'ready');
}

async function preferences(page) {
  await page.keyboard.press('Control+p');
  const dialog = page.getByRole('dialog', { name: 'Preferences', exact: true });
  await flow(dialog).waitFor();
  await dialog.getByText(autoNote, { exact: true }).waitFor();
  return dialog;
}

async function close(dialog) {
  await dialog.getByRole('button', { name: 'Close', exact: true }).first().click();
  await dialog.waitFor({ state: 'hidden' });
}

async function openSend(page, model, material = 'PLA') {
  await page.evaluate(({ model, material }) => {
    const app = window.__plater;
    const state = app.getState();
    const key = state.printer;
    const profile = state.records.printers.find((p) => p.id === key);
    if (!profile || !state.job.id || !profile.slots.length) throw new Error('Demo slice, printer or filament fixture missing');
    const connection = { printer: key, id: 'flow-fixture', name: model, model, kind: 'bambu-lan', has_secret: true,
      auto_flow_calibration: model === 'H2D', host: 'fixture-printer.invalid', serial: 'FIXTURE0000' };
    const at = new Date().toISOString();
    const status = { printer: key, link: 'online', online: true, state: 'idle', at, trays: [{ id: 254, ams: 254, slot: 0, type: material, color: '#d68b55', kind: 'external' }] };
    window.flowFixture = { jobId: state.job.id, slotId: profile.slots[0].id, material, connection };
    app.setState({ records: { ...state.records, printers: state.records.printers.map((p) => p === profile ? { ...p, name: model } : p) },
      printerConnections: { enabled: true, live: true, transfers: [], connections: [connection], status: { [key]: status } }, appDialog: 'send' });
  }, { model, material });
  const dialog = page.getByRole('dialog', { name: 'Send to printer', exact: true });
  await flow(dialog).waitFor();
  await dialog.getByText('Slice your plate first before sending it to the printer.', { exact: true }).waitFor({ state: 'hidden' });
  return dialog;
}

async function capture(page, dialog, name) {
  for (const width of [1360, 390]) {
    await page.setViewportSize({ width, height: 900 });
    const bounds = await flow(dialog).evaluate((el) => {
      const group = el.getBoundingClientRect();
      const container = el.closest('.modal-body');
      return { left: group.left, right: group.right, container: container.clientWidth,
        content: container.scrollWidth, viewport: innerWidth, document: document.documentElement.scrollWidth };
    });
    assert.ok(bounds.left >= 0 && bounds.right <= width && bounds.content <= bounds.container && bounds.document <= width, JSON.stringify(bounds));
    await page.screenshot({ path: path.join(output, `${name}-${width}.png`) });
  }
  await page.setViewportSize({ width: 1360, height: 900 });
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  try {
    const context = await browser.newContext({ viewport: { width: 1360, height: 900 }, serviceWorkers: 'block' });
    await context.routeWebSocket('**/*', (socket) => socket.close());
    await context.route('**/*', async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.origin !== origin || request.method() !== 'GET' || url.pathname.startsWith('/api/')) return route.abort();
      if (url.pathname.endsWith('.gz')) {
        const asset = path.join(assets, path.basename(url.pathname));
        if (fs.existsSync(asset)) return route.fulfill({ body: fs.readFileSync(asset), contentType: 'application/octet-stream' });
      }
      return route.continue();
    });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await page.goto(base);

    async function installFixtures() {
      await ready(page);
      await page.evaluate(() => {
        const fetchDemo = window.fetch.bind(window);
        window.fetch = (input, options) => {
          const url = new URL(input instanceof Request ? input.url : String(input), location.href);
          const fixture = window.flowFixture;
          if (url.origin !== location.origin || !fixture) return fetchDemo(input, options);
          if (url.pathname === `/api/jobs/${encodeURIComponent(fixture.jobId)}/bambu-mapping`) {
            return Promise.resolve(Response.json({ filament_map: [1], filament_volume_map: [0], materials: [
              { slot: 1, type: fixture.material, color: '#d68b55', id: '', flow: '', diameter: 0.4, object: true, support: false },
            ] }));
          }
          if (url.pathname === `/api/jobs/${encodeURIComponent(fixture.jobId)}/send/plan`) {
            const material = { type: fixture.material, color: '#d68b55' };
            return Promise.resolve(Response.json({ slots: [{ filament: 1, slot: fixture.slotId, tray: 'ext', used: true,
              state: 'confirmed', expected: material, reported: material }] }));
          }
          const printer = encodeURIComponent(fixture.connection.printer);
          if (url.pathname === `/api/printer-slots/${printer}`) {
            return Promise.resolve(Response.json({ online: true, slots: [] }));
          }
          if (url.pathname === `/api/printer-connections/${printer}` && options?.method === 'PUT') {
            const edit = JSON.parse(options.body);
            window.flowSavedConnection = { model: edit.model, serial: edit.serial };
            return Promise.resolve(Response.json({ ...fixture.connection, ...edit, auto_flow_calibration: false }));
          }
          return fetchDemo(input, options);
        };
      });
      const sample = await page.evaluate(async () => (await (await fetch('/api/demo/sample')).json()).project);
      if (!page.url().endsWith(`#/p/${encodeURIComponent(sample)}`)) await page.goto(`${base.replace(/#.*$/, '')}#/p/${encodeURIComponent(sample)}`);
      await page.waitForFunction(() => {
        const job = window.__plater.getState().job;
        return job.state === 'done' && !job.stale;
      });
    }

    await installFixtures();
    let dialog = await preferences(page);
    await assertChoices(dialog, 'Auto');
    await capture(page, dialog, 'preferences');
    await close(dialog);
    dialog = await openSend(page, 'H2D');
    await assertChoices(dialog, 'Auto');
    await capture(page, dialog, 'send-h2d');
    await close(dialog);
    dialog = await openSend(page, 'P1S');
    await assertChoices(dialog, 'On', false);
    await capture(page, dialog, 'send-p1s');
    await close(dialog);

    for (const selected of ['Off', 'On', 'Auto']) {
      dialog = await preferences(page);
      await select(dialog, selected);
      await assertChoices(dialog, selected);
      await close(dialog);
      await page.waitForFunction((value) => JSON.parse(sessionStorage.getItem('pancake.demo.v1') ?? '{}').preferences?.document?.['orca.web.flowCalibration'] === value, selected.toLowerCase());
      await page.reload();
      await installFixtures();
      dialog = await preferences(page);
      await assertChoices(dialog, selected);
      await close(dialog);

      dialog = await openSend(page, 'H2D');
      await assertChoices(dialog, selected);
      const expectedOverride = selected === 'Off' ? 'Auto' : selected === 'Auto' ? 'On' : 'Off';
      await radio(dialog, selected).focus();
      await page.keyboard.down('ArrowRight');
      await assertChoices(dialog, expectedOverride);
      assert.equal(await radio(dialog, expectedOverride).evaluate((element) => element === document.activeElement), true);
      await page.keyboard.up('ArrowRight');
      await close(dialog);
      dialog = await openSend(page, 'H2D');
      await assertChoices(dialog, selected);
      await close(dialog);

      dialog = await openSend(page, 'P1S');
      await assertChoices(dialog, selected === 'Auto' ? 'On' : selected, false);
      await close(dialog);
    }

    dialog = await openSend(page, 'H2D', 'TPU');
    await assertChoices(dialog, 'Auto');
    await dialog.getByText(tpuNote, { exact: true }).waitFor();
    await select(dialog, 'On');
    await dialog.getByText(tpuNote, { exact: true }).waitFor();
    await select(dialog, 'Off');
    await dialog.getByText(tpuNote, { exact: true }).waitFor({ state: 'hidden' });
    await close(dialog);
    dialog = await openSend(page, 'H2D');
    await dialog.getByRole('button', { name: 'Connection', exact: true }).click();
    const editor = page.getByRole('dialog', { name: 'Printer connection', exact: true });
    await editor.getByLabel('Serial number', { exact: true }).fill('FIXTURE0001');
    await editor.getByRole('button', { name: 'Save', exact: true }).click();
    await page.waitForFunction(() => window.flowSavedConnection?.serial === 'FIXTURE0001');
    assert.equal(await page.evaluate(() => window.flowSavedConnection.model), undefined);
    await close(dialog);
    assert.deepEqual(errors, []);
    console.log('Verified Flow calibration defaults, persistence, keyboard overrides, printer fallback, TPU helper, model reset, and desktop and phone layouts.');
  } finally {
    await browser.close();
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
