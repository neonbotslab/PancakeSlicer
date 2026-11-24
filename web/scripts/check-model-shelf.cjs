const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { zipSync, strToU8 } = require('fflate');

const [base, output] = process.argv.slice(2);
if (!base || !output) throw new Error('Usage: node web/scripts/check-model-shelf.cjs <fixture-dev-url> <screenshot-directory>');
const origin = new URL(base).origin;
if (!['127.0.0.1', 'localhost'].includes(new URL(base).hostname)) throw new Error('Use a local server with temporary fixture data and printer networking disabled.');
fs.mkdirSync(output, { recursive: true });
const cube = fs.readFileSync(path.resolve(__dirname, '../../engine/tests/data/test_stl/ASCII/20mmbox-LF.stl'));
const modelFile = (name) => ({ name, mimeType: 'model/stl', buffer: cube });
const png = (name) => path.join(output, `${name}.png`);
const models = (page) => page.locator('.models-page');
const tile = (page, name) => models(page).locator('.shelf-tile').filter({ has: page.getByRole('button', { name, exact: true }) });
const action = (page, name) => page.getByRole('button', { name: `Actions for ${name}`, exact: true });
const snapshot = async (page) => page.evaluate(async () => (await fetch('/api/shelf?tree=1')).json());
const objectCount = (page) => page.evaluate(() => window.__plater.getState().objects.length);
const itemCount = async (page) => Object.keys((await snapshot(page)).models).length;
async function shot(page, name) {
  const before = page.viewportSize();
  const height = ['populated-menu', 'added-check-toast'].includes(name) ? 900 : name.startsWith('empty-') ? 640 : 720;
  await page.setViewportSize({ width: 1440, height });
  await page.screenshot({ path: png(name), animations: 'disabled' });
  await page.setViewportSize(before);
}

async function ready(page) {
  await page.goto(base);
  await page.waitForFunction(() => window.__plater && window.__plater.getState().profileLibraryLoad.state === 'ready');
  await page.evaluate(async () => {
    const source = performance.getEntriesByType('resource').findLast((entry) => new URL(entry.name).pathname === '/src/flow/project.ts');
    if (!source) throw new Error('Project startup module was not loaded.');
    await (await import(source.name)).startProjects();
  });
  const closeSetup = page.getByRole('button', { name: 'Close setup', exact: true });
  if (await closeSetup.count()) await closeSetup.click();
  await page.getByRole('button', { name: 'Models', exact: true }).click();
}

async function newFolder(page, name) {
  await page.getByRole('button', { name: 'New folder', exact: true }).click();
  const input = models(page).getByRole('textbox', { name: 'Folder name', exact: true });
  await input.fill(name);
  await input.press('Enter');
  await models(page).getByRole('button', { name, exact: true }).waitFor();
}

async function upload(page, files) {
  await models(page).locator('input[type=file]').setInputFiles(files);
  for (const file of Array.isArray(files) ? files : [files]) {
    const name = file.name.replace(/\.(stl|obj|3mf|step|stp|zip)$/i, '');
    await models(page).getByRole('button', { name, exact: true }).waitFor({ timeout: 120000 });
  }
}

async function add(page, name) {
  const count = await objectCount(page);
  await action(page, name).click();
  await page.getByRole('menuitem', { name: 'Add to Plate', exact: true }).click();
  await page.waitForFunction((count) => window.__plater.getState().objects.length === count + 1, count, { timeout: 120000 });
  await page.getByText(`Added ${name} to plate`, { exact: true }).waitFor();
  assert.equal(await page.evaluate(() => window.__plater.getState().tab), 'models');
}

