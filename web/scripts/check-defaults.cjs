const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const [base, output] = process.argv.slice(2);
if (!base || !output) throw new Error('Usage: node scripts/check-defaults.cjs <demo-url> <output-directory>');
const root = path.resolve(__dirname, '..');
const noteText = 'No printer set up yet, so Export stays primary.';
fs.mkdirSync(output, { recursive: true });

async function preferences(page, action) {
  await page.keyboard.press('Control+p');
  const dialog = page.getByRole('dialog', { name: 'Preferences', exact: true });
  await dialog.waitFor();
  if (action) {
    const choice = dialog.getByRole('radio', { name: action, exact: true });
    await choice.click();
    assert.equal(await choice.getAttribute('aria-checked'), 'true');
  }
  return dialog;
}

async function closePreferences(dialog) {
  await dialog.getByRole('button', { name: 'Close', exact: true }).last().click();
  await dialog.waitFor({ state: 'hidden' });
}

async function configured(page, exists, online = false) {
  await page.evaluate(({ exists, online }) => {
    const s = window.__plater.getState();
    window.__plater.setState({ printerConnections: {
      enabled: true, live: true, transfers: [],
      connections: exists ? [{ printer: s.printer, id: 'defaults-browser', name: 'Test printer', kind: 'moonraker', has_secret: true }] : [],
      status: exists ? { [s.printer]: { printer: s.printer, link: online ? 'online' : 'retrying', online, state: online ? 'idle' : 'offline', at: new Date().toISOString() } } : {},
    } });
  }, { exists, online });
}

async function buttonSnapshot(page) {
  return page.locator('.tabbar-actions').evaluate(el => [...el.querySelectorAll('button')].map(button => ({
    text: button.textContent.trim(), label: button.getAttribute('aria-label'), className: button.className, disabled: button.disabled,
  })));
}

async function headerBounds(page) {
  const bounds = await page.locator('.compact-topbar').evaluate(el => {
    const visible = [...el.querySelectorAll('button')].filter(button => button.getClientRects().length);
    return { viewport: innerWidth, page: document.documentElement.scrollWidth,
      left: Math.min(...visible.map(button => button.getBoundingClientRect().left)),
      right: Math.max(...visible.map(button => button.getBoundingClientRect().right)) };
  });
  assert.ok(bounds.page <= bounds.viewport && bounds.left >= -1 && bounds.right <= bounds.viewport + 1, JSON.stringify(bounds));
  return bounds;
}

