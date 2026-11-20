const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const [base, output] = process.argv.slice(2);
if (!base || !output) throw new Error('Usage: npm run test:browser -- <local-3DBenchy-project-url> <output-directory>');
const origin = new URL(base).origin;
if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(base).hostname)) throw new Error('This check needs a local PancakeSlicer service.');
fs.mkdirSync(output, { recursive: true });
(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  const context = await browser.newContext({ viewport: { width: 1360, height: 900 } });
  await context.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
  await context.addInitScript(() => {
    window.notifyPermission = 'default';
    window.notifyHidden = false;
    window.notifyPrompts = [];
    Object.defineProperty(Notification, 'permission', { configurable: true, get: () => window.notifyPermission });
    Notification.requestPermission = () => { window.notifyPrompts.push(navigator.userActivation.isActive); window.notifyPermission = 'granted'; return Promise.resolve('granted'); };
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => window.notifyHidden });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => window.notifyHidden ? 'hidden' : 'visible' });
  });
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(base);
  await page.waitForFunction(() => window.__plater?.getState().objects.some((object) => /3DBenchy/i.test(object.name)));
  assert.deepEqual(await page.evaluate(() => window.notifyPrompts), []);
  await page.getByRole('button', { name: 'Slice plate', exact: true }).click();
  await page.evaluate(() => { window.notifyHidden = true; document.dispatchEvent(new Event('visibilitychange')); });
  await page.getByText('Get notified when slicing finishes in another tab?', { exact: true }).waitFor({ timeout: 30000 });
  const fixture = await page.evaluate(() => window.__plater.getState().job.status);
  assert.equal(fixture.state, 'done');
  await page.evaluate(() => { window.notifyHidden = false; document.dispatchEvent(new Event('visibilitychange')); });
  const readyCard = await page.locator('.notification-offer').evaluateHandle(offer => offer.closest('.notification'));
  const toastLayout = await page.locator('.notification-offer').evaluate(offer => {
    const toast = offer.closest('.notification').getBoundingClientRect();
    const canvas = document.querySelector('.canvas-area').getBoundingClientRect();
    const range = document.createRange();
    range.selectNodeContents(offer.firstElementChild);
    return { centerOffset: toast.x + toast.width / 2 - canvas.x - canvas.width / 2, top: toast.top - canvas.top,
      questionLines: new Set([...range.getClientRects()].filter(rect => rect.width > 0).map(rect => Math.round(rect.top))).size };
  });
  assert.ok(Math.abs(toastLayout.centerOffset) < 2, JSON.stringify(toastLayout));
  assert.ok(toastLayout.top >= 0 && toastLayout.top < 130, JSON.stringify(toastLayout));
  assert.equal(toastLayout.questionLines, 1);
  await page.screenshot({ path: path.join(output, 'toast-permission.png') });
  await page.getByRole('button', { name: 'Turn on', exact: true }).click();
  assert.deepEqual(await page.evaluate(() => window.notifyPrompts), [true]);
  await page.waitForFunction(() => !document.querySelector('.notification-offer'));
  const readyLayout = await readyCard.evaluate(card => {
    const bounds = card.getBoundingClientRect();
    const canvas = document.querySelector('.canvas-area').getBoundingClientRect();
    return { sameCard: card.isConnected, centerOffset: bounds.x + bounds.width / 2 - canvas.x - canvas.width / 2 };
  });
  assert.equal(readyLayout.sameCard, true);
  assert.ok(Math.abs(readyLayout.centerOffset) < 2, JSON.stringify(readyLayout));
  await page.evaluate(() => window.__plater.setState({ appDialog: 'preferences' }));
  const setting = page.getByRole('checkbox', { name: 'Notify me when PancakeSlicer is in the background' });
  await setting.waitFor();
  const zoom = page.getByRole('checkbox', { name: 'Zoom to mouse position' });
  assert.equal((await setting.boundingBox()).x, (await zoom.boundingBox()).x);
  await page.screenshot({ path: path.join(output, 'settings-normal.png') });
  assert.equal(await setting.getAttribute('aria-checked'), 'true');
  await page.evaluate(() => { window.notifyPermission = 'denied'; window.dispatchEvent(new Event('focus')); });
  await page.getByText('Blocked by your browser. Allow notifications for this site from the address bar.').waitFor();
  assert.equal(await setting.isDisabled(), true);
  const blockedLayout = await checkDisabledSetting(page, setting);
  await page.screenshot({ path: path.join(output, 'settings-blocked.png') });
  await page.evaluate(() => { Object.defineProperty(window, 'isSecureContext', { configurable: true, value: false }); window.dispatchEvent(new Event('focus')); });
  await page.getByText('Only works on an https address.').waitFor();
  const insecureLayout = await checkDisabledSetting(page, setting);
  await page.screenshot({ path: path.join(output, 'settings-insecure.png') });
  assert.equal(await setting.isDisabled(), true);
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ screenshots: fs.readdirSync(output), permissionRequests: await page.evaluate(() => window.notifyPrompts), toastLayout, readyLayout, blockedLayout, insecureLayout, errors }));
  await checkLifecycle(browser, fixture);
  await browser.close();
})().catch(error => { console.error(error); process.exit(1); });

