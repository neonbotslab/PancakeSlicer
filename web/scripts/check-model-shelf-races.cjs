const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const [base, output] = process.argv.slice(2);
if (!base || !output) throw new Error('Usage: node web/scripts/check-model-shelf-races.cjs <fixture-dev-url> <output-directory>');
const origin = new URL(base).origin;
if (!['127.0.0.1', 'localhost'].includes(new URL(base).hostname)) throw new Error('Use a local server with temporary fixture data and printer networking disabled.');

const cube = fs.readFileSync(path.resolve(__dirname, '../../engine/tests/data/test_stl/ASCII/20mmbox-LF.stl'));
const models = (page) => page.getByRole('main', { name: 'Models', exact: true });
const button = (scope, name) => scope.getByRole('button', { name, exact: true });
const shelfButton = (page, name) => button(models(page), name);
const shelfName = `Race checks ${Date.now()}`;
const initialModel = 'Seed';
const placementModel = 'Placement.2';
const changedPlate = 'The active plate changed. Add the model again.';
const changedShelf = 'The shelf changed. Check the folder contents before deleting it again.';
const defaultTimeout = 8000;

async function readJSON(page, route) {
  const response = await page.request.get(new URL(route, origin).href);
  assert.ok(response.ok(), `${route} returned ${response.status()}`);
  return response.json();
}

const readShelf = (page) => readJSON(page, '/api/shelf?tree=1');
const objectNames = (page) => page.evaluate(() => window.__plater.getState().objects.map(({ name }) => name));

async function savedProject(page, count, name = null) {
  await page.waitForFunction(() => /^\/p\/[^/]+/.test(location.pathname));
  const id = decodeURIComponent(new URL(page.url()).pathname.split('/')[2]);
  await page.waitForFunction(async ({ id, count, name }) => {
    const response = await fetch(`/api/projects/${encodeURIComponent(id)}`);
    if (!response.ok) return false;
    const { doc, meta } = await response.json();
    return doc.objects.length === count && (!name || meta.name === name);
  }, { id, count, name }, { polling: 100 });
  return readJSON(page, `/api/projects/${encodeURIComponent(id)}`);
}

async function command(scope, page, buttonName, menuName) {
  await button(scope, buttonName).click();
  await page.getByRole('menuitem', { name: menuName, exact: typeof menuName === 'string' }).click();
}

async function submitName(page, label, name) {
  const input = models(page).getByRole('textbox', { name: label, exact: true });
  await input.fill(name);
  await input.press('Enter');
}

async function newFolder(page, name) {
  await shelfButton(page, 'New folder').click();
  await submitName(page, 'Folder name', name);
}

async function chooseFile(page, name) {
  await page.locator('.models-page input[type=file]').setInputFiles({ name: `${name}.stl`, mimeType: 'model/stl', buffer: cube });
}

async function dropFile(page, target, name) {
  const transfer = await page.evaluateHandle(({ name, bytes }) => {
    const data = new DataTransfer();
    const contents = Uint8Array.from(atob(bytes), (character) => character.charCodeAt(0));
    data.items.add(new File([contents], `${name}.stl`, { type: 'model/stl' }));
    return data;
  }, { name, bytes: cube.toString('base64') });
  for (const event of ['dragenter', 'dragover', 'drop']) await target.dispatchEvent(event, { dataTransfer: transfer });
  await transfer.dispose();
}

async function delayRequest(page, url, stage, action) {
  const paused = page.waitForEvent('request', (request) => request.url().match(url));
  paused.catch(() => {});
  const { promise: wait, resolve: resume } = Promise.withResolvers();
  const { promise: started, resolve: signal, reject: fail } = Promise.withResolvers();
  const handle = async (route) => {
    try {
      const response = stage === 'response' ? await route.fetch() : null;
      if (response) assert.ok(response.ok(), `${route.request().url()} returned ${response.status()}`);
      signal();
      await wait;
      if (response) await route.fulfill({ response });
      else await route.continue();
    } catch (error) {
      fail(error);
      await route.abort().catch(() => {});
    }
  };
  await page.route(url, handle, { times: 1 });
  try {
    await action(async () => { await paused; await started; }, resume);
  } finally {
    resume();
    await page.unroute(url, handle);
  }
}

async function openModels(page) {
  const modelsTab = button(page, 'Models');
  if (!(await models(page).isVisible())) await modelsTab.click();
  await models(page).waitFor();
}

