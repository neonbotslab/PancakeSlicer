const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const [base, output = '/tmp/overview-shots'] = process.argv.slice(2);
if (!base) throw new Error('Usage: node scripts/check-overview.cjs <demo-url> [output-directory]');
const root = path.resolve(__dirname, '..');
fs.mkdirSync(output, { recursive: true });
const mp4 = execFileSync('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=5', '-t', '120', '-an', '-c:v', 'libx264', '-preset', 'ultrafast', '-tune', 'zerolatency', '-g', '5', '-pix_fmt', 'yuv420p', '-movflags', 'frag_keyframe+empty_moov+default_base_moof', '-f', 'mp4', 'pipe:1'], { maxBuffer: 32 << 20 });
const jpegBytes = execFileSync('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=5', '-frames:v', '2', '-f', 'image2pipe', '-c:v', 'mjpeg', '-threads', '1', 'pipe:1']);
const jpegFrames = [];
for (let start = 0; start < jpegBytes.length;) {
  const end = jpegBytes.indexOf(Buffer.from([0xff, 0xd9]), start);
  assert.ok(end >= start);
  jpegFrames.push(jpegBytes.subarray(start, end + 2)); start = end + 2;
}
const fragments = [];
let init = Buffer.alloc(0), fragment = Buffer.alloc(0);
for (let offset = 0; offset < mp4.length;) {
  const size = mp4.readUInt32BE(offset), type = mp4.toString('ascii', offset + 4, offset + 8);
  assert.ok(size >= 8);
  const box = mp4.subarray(offset, offset + size);
  if (type === 'ftyp' || type === 'moov') init = Buffer.concat([init, box]);
  if (type === 'moof') fragment = box;
  if (type === 'mdat') fragments.push(Buffer.concat([fragment, box]));
  offset += size;
}
const active = new Map(), maximum = new Map(), received = new Map(), stalled = new Set();
const server = http.createServer((req, res) => {
  const match = /^\/api\/printer-connections\/([^/]+)\/camera(?:\/(\d+))?$/.exec(req.url);
  if (!match) { res.writeHead(404).end(); return; }
  const printer = decodeURIComponent(match[1]);
  if (!match[2]) {
    const cameras = printer === 'overview-echo' ? [] : [{ id: '0', name: 'Chamber camera', format: printer === 'overview-alpha' ? 'mp4' : 'mjpeg', url: `/api/printer-connections/${encodeURIComponent(printer)}/camera/0` }];
    res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify({ cameras })); return;
  }
  active.set(printer, (active.get(printer) ?? 0) + 1);
  maximum.set(printer, Math.max(maximum.get(printer) ?? 0, active.get(printer)));
  received.set(printer, (received.get(printer) ?? 0) + 1);
  const mjpeg = printer !== 'overview-alpha';
  res.writeHead(200, { 'Content-Type': mjpeg ? 'multipart/x-mixed-replace; boundary=fixture' : 'video/mp4', 'Cache-Control': 'no-store' });
  if (!mjpeg) res.write(init);
  let index = 0;
  const emit = () => {
    if (stalled.has(printer)) return;
    if (mjpeg) {
      const jpeg = jpegFrames[index++ % jpegFrames.length];
      res.write(Buffer.concat([Buffer.from(`--fixture\r\nContent-Type: image/jpeg\r\nContent-Length: ${jpeg.length}\r\n\r\n`), jpeg, Buffer.from('\r\n')]));
    } else if (index < fragments.length) res.write(fragments[index++]);
    else res.end();
  };
  emit();
  const timer = setInterval(emit, mjpeg ? 200 : 1000);
  res.on('close', () => { clearInterval(timer); active.set(printer, active.get(printer) - 1); });
});