async function checkLifecycle(browser, fixture) {
  const context = await browser.newContext({ viewport: { width: 1360, height: 900 } });
  await context.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
  await context.addInitScript(() => {
    window.notifyHidden = false;
    window.notifyMessages = [];
    localStorage.setItem('orca.notifications.enabled', '1');
    class TestNotification {
      static permission = 'granted';
      static requestPermission() { throw new Error('Unexpected permission prompt'); }
      constructor(title, options) { this.title = title; this.options = options; window.notifyMessages.push(this); }
      close() { this.closed = true; }
    }
    window.Notification = TestNotification;
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => window.notifyHidden });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => window.notifyHidden ? 'hidden' : 'visible' });
  });
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(base);
  await page.waitForFunction(() => window.__plater?.getState().objects.some((object) => /3DBenchy/i.test(object.name)));
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent.trim() === 'Slice plate' && !button.disabled));
  await page.evaluate((sourceJob) => {
    const original = window.__plater.getState().api;
    let serial = 0;
    window.testStatus = { id: 'fixture-job', state: 'slicing' };
    window.__plater.setState({ api: {
      ...original,
      mock: true,
      submit: async () => `fixture-job-${++serial}`,
      events: (id, handlers) => { window.testHandlers = handlers; window.testStatus = { id, state: 'slicing' }; return () => {}; },
      status: async () => window.testStatus,
      movesUrl: () => original.movesUrl(sourceJob),
      cancel: async () => {},
    } });
  }, fixture.id);
  await page.getByRole('button', { name: 'Slice plate', exact: true }).click();
  await page.waitForFunction(() => !!window.testHandlers);
  await page.evaluate(f => window.testHandlers.result({ ...f, id: window.testStatus.id, state: 'slicing' }), fixture);
  await page.waitForTimeout(9000);
  assert.equal(await page.evaluate(() => window.notifyMessages.length), 0);
  await page.evaluate(f => {
    window.notifyHidden = true;
    document.dispatchEvent(new Event('visibilitychange'));
    window.testStatus = { ...f, id: window.testStatus.id, state: 'done' };
    window.testHandlers.done(window.testStatus);
  }, fixture);
  await page.waitForFunction(() => window.notifyMessages.length === 1);
  assert.equal(await page.evaluate(() => window.notifyMessages[0].title), 'Slice ready');
  assert.match(await page.evaluate(() => window.notifyMessages[0].options.body), /3DBenchy.* · .* · .* g$/);
  await page.waitForTimeout(8500);
  assert.ok(await page.locator('.notification.done').count() > 0);
  await page.evaluate(() => {
    window.notifyHidden = false;
    document.dispatchEvent(new Event('visibilitychange'));
    window.__plater.setState({ tab: 'prepare' });
    window.notifyMessages[0].onclick();
  });
  await page.waitForFunction(() => window.__plater.getState().tab === 'preview');
  await page.evaluate(() => { window.testHandlers = null; window.__plater.setState({ job: { state: 'idle', pct: 0, stage: '' }, tab: 'prepare' }); });
  await page.getByRole('button', { name: 'Slice plate', exact: true }).click();
  await page.waitForFunction(() => !!window.testHandlers);
  await page.evaluate(() => {
    window.notifyHidden = true;
    document.dispatchEvent(new Event('visibilitychange'));
    window.testStatus = { id: window.testStatus.id, state: 'failed', error: 'bad_alloc\nstack trace detail' };
    window.testHandlers.error(window.testStatus.error, 'slice_failed', window.testStatus);
  });
  await page.waitForFunction(() => window.notifyMessages.length === 2);
  assert.equal(await page.evaluate(() => window.notifyMessages[1].title), 'Slice failed');
  assert.doesNotMatch(await page.evaluate(() => window.notifyMessages[1].options.body), /bad_alloc|stack trace/);
  await page.evaluate(() => { window.notifyHidden = false; document.dispatchEvent(new Event('visibilitychange')); window.notifyMessages[1].onclick(); });
  await page.waitForFunction(() => !!document.querySelector('.slice-failed details')?.open);
  await page.evaluate(() => {
    const current = window.__plater.getState().job;
    window.__plater.setState({ job: { ...current, id: 'newer-completed-job', state: 'done', pct: 100, stage: 'done' }, notificationError: null });
    window.notifyMessages[1].onclick();
  });
  await page.waitForFunction(() => !!window.__plater.getState().notificationError);
  assert.equal(await page.evaluate(() => window.__plater.getState().job.id), 'newer-completed-job');
  await page.evaluate(() => window.notifyMessages[0].onclick());
  await page.waitForFunction(() => window.__plater.getState().tab === 'preview');
  assert.equal(await page.evaluate(() => window.__plater.getState().job.id), 'newer-completed-job');
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ outcomes: await page.evaluate(() => window.notifyMessages.map(n => ({ title: n.title, body: n.options.body, tag: n.options.tag }))), earlyResultSilent: true, hiddenToastRetained: true, previewClick: true, failureDetailsClick: true, staleClicksPreserveNewerJob: true, errors }));
  await context.close();
}