async function checkQueuedDestination(page, geometryURL) {
  const original = await savedProject(page, 1);
  const originalPath = new URL(page.url()).pathname;
  let geometryRequests = 0;
  const countGeometry = (request) => { if (geometryURL.test(request.url())) geometryRequests++; };
  page.on('request', countGeometry);
  try {
    await delayRequest(page, /\/api\/shelf\/folders$/, 'request', async (paused, resume) => {
      await newFolder(page, 'Write in progress');
      await paused();
      await command(models(page), page, `Actions for ${placementModel}`, 'Add to Plate');
      await command(page, page, 'File', /^New project/);
      await page.waitForFunction((previous) => location.pathname !== previous && window.__plater.getState().objects.length === 0, originalPath);
      await openModels(page);
      resume();
      await page.getByText(changedPlate, { exact: true }).waitFor();
      await shelfButton(page, 'Write in progress').waitFor();
    });
  } finally {
    page.off('request', countGeometry);
  }
  assert.deepEqual(await objectNames(page), [], 'The newly opened project stays empty.');
  const saved = await readJSON(page, `/api/projects/${encodeURIComponent(original.meta.id)}`);
  assert.equal(saved.doc.objects.length, 1, 'The earlier project still has only its seed model.');
  assert.equal(geometryRequests, 0, 'A queued Add fails before fetching geometry if the project changed.');
  return { earlierProject: original.meta.id, earlierObjects: saved.doc.objects.length, newObjects: 0, geometryRequests };
}

async function checkPendingGeometry(page, geometryURL, folderId) {
  let folderIdWhilePending;
  await delayRequest(page, geometryURL, 'response', async (paused, resume) => {
    await command(models(page), page, `Actions for ${placementModel}`, 'Add to Plate');
    await paused();
    await newFolder(page, 'Created during geometry');
    await shelfButton(page, 'Created during geometry').waitFor();
    const created = Object.values((await readShelf(page)).folders).find(({ name, parent }) => name === 'Created during geometry' && parent === folderId);
    assert.ok(created, 'A folder created while Add waits for geometry is present in storage.');
    folderIdWhilePending = created.id;
    assert.deepEqual(await objectNames(page), [], 'No object is visible before geometry reaches the browser.');
    resume();
    await page.getByText(`Added ${placementModel} to plate`, { exact: true }).waitFor();
  });
  assert.deepEqual(await objectNames(page), [`${placementModel}.stl`]);
  const project = await savedProject(page, 1);
  return { folderId: folderIdWhilePending, project: project.meta.id, savedName: project.doc.objects[0].name };
}

async function checkPendingLease(page, geometryURL, folderId, project) {
  const projectName = `${shelfName} project`;
  const leaseURL = new RegExp(`/api/project-leases/${encodeURIComponent(project)}$`);
  let renameRevision;
  let geometryRequests = 0;
  const countGeometry = (request) => { if (geometryURL.test(request.url())) geometryRequests++; };
  page.on('request', countGeometry);
  try {
    await delayRequest(page, leaseURL, 'request', async (paused, resume) => {
      await button(page, placementModel).and(page.locator('.project-title button')).click();
      const input = page.getByRole('textbox', { name: 'Project name', exact: true });
      await input.fill(projectName);
      await input.press('Enter');
      await paused();
      await command(models(page), page, `Actions for ${placementModel}`, 'Add to Plate');
      await command(models(page), page, 'Actions for Created during geometry', 'Rename');
      await submitName(page, 'Folder name', 'Renamed during lease');
      await shelfButton(page, 'Renamed during lease').waitFor();
      const tree = await readShelf(page);
      assert.ok(Object.values(tree.folders).some(({ name, parent }) => name === 'Renamed during lease' && parent === folderId));
      renameRevision = tree.revision;
      assert.equal(geometryRequests, 0, 'Add waits for the lease before fetching geometry.');
      const resumedGeometry = page.waitForRequest(geometryURL);
      resume();
      await resumedGeometry;
      await page.getByText(/The shelf changed\. Reload it before editing\./i).waitFor();
    });
  } finally {
    page.off('request', countGeometry);
  }
  assert.deepEqual(await objectNames(page), [`${placementModel}.stl`], 'A stale Add does not alter the plate.');
  const saved = await savedProject(page, 1, projectName);
  return { renameRevision, geometryRequests, savedProjectName: saved.meta.name };
}