async function dragFile(page, target, name, event = 'drop') {
  const transfer = await page.evaluateHandle(({ name, bytes }) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File([Uint8Array.from(atob(bytes), (c) => c.charCodeAt(0))], name, { type: 'model/stl' }));
    return transfer;
  }, { name, bytes: cube.toString('base64') });
  await target.dispatchEvent('dragenter', { dataTransfer: transfer });
  if (event === 'dragenter') return transfer;
  await target.dispatchEvent('dragover', { dataTransfer: transfer });
  await target.dispatchEvent('drop', { dataTransfer: transfer });
  await transfer.dispose();
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, serviceWorkers: 'block' });
  const errors = [];
  context.on('page', (page) => page.on('pageerror', (error) => errors.push(error.message)));
  try {
    const page = await context.newPage();
    await ready(page);
    await page.getByRole('button', { name: 'File', exact: true }).click();
    await page.getByRole('menuitem', { name: /^New project/ }).click();
    await page.getByRole('button', { name: 'Models', exact: true }).click();
    await models(page).getByText('Drop models or a zip here', { exact: true }).waitFor();
    const empty = await snapshot(page);
    assert.equal(Object.keys(empty.models).length + Object.keys(empty.folders).length, 0, 'Start with an empty shelf in a temporary fixture directory.');
    await shot(page, 'empty-shelf');
    await newFolder(page, 'Calibration');
    await models(page).getByRole('button', { name: 'Calibration', exact: true }).click();
    await shot(page, 'empty-folder');
    await newFolder(page, 'Towers');
    await tile(page, 'Towers').getByRole('button', { name: 'Towers', exact: true }).focus();
    await page.keyboard.press('Enter');
    await newFolder(page, 'Posts');
    await newFolder(page, 'Tolerance');
    await upload(page, [modelFile('Benchy.stl'), modelFile('Cube.stl'), modelFile('Retract.stl'), modelFile('Bridge.stl'), modelFile('Temp tower.stl')]);
    await upload(page, modelFile('Cube.stl'));
    await models(page).getByRole('button', { name: 'Cube 2', exact: true }).waitFor();
    const beforeClick = await objectCount(page);
    const cubeTile = tile(page, 'Cube');
    await cubeTile.getByRole('button', { name: 'Cube', exact: true }).click();
    assert.equal(await objectCount(page), beforeClick, 'A single model click only selects.');
    assert.equal(await cubeTile.getByRole('button', { name: 'Cube', exact: true }).getAttribute('aria-pressed'), 'true');
    await action(page, 'Cube').focus();
    assert.equal(await action(page, 'Cube').evaluate((element) => getComputedStyle(element).opacity), '1');
    await cubeTile.click({ button: 'right', position: { x: 10, y: 50 } });
    const menu = page.getByRole('menu', { name: 'Actions for Cube', exact: true });
    await menu.waitFor();
    const buttonBox = await action(page, 'Cube').boundingBox(), menuBox = await menu.boundingBox();
    assert.ok(Math.abs(menuBox.y - buttonBox.y - buttonBox.height - 2) <= 1, 'Right-click menu stays below its overflow button.');
    assert.ok(Math.abs(menuBox.x + menuBox.width - buttonBox.x - buttonBox.width) <= 1, 'Menu right edge aligns with its overflow button.');
    await shot(page, 'populated-menu');
    await page.keyboard.press('End');
    assert.equal(await page.evaluate(() => document.activeElement.textContent), 'Delete');
    await page.keyboard.press('Escape');
    assert.equal(await action(page, 'Cube').evaluate((element) => element === document.activeElement), true);
    await add(page, 'Benchy');
    const pulse = await tile(page, 'Benchy').evaluate((element) => {
      const style = getComputedStyle(element.querySelector('.shelf-added-outline'));
      return { duration: style.animationDuration, animation: style.animationName, background: style.backgroundColor, opacity: Number(style.opacity) };
    });
    assert.equal(pulse.duration, '0.4s');
    assert.equal(pulse.animation, 'shelf-added-fade');
    assert.equal(pulse.background, 'rgba(0, 0, 0, 0)', 'Add feedback has no green wash.');
    assert.ok(pulse.opacity > 0, 'Add briefly pulses an outline.');
    assert.equal(await models(page).locator('.shelf-check').count(), 0, 'Add has no check badge.');
    await page.screenshot({ path: png('added-outline-pulse'), animations: 'allow' });
    await page.waitForFunction(() => !document.querySelector('.shelf-added'));
    await shot(page, 'added-toast-after-pulse');
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await add(page, 'Benchy');
    assert.equal(await tile(page, 'Benchy').evaluate((element) => getComputedStyle(element.querySelector('.shelf-added-outline')).animationName), 'none');
    assert.equal(await tile(page, 'Benchy').evaluate((element) => getComputedStyle(element.querySelector('.shelf-added-outline')).opacity), '0');
    await shot(page, 'added-reduced-motion');
    await page.emulateMedia({ reducedMotion: 'no-preference' });
    await add(page, 'Benchy');
    const earlierOutline = await tile(page, 'Benchy').locator('.shelf-added-outline').elementHandle();
    await add(page, 'Benchy');
    assert.equal(await tile(page, 'Benchy').locator('.shelf-added-outline').evaluate((element, earlier) => element !== earlier, earlierOutline), true,
      'Successive adds create fresh outline animations.');
    const copies = await page.evaluate(() => {
      const objects = window.__plater.getState().objects;
      const [a, b] = objects.slice(-2);
      return { differentObjects: a.id !== b.id, differentInstances: a.instances[0].id !== b.instances[0].id,
        differentFiles: a.source !== b.source, dx: Math.abs(a.instances[0].x - b.instances[0].x), dy: Math.abs(a.instances[0].y - b.instances[0].y) };
    });
    assert.ok(copies.differentObjects && copies.differentInstances && copies.differentFiles, 'Each Add makes an independent copy.');
    assert.ok(copies.dx >= 20 || copies.dy >= 20, 'Repeated adds never overlap.');
    await page.getByRole('button', { name: 'View', exact: true }).click();
    assert.equal(await page.evaluate(() => window.__plater.getState().tab), 'prepare');
    await page.getByRole('button', { name: 'Models', exact: true }).click();
    await cubeTile.getByRole('button', { name: 'Cube', exact: true }).focus();
    await page.keyboard.press('F2');
    const rename = models(page).getByRole('textbox', { name: 'Model name', exact: true });
    await rename.fill('Calibration cube');
    await rename.press('Enter');
    await models(page).getByRole('button', { name: 'Calibration cube', exact: true }).waitFor();
    const selected = Object.values((await snapshot(page)).models).find((model) => model.name === 'Calibration cube.stl');
    await action(page, 'Calibration cube').click();
    await page.getByRole('menuitem', { name: 'Move to…', exact: true }).click();
    const moveDialog = page.getByRole('dialog', { name: 'Move Calibration cube', exact: true });
    await moveDialog.getByLabel('Destination folder').selectOption({ label: 'Calibration / Towers / Posts' });
    await shot(page, 'move-folder-picker');
    await moveDialog.getByRole('button', { name: 'Move', exact: true }).focus();
    await page.keyboard.press('Tab');
    assert.equal(await moveDialog.getByLabel('Destination folder').evaluate((element) => element === document.activeElement), true);
    await moveDialog.getByRole('button', { name: 'Move', exact: true }).click();
    await tile(page, 'Calibration cube').waitFor({ state: 'hidden' });
    await models(page).getByRole('button', { name: 'Posts', exact: true }).click();
    const moving = await page.evaluateHandle(() => new DataTransfer());
    await tile(page, 'Calibration cube').dispatchEvent('dragstart', { dataTransfer: moving });
    const towersCrumb = models(page).getByRole('navigation', { name: 'Folder path' }).getByRole('button', { name: 'Towers', exact: true });
    await towersCrumb.dispatchEvent('dragover', { dataTransfer: moving });
    await towersCrumb.dispatchEvent('drop', { dataTransfer: moving });
    await tile(page, 'Calibration cube').waitFor({ state: 'hidden' });
    await towersCrumb.click();
    await tile(page, 'Calibration cube').waitFor();
    const folderMove = await page.evaluateHandle(() => new DataTransfer());
    await tile(page, 'Calibration cube').dispatchEvent('dragstart', { dataTransfer: folderMove });
    await tile(page, 'Tolerance').dispatchEvent('dragover', { dataTransfer: folderMove });
    await shot(page, 'drag-in-progress');
    await tile(page, 'Tolerance').dispatchEvent('drop', { dataTransfer: folderMove });
    await tile(page, 'Calibration cube').waitFor({ state: 'hidden' });
    await models(page).getByRole('button', { name: 'Tolerance', exact: true }).click();
    await tile(page, 'Calibration cube').waitFor();
    await tile(page, 'Calibration cube').dispatchEvent('dragstart', { dataTransfer: moving });
    const back = models(page).getByRole('navigation', { name: 'Folder path' }).getByRole('button', { name: 'Towers', exact: true });
    await back.dispatchEvent('dragover', { dataTransfer: moving });
    await back.dispatchEvent('drop', { dataTransfer: moving });
    await tile(page, 'Calibration cube').waitFor({ state: 'hidden' });
    await back.click();
    await moving.dispose();
    await folderMove.dispose();
    assert.equal((await snapshot(page)).models[selected.id].parent, selected.parent, 'The picker, folder tiles and breadcrumbs move the same item.');
    await tile(page, 'Calibration cube').getByRole('button', { name: 'Calibration cube', exact: true }).focus();
    await page.keyboard.press('Delete');
    await page.getByRole('button', { name: 'Undo', exact: true }).waitFor();
    assert.equal(await page.getByRole('alertdialog').count(), 0, 'Deleting a model is immediate.');
    await shot(page, 'deleted-undo');
    await page.getByRole('button', { name: 'Undo', exact: true }).click();
    await models(page).getByRole('button', { name: 'Calibration cube', exact: true }).waitFor();
    assert.ok((await snapshot(page)).models[selected.id], 'Undo restores the same model ID.');
    const beforeDrop = await objectCount(page);
    const transfer = await dragFile(page, models(page), 'Dropped.stl', 'dragenter');
    await page.getByText('Drop to add to Towers', { exact: true }).waitFor();
    await shot(page, 'drop-overlay');
    await models(page).dispatchEvent('drop', { dataTransfer: transfer });
    await transfer.dispose();
    await models(page).getByRole('button', { name: 'Dropped', exact: true }).waitFor();
    assert.equal(await objectCount(page), beforeDrop, 'Models drop uploads without adding to the plate.');
    const archive = zipSync({ 'sub/Hook.stl': new Uint8Array(cube), 'sub/deeper/Bridge.stl': new Uint8Array(cube), 'notes.txt': strToU8('Fixture text') });
    const zipClient = await context.newCDPSession(page);
    let zipResponse;
    const zipPaused = new Promise((resolve) => { zipResponse = resolve; });
    await zipClient.send('Fetch.enable', { patterns: [{ urlPattern: '*/api/shelf/uploads*', requestStage: 'Response' }] });
    zipClient.on('Fetch.requestPaused', ({ requestId }) => zipResponse(requestId));
    await models(page).locator('input[type=file]').setInputFiles({ name: 'parts.zip', mimeType: 'application/zip', buffer: Buffer.from(archive) });
    await models(page).getByText('Making preview', { exact: true }).waitFor();
    assert.equal(await models(page).locator('[data-upload-id]').count(), 1, 'A ZIP stays one pending tile.');
    await shot(page, 'zip-upload');
    await zipClient.send('Fetch.continueResponse', { requestId: await zipPaused });
    await models(page).getByRole('button', { name: 'parts', exact: true }).waitFor();
    await zipClient.send('Fetch.disable');
    await zipClient.detach();
    await models(page).getByText("Skipped 1 file that isn't a model", { exact: true }).waitFor();
    await shot(page, 'zip-folder-skipped');
    const imported = await snapshot(page);
    const zipFolder = Object.values(imported.folders).find((folder) => folder.name === 'parts');
    assert.ok(zipFolder);
    assert.ok(Object.values(imported.folders).some((folder) => folder.name === 'sub' && folder.parent === zipFolder.id));
    const client = await context.newCDPSession(page);
    await client.send('Network.enable');
    await client.send('Network.emulateNetworkConditions', { offline: false, latency: 30, downloadThroughput: -1, uploadThroughput: 128 * 1024 });
    let responsePaused;
    const previewPaused = new Promise((resolve) => { responsePaused = resolve; });
    await client.send('Fetch.enable', { patterns: [{ urlPattern: '*/api/shelf/uploads*', requestStage: 'Response' }] });
    client.on('Fetch.requestPaused', ({ requestId }) => responsePaused(requestId));
    const uploading = models(page).locator('input[type=file]').setInputFiles({ name: 'Hook.stl', mimeType: 'model/stl', buffer: Buffer.concat([cube, Buffer.alloc(512 * 1024, 32)]) });
    await models(page).getByRole('progressbar').waitFor();
    await page.waitForFunction(() => {
      const progress = document.querySelector('.shelf-progress-track');
      const value = progress?.getAttribute('aria-valuenow');
      return value !== null && Number(value) > 0 && Number(value) < 1;
    });
    await models(page).locator('.shelf-upload .shelf-caption').getByText('Hook.stl', { exact: true }).waitFor();
    assert.equal(await models(page).locator('.shelf-progress-fill').evaluate((element) => getComputedStyle(element).backgroundColor),
      await models(page).evaluate((element) => { const color = document.createElement('span'); color.style.color = 'var(--accent)'; element.append(color); const value = getComputedStyle(color).color; color.remove(); return value; }));
    await shot(page, 'upload-progress');
    await client.send('Network.emulateNetworkConditions', { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    await uploading;
    await models(page).getByText('Making preview', { exact: true }).waitFor();
    await shot(page, 'making-preview');
    await client.send('Fetch.continueResponse', { requestId: await previewPaused });
    await models(page).getByRole('button', { name: 'Hook', exact: true }).waitFor();
    await client.send('Fetch.disable');
    const skippedArchive = zipSync({ 'one.txt': strToU8('one'), 'two.txt': strToU8('two'), 'three.txt': strToU8('three') });
    await upload(page, { name: 'three-skipped.zip', mimeType: 'application/zip', buffer: Buffer.from(skippedArchive) });
    await models(page).getByText("Skipped 3 files that aren't models", { exact: true }).waitFor();
    await shot(page, 'skipped-files-plural');
    await models(page).locator('input[type=file]').setInputFiles({ name: 'scan 3.stl', mimeType: 'model/stl', buffer: Buffer.from('unreadable model') });
    await models(page).getByRole('button', { name: 'Remove scan 3.stl', exact: true }).waitFor();
    await shot(page, 'upload-failed');
    await page.getByRole('button', { name: 'Prepare', exact: true }).click();
    await page.route('**/api/shelf?tree=1', (route) => route.abort());
    await page.getByRole('button', { name: 'Models', exact: true }).click();
    await models(page).getByRole('button', { name: 'Try again', exact: true }).waitFor();
    await shot(page, 'unreachable-cached');
    assert.ok(await models(page).getByRole('button', { name: 'Benchy', exact: true }).count(), 'Cached models remain available.');
    await page.unroute('**/api/shelf?tree=1');
    await page.getByRole('button', { name: 'Try again', exact: true }).click();
    await models(page).getByRole('button', { name: 'Try again', exact: true }).waitFor({ state: 'hidden' });
    const query = models(page).getByRole('searchbox', { name: 'Search models', exact: true });
    await query.fill('tower');
    await models(page).getByRole('button', { name: 'Temp tower', exact: true }).waitFor();
    assert.equal(await models(page).getByRole('button', { name: 'Retract', exact: true }).count(), 0, 'Search ignores ancestor folder names.');
    assert.equal(await models(page).getByRole('navigation', { name: 'Folder path' }).count(), 0, 'Search hides breadcrumbs.');
    assert.ok(await models(page).getByText('Calibration / Towers', { exact: true }).count());
    await shot(page, 'search-paths');
    await query.fill('');
    await models(page).getByRole('navigation', { name: 'Folder path' }).getByRole('button', { name: 'Calibration', exact: true }).click();
    const beforeCancel = (await snapshot(page)).revision;
    const countLabel = await tile(page, 'Towers').locator('.shelf-meta').textContent();
    assert.equal(countLabel, '16 items', 'Folder tiles count all nested folders and models.');
    await action(page, 'Towers').click();
    await shot(page, 'folder-menu');
    await page.getByRole('menuitem', { name: 'Delete', exact: true }).click();
    await page.getByRole('alertdialog').waitFor();
    assert.equal(await page.getByRole('alertdialog').getAttribute('aria-label'), `Delete Towers and its ${countLabel}?`);
    await shot(page, 'folder-delete-confirm');
    await page.getByRole('alertdialog').getByRole('button', { name: 'Delete', exact: true }).focus();
    await page.keyboard.press('Tab');
    assert.equal(await page.getByRole('button', { name: 'Cancel', exact: true }).evaluate((element) => element === document.activeElement), true);
    await page.getByRole('button', { name: 'Cancel', exact: true }).click();
    assert.equal((await snapshot(page)).revision, beforeCancel, 'Cancel leaves the folder unchanged.');
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await tile(page, 'Towers').hover();
    assert.equal(await tile(page, 'Towers').evaluate((element) => getComputedStyle(element).transform), 'none');
    await shot(page, 'reduced-motion');
    await page.emulateMedia({ reducedMotion: 'no-preference' });
    await page.evaluate(() => { document.documentElement.dataset.theme = 'light'; });
    await shot(page, 'light-theme');
    await page.evaluate(() => { document.documentElement.dataset.theme = 'dark'; });
    const shelfBeforePrepareDrop = await itemCount(page), plateBeforePrepareDrop = await objectCount(page);
    await page.getByRole('button', { name: 'Prepare', exact: true }).click();
    await dragFile(page, page.locator('.canvas-area'), 'Plate drop.stl');
    await page.waitForFunction((count) => window.__plater.getState().objects.length === count + 1, plateBeforePrepareDrop);
    assert.equal(await itemCount(page), shelfBeforePrepareDrop, 'Prepare drop keeps its existing plate-only route.');
    await shot(page, 'prepare-drop');
    await page.waitForFunction(() => /^\/p\/[^/]+/.test(location.pathname));
    const savedId = decodeURIComponent(new URL(page.url()).pathname.split('/')[2]);
    const savedProject = await page.evaluate(async ({ id, count }) => {
      for (let attempt = 0; attempt < 100; attempt++) {
        const response = await fetch(`/api/projects/${encodeURIComponent(id)}`);
        if (response.ok) {
          const record = await response.json();
          if (record.doc.objects.length === count) return record;
        }
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      throw new Error('Added copies did not reach the saved project.');
    }, { id: savedId, count: await objectCount(page) });
    const saved = { id: savedId, revision: savedProject.meta.revision };
    assert.ok(saved.revision > 0, 'Added copies save through the normal project flow.');
    const triangleCounts = await page.evaluate(() => window.__plater.getState().objects.map((object) =>
      (object.geometry.index?.count ?? object.geometry.attributes.position.count) / 3));
    await page.reload();
    await page.waitForFunction((count) => window.__plater?.getState().objects.length === count, triangleCounts.length);
    assert.deepEqual(await page.evaluate(() => window.__plater.getState().objects.map((object) =>
      (object.geometry.index?.count ?? object.geometry.attributes.position.count) / 3)), triangleCounts, 'Copied geometry survives a project reload.');
    let releaseLoading;
    const loadingGate = new Promise((resolve) => { releaseLoading = resolve; });
    const loading = await context.newPage();
    await loading.route('**/api/shelf?tree=1', async (route) => { await loadingGate; await route.continue(); });
    await ready(loading);
    await models(loading).getByRole('status', { name: 'Loading models' }).waitFor();
    await shot(loading, 'skeleton-loading');
    releaseLoading();
    await models(loading).getByRole('button', { name: 'Calibration', exact: true }).waitFor();
    await loading.close();
    const failed = await context.newPage();
    await failed.route('**/thumbnail.png', (route) => route.abort());
    await ready(failed);
    await models(failed).getByRole('button', { name: 'Calibration', exact: true }).click();
    await models(failed).getByRole('button', { name: 'Towers', exact: true }).click();
    await models(failed).locator('.shelf-model-mark').first().waitFor();
    await shot(failed, 'preview-failed');
    await action(failed, 'Benchy').click();
    assert.equal(await failed.getByRole('menuitem', { name: 'Add to Plate', exact: true }).isEnabled(), true);
    await failed.close();
    assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'browser-results.json'), JSON.stringify({ result: 'passed', project: saved.id, revision: saved.revision, copies }, null, 2));
    console.log('Model shelf browser checks passed. Screenshots and browser-results.json saved.');
  } finally {
    await browser.close();
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