async function checkDisabledSetting(page, setting) {
  const metrics = await setting.evaluate(control => {
    const row = control.closest('label');
    const note = document.getElementById(control.getAttribute('aria-describedby'));
    const text = row.querySelector('span:not([data-state])');
    const range = document.createRange();
    if (text) range.selectNodeContents(text);
    else range.selectNodeContents([...row.childNodes].find(node => node.nodeType === Node.TEXT_NODE && node.textContent.trim()));
    const labelBounds = range.getBoundingClientRect();
    const noteBounds = note.getBoundingClientRect();
    let opacity = 1;
    for (let node = control; node; node = node.parentElement) opacity *= Number(getComputedStyle(node).opacity);
    return { labelCursor: getComputedStyle(row).cursor, controlCursor: getComputedStyle(control).cursor,
      opacity, helperIndent: noteBounds.x - labelBounds.x, helperGap: noteBounds.top - row.getBoundingClientRect().bottom,
      helperSize: Number.parseFloat(getComputedStyle(note).fontSize), labelSize: Number.parseFloat(getComputedStyle(row).fontSize),
      helperColor: getComputedStyle(note).color, labelColor: getComputedStyle(row).color,
      labelAlpha: Number(getComputedStyle(row).color.match(/\/\s*([\d.]+)/)?.[1] ?? 1) };
  });
  assert.equal(metrics.labelCursor, 'not-allowed');
  assert.equal(metrics.controlCursor, 'not-allowed');
  assert.ok(metrics.labelAlpha >= 0.45 && metrics.labelAlpha <= 0.55, JSON.stringify(metrics));
  assert.ok(metrics.opacity >= 0.45 && metrics.opacity <= 0.55, JSON.stringify(metrics));
  assert.ok(Math.abs(metrics.helperIndent) < 2, JSON.stringify(metrics));
  assert.ok(metrics.helperGap >= 3 && metrics.helperGap <= 5, JSON.stringify(metrics));
  assert.ok(metrics.helperSize < metrics.labelSize, JSON.stringify(metrics));
  assert.notEqual(metrics.helperColor, metrics.labelColor);
  const appearance = () => setting.evaluate(control => [control, control.closest('label')].map(element => {
    const style = getComputedStyle(element);
    return [style.backgroundColor, style.borderColor, style.color, style.opacity, style.boxShadow, style.transform];
  }));
  const before = await appearance();
  await setting.hover({ force: true });
  assert.deepEqual(await appearance(), before);
  await page.mouse.down();
  assert.deepEqual(await appearance(), before);
  await page.mouse.up();
  assert.equal(await setting.getAttribute('aria-checked'), 'false');
  return metrics;
}