async function checkDeleteAndDrop(page, folderId) {
  const uploadedName = 'Arrived during confirmation';
  const droppedName = `File drop ${shelfName}`;
  const folderKey = encodeURIComponent(folderId);
  const uploadsURL = new RegExp(`/api/shelf/uploads\\?folder=${folderKey}$`);
  const deleteURL = new RegExp(`/api/shelf/folders/${folderKey}\\?recursive=true$`);
  const uploadPaths = [];
  let deletes = 0;
  const observe = (request) => {
    if (request.method() === 'POST' && request.url().includes('/api/shelf/uploads?folder=')) uploadPaths.push(new URL(request.url()).search);
    if (request.method() === 'DELETE' && deleteURL.test(request.url())) deletes++;
  };
  page.on('request', observe);
  try {
    await delayRequest(page, uploadsURL, 'request', async (paused, resume) => {
      await chooseFile(page, uploadedName);
      await paused();
      await button(models(page).getByRole('navigation', { name: 'Folder path' }), 'Shelf').click();
      await command(models(page), page, `Actions for ${shelfName}`, 'Delete');
      const dialog = page.getByRole('alertdialog', { name: `Delete ${shelfName} and its 2 models?`, exact: true });
      await dialog.waitFor();
      await dropFile(page, dialog, droppedName);
      assert.equal(await page.getByText('Drop to add to Shelf', { exact: true }).count(), 0);
      await button(dialog, 'Delete').click();
      resume();
      await page.getByText(changedShelf, { exact: true }).waitFor();
    });
    await button(page, 'Prepare').click();
    const refreshed = page.waitForResponse((response) => new URL(response.url()).pathname === '/api/shelf' && response.ok());
    await openModels(page);
    await refreshed;
    const modalDropUploads = uploadPaths.length - 1;
    assert.deepEqual(uploadPaths, [`?folder=${folderKey}`], 'Only the file chosen before confirmation was uploaded.');
    assert.equal(deletes, 0, 'The stale confirmation did not submit a recursive Delete.');
    const tree = await readShelf(page);
    const children = Object.values(tree.models).filter(({ parent }) => parent === folderId).map(({ name }) => name).sort();
    assert.deepEqual(children, [`${uploadedName}.stl`, `${placementModel}.stl`, `${initialModel}.stl`].sort());
    assert.ok(tree.folders[folderId], 'The folder survived the stale confirmation.');
    assert.equal(Object.values(tree.models).some(({ name }) => name === `${droppedName}.stl`), false);
    await shelfButton(page, shelfName).click();
    await dropFile(page, models(page), droppedName);
    await shelfButton(page, droppedName).waitFor();
    const afterDrop = await readShelf(page);
    assert.ok(Object.values(afterDrop.models).some(({ name, parent }) => name === `${droppedName}.stl` && parent === folderId));
    return { revision: tree.revision, preservedChildren: children, deleteRequests: deletes, modalDropUploads, uploadsAfterModal: uploadPaths.length - 1 };
  } finally {
    page.off('request', observe);
  }
}

async function main() {
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, serviceWorkers: 'block' });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    page.setDefaultTimeout(defaultTimeout);
    await page.goto(base);
    await page.waitForFunction(() => window.__plater?.getState().profileLibraryLoad.state === 'ready');
    const closeSetup = button(page, 'Close setup');
    if (await closeSetup.count()) await closeSetup.click();
    await command(page, page, 'File', /^New project/);
    await openModels(page);
    await newFolder(page, shelfName);
    await shelfButton(page, shelfName).click();
    for (const name of [initialModel, placementModel]) {
      await chooseFile(page, name);
      await shelfButton(page, name).waitFor();
    }
    const tree = await readShelf(page);
    const folder = Object.values(tree.folders).find(({ name }) => name === shelfName);
    assert.ok(folder);
    const model = Object.values(tree.models).find(({ name, parent }) => name === `${placementModel}.stl` && parent === folder.id);
    assert.ok(model);
    const geometryURL = new RegExp(`/api/shelf/models/${encodeURIComponent(model.id)}/geometry\\.stl\\?revision=\\d+$`);
    await command(models(page), page, `Actions for ${initialModel}`, 'Add to Plate');
    await page.getByText(`Added ${initialModel} to plate`, { exact: true }).waitFor();
    const queuedDestination = await checkQueuedDestination(page, geometryURL);
    const pendingGeometry = await checkPendingGeometry(page, geometryURL, folder.id);
    const pendingLease = await checkPendingLease(page, geometryURL, folder.id, pendingGeometry.project);
    const deleteAndDrop = await checkDeleteAndDrop(page, folder.id);
    assert.deepEqual(errors, []);
    fs.mkdirSync(output, { recursive: true });
    const resultPath = path.join(output, 'race-results.json');
    fs.writeFileSync(resultPath, JSON.stringify({ result: 'passed', folder: shelfName, queuedDestination, pendingGeometry, pendingLease, deleteAndDrop }, null, 2) + '\n');
    console.log(`Model shelf race checks passed. Results written to ${resultPath}`);
  } finally {
    await browser.close();
  }
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