async function outputState(page, expected, width) {
  await page.setViewportSize({ width, height: 900 });
  const toolbar = page.locator('.tabbar-actions');
  const primary = toolbar.locator('button.btn-primary').filter({ hasText: expected === 'send' ? /Send/ : /^Export/ });
  await primary.waitFor();
  assert.equal(await primary.count(), 1);
  assert.equal(await primary.isEnabled(), true);
  if (width >= 1200) {
    await toolbar.getByRole('button', { name: 'Send', exact: true }).waitFor();
    const labels = await toolbar.getByRole('button').allTextContents();
    assert.ok(labels.findIndex(label => /^Export/.test(label)) < labels.findIndex(label => label === 'Send'), JSON.stringify(labels));
    assert.equal(await toolbar.getByRole('button', { name: 'Send', exact: true }).count(), 1);
    assert.equal(await toolbar.locator('button.btn-primary').count(), 1);
  } else {
    const hiddenAction = toolbar.getByRole('button', { name: expected === 'send' ? /^Export/ : /^Send/ });
    await hiddenAction.waitFor({ state: 'hidden' });
    assert.equal(await hiddenAction.count(), 0);
    await headerBounds(page);
    await toolbar.getByRole('button', { name: 'More output actions', exact: true }).click();
    const alternate = page.getByRole('menuitem', { name: expected === 'send' ? /^Export/ : 'Send to printer', exact: expected !== 'send' });
    await alternate.waitFor();
    assert.equal(await alternate.isEnabled(), true);
    await page.screenshot({ path: path.join(output, `narrow-${width}-${expected}-menu.png`) });
    await page.keyboard.press('Escape');
  }
  const bounds = await headerBounds(page);
  await page.screenshot({ path: path.join(output, `${width >= 1200 ? 'wide' : 'narrow'}-${width}-${expected}.png`) });
  return { width, expected, labels: await toolbar.getByRole('button').allTextContents(), bounds };
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  try {
    const context = await browser.newContext({ viewport: { width: 1360, height: 900 } });
    await context.route('**/api/**', route => route.abort());
    await context.route(/\.gz(?:\?|$)/, async route => {
      const file = path.join(root, 'dist-demo/assets', new URL(route.request().url()).pathname.split('/').pop());
      if (fs.existsSync(file)) await route.fulfill({ body: fs.readFileSync(file), contentType: 'application/octet-stream' });
      else await route.continue();
    });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(base);
    await page.waitForFunction(() => window.__plater && window.__plater.getState().profileLibraryLoad.state === 'ready');
    const project = await page.evaluate(async () => (await (await fetch('/api/demo/sample')).json()).project);
    await page.goto(`${base.replace(/#.*$/, '')}#/p/${encodeURIComponent(project)}`);
    await page.waitForFunction(() => window.__plater?.getState().job.state === 'done');
    await page.evaluate(() => {
      const app = window.__plater;
      app.setState({ tab: 'prepare', job: { id: app.getState().job.id, state: 'idle', pct: 0, stage: '' } });
    });
    let dialog = await preferences(page);
    assert.equal(await dialog.getByRole('radio', { name: 'Export', exact: true }).getAttribute('aria-checked'), 'true');
    assert.deepEqual(await dialog.locator('h3.modal-group').allTextContents(), ['Camera', 'Defaults', 'Notifications', 'Advanced']);
    await page.screenshot({ path: path.join(output, 'preferences-defaults.png') });
    await closePreferences(dialog);
    await configured(page, true);
    const initial = {};
    for (const width of [1360, 1000, 800, 390]) {
      await page.setViewportSize({ width, height: 900 });
      await page.waitForTimeout(100);
      initial[width] = await buttonSnapshot(page);
    }
    await page.setViewportSize({ width: 1360, height: 900 });
    dialog = await preferences(page, 'Send to printer');
    await closePreferences(dialog);
    for (const width of [1360, 1000, 800, 390]) {
      await page.setViewportSize({ width, height: 900 });
      await page.waitForTimeout(100);
      assert.deepEqual(await buttonSnapshot(page), initial[width]);
    }
    await page.setViewportSize({ width: 1360, height: 900 });
    await configured(page, false);
    dialog = await preferences(page);
    const note = dialog.getByText(noteText, { exact: true });
    await note.waitFor();
    const noteLayout = await note.evaluate(el => {
      const row = el.previousElementSibling;
      const label = row.firstElementChild;
      const bounds = el.getBoundingClientRect(), rowBounds = row.getBoundingClientRect();
      return { left: bounds.left, labelLeft: label.getBoundingClientRect().left, gap: bounds.top - rowBounds.bottom,
        font: parseFloat(getComputedStyle(el).fontSize), labelFont: parseFloat(getComputedStyle(label).fontSize),
        color: getComputedStyle(el).color, labelColor: getComputedStyle(label).color };
    });
    assert.ok(Math.abs(noteLayout.left - noteLayout.labelLeft) < 1, JSON.stringify(noteLayout));
    assert.ok(Math.abs(noteLayout.gap - 4) < 1, JSON.stringify(noteLayout));
    assert.ok(noteLayout.font < noteLayout.labelFont && noteLayout.color !== noteLayout.labelColor, JSON.stringify(noteLayout));
    assert.equal(await dialog.getByRole('radio', { name: 'Send to printer', exact: true }).isEnabled(), true);
    await page.screenshot({ path: path.join(output, 'preferences-no-printer-helper.png') });
    await closePreferences(dialog);
    await page.waitForFunction(() => JSON.parse(sessionStorage.getItem('pancake.demo.v1') ?? '{}').preferences?.document?.['orca.web.afterSlice'] === 'send');
    await page.reload();
    await page.waitForFunction(() => window.__plater && window.__plater.getState().profileLibraryLoad.state === 'ready');
    dialog = await preferences(page);
    assert.equal(await dialog.getByRole('radio', { name: 'Send to printer', exact: true }).getAttribute('aria-checked'), 'true');
    await closePreferences(dialog);
    await page.waitForFunction(() => window.__plater.getState().job.state === 'done', null, { timeout: 30000 });
    const layouts = [];
    for (const width of [1360, 1000, 800, 390, 320]) layouts.push(await outputState(page, 'export', width));
    await configured(page, true, false);
    for (const width of [1360, 1000, 800, 390, 320]) layouts.push(await outputState(page, 'send', width));
    await configured(page, true, true);
    layouts.push(await outputState(page, 'send', 1360));
    dialog = await preferences(page, 'Export');
    assert.equal(await dialog.getByText(noteText, { exact: true }).count(), 0);
    await closePreferences(dialog);
    for (const width of [1360, 1000, 800, 390, 320]) layouts.push(await outputState(page, 'export', width));
    dialog = await preferences(page, 'Send to printer');
    assert.equal(await dialog.getByText(noteText, { exact: true }).count(), 0);
    await closePreferences(dialog);
    await page.waitForFunction(() => !document.activeElement?.closest('[role="dialog"]'));
    await page.keyboard.press('Control+g');
    await page.getByRole('alert').getByText('Not available in demo mode', { exact: true }).waitFor();
    assert.equal(await page.evaluate(() => window.__plater.getState().appDialog), null);
    await page.setViewportSize({ width: 1000, height: 900 });
    await page.locator('.tabbar-actions').getByRole('button', { name: 'Send to printer', exact: true }).click();
    await page.waitForFunction(() => window.__plater.getState().appDialog === 'send');
    await page.getByRole('dialog', { name: 'Send to printer', exact: true }).waitFor();
    await page.keyboard.press('Escape');
    await page.evaluate(() => { const s = window.__plater.getState(); window.__plater.setState({ job: { ...s.job, stale: true } }); });
    assert.equal(await page.locator('.tabbar-actions').getByRole('button', { name: 'Slice plate', exact: true }).getAttribute('class'), 'btn-primary slant');
    await page.setViewportSize({ width: 390, height: 900 });
    await page.waitForTimeout(100);
    assert.equal(await page.locator('.tabbar-actions').getByRole('button', { name: /^Send/ }).count(), 0);
    assert.equal(await page.locator('.tabbar-actions').getByRole('button', { name: 'Slice', exact: true }).getAttribute('class'), 'btn-primary slant');
    assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'verification.json'), JSON.stringify({ persistence: true, beforeSliceUnchanged: true, sendDialog: true, exportShortcut: true, staleSlice: true, noteLayout, layouts, errors }, null, 2));
    console.log(JSON.stringify({ persistence: true, beforeSliceUnchanged: true, sendDialog: true, exportShortcut: true, staleSlice: true, noteLayout, layouts: layouts.length, screenshots: fs.readdirSync(output).filter(name => name.endsWith('.png')), errors }));
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