async function fixture(page) {
  await page.waitForFunction(() => window.__plater?.getState().profileLibraryLoad.state === 'ready');
  return page.evaluate(() => {
    const app = window.__plater, s = app.getState();
    const template = s.records.printers.find(p => p.id === s.printer) ?? s.records.printers[0];
    if (!template) throw new Error('Demo printer profile is missing');
    const names = ['Alpha', 'Bravo', 'Charlie', 'Delta', 'Echo'], models = ['H2D', 'P1S', 'A1 mini', 'A1', 'H2D'];
    const keys = names.map(name => `overview-${name.toLowerCase()}`);
    const profiles = names.map((name, i) => ({ ...structuredClone(template), id: keys[i], name, model: models[i],
      slots: template.slots.map((slot, j) => ({ ...slot, id: `${keys[i]}:${j}` })) }));
    const connections = names.map((name, i) => ({ printer: keys[i], id: keys[i], name, model: models[i], kind: 'bambu-lan', has_secret: true }));
    const now = new Date().toISOString();
    const statuses = ['printing', 'paused', 'idle', 'offline', 'idle'].map((state, i) => ({ printer: keys[i], state, link: state === 'offline' ? 'retrying' : 'online', online: state !== 'offline', at: now, last_seen: new Date(2026, 10, 19, 15, 12).toISOString(),
      ...(i < 2 ? { file: i ? 'Hinge bracket.gcode.3mf' : 'Benchy prototype.gcode.3mf', progress: i ? 0.38 : 0.62, remaining_s: 4440 } : {}),
      ...(i === 1 ? { messages: [{ code: '07002200-00020001', message: 'raw message is intentionally ignored' }, { code: '12038005' }] } : {}) }));
    const original = window.fetch;
    window.fetch = (input, options) => {
      const url = new URL(input instanceof Request ? input.url : String(input), location.href);
      if (/^\/api\/printer-connections\/[^/]+\/camera(?:\/\d+)?$/.test(url.pathname)) return window.overviewNativeFetch(input, options);
      if (/^\/api\/printer-sends/.test(url.pathname)) return Promise.resolve(new Response(JSON.stringify({ sends: [], total: 0 }), { headers: { 'Content-Type': 'application/json' } }));
      return original(input, options);
    };
    app.setState({ records: { ...s.records, printers: [...s.records.printers, ...profiles] }, printerConnections: { enabled: true, live: true, transfers: [], connections, status: Object.fromEntries(statuses.map(status => [status.printer, status])) } });
    return { keys, document: JSON.stringify({ project: s.project, projectName: s.projectName, objects: s.objects }) };
  });
}

