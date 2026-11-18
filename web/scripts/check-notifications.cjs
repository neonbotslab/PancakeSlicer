const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const root = path.resolve(__dirname, '..');
const [base, output] = process.argv.slice(2);
if (!base || !output) throw new Error('Usage: npm run test:browser -- <demo-url> <output-directory>');
fs.mkdirSync(output, { recursive: true });
(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  const context = await browser.newContext({ viewport: { width: 1360, height: 900 } });
  await context.route('**/api/**', route => route.abort());
  await context.route(/\.gz(?:\?|$)/, async route => {
    const name = new URL(route.request().url()).pathname.split('/').pop();
    const file = path.join(root, 'dist-demo/assets', name);
    if (fs.existsSync(file)) await route.fulfill({ body: fs.readFileSync(file), contentType: 'application/octet-stream' });
    else await route.continue();
  });
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
  await page.waitForFunction(() => window.__plater && window.__plater.getState().profileLibraryLoad.state === 'ready');
  assert.deepEqual(await page.evaluate(() => window.notifyPrompts), []);
  await page.getByRole('button', { name: 'Slice plate', exact: true }).click();
  await page.evaluate(() => { window.notifyHidden = true; document.dispatchEvent(new Event('visibilitychange')); });
  await page.getByText('Get notified when slicing finishes in another tab?', { exact: true }).waitFor({ timeout: 30000 });
  await page.evaluate(() => { window.notifyHidden = false; document.dispatchEvent(new Event('visibilitychange')); });
  await page.screenshot({ path: path.join(output, 'toast-permission.png') });
  await page.getByRole('button', { name: 'Turn on', exact: true }).click();
  assert.deepEqual(await page.evaluate(() => window.notifyPrompts), [true]);
  await page.evaluate(() => window.__plater.setState({ appDialog: 'preferences' }));
  const setting = page.getByRole('switch', { name: 'Notify me when PancakeSlicer is in the background' });
  await setting.waitFor();
  await page.screenshot({ path: path.join(output, 'settings-normal.png') });
  assert.equal(await setting.getAttribute('aria-checked'), 'true');
  await page.evaluate(() => { window.notifyPermission = 'denied'; window.dispatchEvent(new Event('focus')); });
  await page.getByText('Blocked by your browser. Allow notifications for this site from the address bar.').waitFor();
  assert.equal(await setting.isDisabled(), true);
  await page.screenshot({ path: path.join(output, 'settings-blocked.png') });
  await page.evaluate(() => { Object.defineProperty(window, 'isSecureContext', { configurable: true, value: false }); window.dispatchEvent(new Event('focus')); });
  await page.getByText('Needs a secure (https) address.').waitFor();
  await page.screenshot({ path: path.join(output, 'settings-insecure.png') });
  assert.equal(await setting.isDisabled(), true);
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ screenshots: fs.readdirSync(output), permissionRequests: await page.evaluate(() => window.notifyPrompts), errors }));
  await checkLifecycle(browser);
  await browser.close();
})().catch(error => { console.error(error); process.exit(1); });

async function checkLifecycle(browser) {
  const fixture = JSON.parse(require('node:zlib').gunzipSync(fs.readFileSync(path.join(root, 'src/demo/fixtures/benchy-flat-020.status.json.gz'))));
  const context = await browser.newContext({ viewport: { width: 1360, height: 900 } });
  await context.route('**/api/**', route => route.abort());
  await context.route(/\.gz(?:\?|$)/, async route => {
    const file = path.join(root, 'dist-demo/assets', new URL(route.request().url()).pathname.split('/').pop());
    if (fs.existsSync(file)) await route.fulfill({ body: fs.readFileSync(file), contentType: 'application/octet-stream' });
    else await route.continue();
  });
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
  await page.waitForFunction(() => window.__plater && window.__plater.getState().profileLibraryLoad.state === 'ready');
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent.trim() === 'Slice plate' && !button.disabled));
  await page.evaluate(() => {
    const original = window.__plater.getState().api;
    let serial = 0;
    window.testStatus = { id: 'fixture-job', state: 'slicing' };
    window.__plater.setState({ api: {
      ...original,
      submit: async () => `fixture-job-${++serial}`,
      events: (id, handlers) => { window.testHandlers = handlers; window.testStatus = { id, state: 'slicing' }; return () => {}; },
      status: async () => window.testStatus,
      movesUrl: () => original.movesUrl('demo-benchy-flat-020-fixture'),
      cancel: async () => {},
    } });
  });
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
    window.testHandlers.error(window.testStatus.error, window.testStatus);
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