(async () => {
  await new Promise(resolve => server.listen(20126, '127.0.0.1', resolve));
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  try {
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
    await context.route('**/*', async route => {
      const url = new URL(route.request().url());
      if (!['127.0.0.1', 'localhost'].includes(url.hostname)) { await route.abort(); return; }
      if (/\.gz$/.test(url.pathname)) {
        const file = path.join(root, 'dist-demo/assets', path.basename(url.pathname));
        if (fs.existsSync(file)) { await route.fulfill({ body: fs.readFileSync(file), contentType: 'application/octet-stream' }); return; }
      }
      await route.continue();
    });
    await context.addInitScript(() => {
      window.overviewNativeFetch = window.fetch.bind(window);
      window.overviewHidden = false;
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => window.overviewHidden ? 'hidden' : 'visible' });
      Object.defineProperty(document, 'hidden', { configurable: true, get: () => window.overviewHidden });
    });
    const page = await context.newPage(), errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(base);
    const seeded = await fixture(page);
    await page.getByRole('button', { name: 'Device', exact: true }).click();
    await page.getByRole('heading', { name: 'All printers', exact: true }).waitFor();
    const cards = page.locator('.printer-overview-card');
    await page.waitForFunction(() => [...document.querySelectorAll('.overview-camera video')].filter(video => video.videoWidth > 0 && video.currentTime > 0).length === 1
      && [...document.querySelectorAll('.overview-camera img')].filter(image => image.naturalWidth > 0).length === 2);
    assert.equal(await cards.count(), 5);
    assert.equal(await page.getByRole('button', { name: 'All printers', exact: true }).count(), 0);
    await page.screenshot({ path: path.join(output, 'wide.png') });
    const geometry = await cards.evaluateAll(elements => elements.map(el => { const r = el.getBoundingClientRect(); return { width: r.width, height: r.height, text: el.textContent }; }));
    assert.ok(geometry.every(card => card.width >= 319));
    assert.ok(Math.max(...geometry.map(card => card.height)) - Math.min(...geometry.map(card => card.height)) < 1);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: path.join(output, 'phone.png') });
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    const mobile = await cards.first().evaluate(el => { const card = el.getBoundingClientRect(), video = el.querySelector('.overview-camera').getBoundingClientRect(); return { width: video.width, height: card.height }; });
    assert.ok(mobile.width >= 105 && mobile.width <= 120, JSON.stringify(mobile));
    await page.setViewportSize({ width: 390, height: 440 });
    const scroll = page.locator('.printer-overview');
    await scroll.evaluate(el => { el.scrollTop = el.scrollHeight; });
    const scrollBefore = await scroll.evaluate(el => el.scrollTop);
    assert.ok(scrollBefore > 0, 'Fixture must exercise a scrolled overview');
    await cards.last().click();
    await page.getByRole('link', { name: '← All printers', exact: true }).waitFor();
    assert.ok(page.url().endsWith('/device/overview-echo'), page.url());
    await page.goBack();
    await page.getByRole('heading', { name: 'All printers', exact: true }).waitFor();
    assert.equal(await scroll.evaluate(el => el.scrollTop), scrollBefore);
    await cards.last().click();
    await page.getByRole('link', { name: '← All printers', exact: true }).click();
    await page.getByRole('heading', { name: 'All printers', exact: true }).waitFor();
    assert.equal(await scroll.evaluate(el => el.scrollTop), scrollBefore);
    await cards.last().click();
    await page.locator('.device-printer[href$="/device/overview-bravo"]').click();
    await page.getByRole('link', { name: '← All printers', exact: true }).click();
    await page.getByRole('heading', { name: 'All printers', exact: true }).waitFor();
    assert.equal(await scroll.evaluate(el => el.scrollTop), scrollBefore);
    await page.setViewportSize({ width: 1600, height: 1000 });
    await cards.first().click();
    await page.getByRole('link', { name: '← All printers', exact: true }).waitFor();
    await page.waitForFunction(() => document.querySelector('.device-camera video')?.currentTime > 0);
    await page.mouse.move(1590, 990);
    await page.screenshot({ path: path.join(output, 'single.png') });
    await page.getByRole('link', { name: '← All printers', exact: true }).click();
    await page.getByRole('heading', { name: 'All printers', exact: true }).waitFor();
    await page.waitForFunction(() => [...document.querySelectorAll('.overview-camera video')].filter(video => video.currentTime > 0).length === 1);
    const direct = await context.newPage();
    await direct.goto(`${base.replace(/#.*$/, '')}#/device/overview-alpha`);
    await fixture(direct);
    await direct.getByRole('link', { name: '← All printers', exact: true }).waitFor();
    await direct.waitForFunction(() => document.querySelector('.device-camera video')?.currentTime > 0);
    assert.ok(direct.url().endsWith('/device/overview-alpha'));
    await direct.close();
    await expectEventually(() => [...active.values()].reduce((a, b) => a + b, 0) === 3);
    const counts = Object.fromEntries(received);
    await page.evaluate(() => { window.overviewHidden = true; document.dispatchEvent(new Event('visibilitychange')); });
    await expectEventually(() => [...active.values()].every(n => n === 0));
    await page.waitForTimeout(2300);
    assert.deepEqual(Object.fromEntries(received), counts);
    await page.evaluate(() => { window.overviewHidden = false; document.dispatchEvent(new Event('visibilitychange')); });
    await expectEventually(() => [...active.values()].reduce((a, b) => a + b, 0) === 3);
    stalled.add('overview-alpha');
    await page.locator('.overview-camera').first().getByText('reconnecting', { exact: true }).waitFor({ timeout: 16000 });
    stalled.clear();
    await page.waitForFunction(() => document.querySelector('.overview-camera video')?.currentTime > 0);
    await page.getByRole('button', { name: 'Prepare', exact: true }).click();
    await expectEventually(() => [...active.values()].every(n => n === 0));
    assert.equal(await page.evaluate(() => { const s = window.__plater.getState(); return JSON.stringify({ project: s.project, projectName: s.projectName, objects: s.objects }); }), seeded.document);
    assert.deepEqual(errors, []);
    const evidence = { screenshots: ['wide.png', 'phone.png', 'single.png'], geometry, mobile, scrollBefore, browserStreamRequests: Object.fromEntries(received), browserMaximumViewers: Object.fromEntries(maximum), errors };
    fs.writeFileSync(path.join(output, 'evidence.json'), JSON.stringify(evidence, null, 2));
    console.log(JSON.stringify(evidence));
    await context.close();
  } finally { await browser.close(); server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
})().catch(error => { console.error(error); server.closeAllConnections(); server.close(); process.exitCode = 1; });

async function expectEventually(predicate) {
  const until = Date.now() + 10000;
  while (!predicate()) { assert.ok(Date.now() < until, 'Camera connection counts did not settle'); await new Promise(resolve => setTimeout(resolve, 50)); }
}
