const { chromium } = require(process.env.PLAYWRIGHT_CORE ?? 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const [base, output, tool, resources] = process.argv.slice(2);
if (!base || !output || !tool || !resources)
  throw new Error('Usage: node scripts/check-micron-plus.cjs <temporary-dev-url> <output-directory> <engine-tool> <resources-directory>');
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(base).hostname), 'Use a local dev server with a temporary data directory');
fs.mkdirSync(output, { recursive: true });

async function json(url) {
  const response = await fetch(new URL(url, base));
  assert.equal(response.ok, true, `${url}: ${response.status}`);
  return response.json();
}

function gcodeBounds(gcode) {
  const bounds = Object.fromEntries(['X', 'Y', 'Z'].map(axis => [axis, { min: Infinity, max: -Infinity }]));
  let moves = 0;
  for (const line of gcode.split('\n')) {
    const command = line.split(';')[0].trim();
    assert.ok(!/^G91(?:\s|$)/.test(command), 'Bounds check expects absolute XYZ moves');
    assert.ok(!/^G[23](?:\s|$)/.test(command), 'Bounds check expects linear moves');
    if (!/^G[01](?:\s|$)/.test(command)) continue;
    moves++;
    for (const match of command.matchAll(/\b([XYZ])([-+]?(?:\d+(?:\.\d*)?|\.\d+))/g)) {
      const value = Number(match[2]), axis = bounds[match[1]];
      axis.min = Math.min(axis.min, value);
      axis.max = Math.max(axis.max, value);
    }
  }
  assert.ok(moves > 100, 'The engine produced cube toolpaths');
  for (const [axis, max] of [['X', 173], ['Y', 180], ['Z', 165]]) {
    assert.ok(Number.isFinite(bounds[axis].min) && Number.isFinite(bounds[axis].max), `${axis} has measured bounds`);
    assert.ok(bounds[axis].min >= 0 && bounds[axis].max <= max, `${axis} exceeds the printable volume: ${JSON.stringify(bounds[axis])}`);
  }
  return { moves, bounds };
}

(async () => {
  const catalog = await json('/api/catalog/printers');
  const printer = catalog.printers.find(p => p.id === 'Micron+');
  assert.ok(printer, 'Micron+ appears in the server printer catalog');
  assert.equal(printer.vendor, 'PrintersForAnts');
  assert.equal(printer.bed, '173x180x165');
  assert.deepEqual(printer.nozzles, ['0.4']);
  const model = await json('/api/catalog/printers/Micron%2B');
  assert.equal(model.model.default_bed_type, 'Textured PEI Plate');
  const variant = model.variants.find(v => v.default && v.nozzle === '0.4');
  assert.ok(variant, 'Micron+ has a default 0.4 mm nozzle');
  assert.equal(variant.defaultPrint, '0.20mm Standard');
  const machine = { ...model.base, ...variant.values };
  assert.equal(machine.gcode_flavor, 'klipper');
  assert.equal(machine.printable_area, '0x0,173x0,173x180,0x180');
  assert.equal(machine.printable_height, '165');
  for (const asset of [model.model.bed_model, model.model.bed_texture]) {
    assert.ok(asset, 'Micron+ declares its bed assets');
    const response = await fetch(new URL(`/profiles/PrintersForAnts/${asset}`, base));
    assert.equal(response.ok, true, `Bed asset ${asset} is served`);
    assert.ok((await response.arrayBuffer()).byteLength > 1000);
  }
  const prints = await json('/api/catalog/prints');
  const standard = prints.prints.find(p => p.id === '0.20mm Standard');
  assert.ok(standard);
  const compatible = {};
  for (const [kind, names] of [['prints', ['0.12mm Fine', '0.20mm Standard', '0.28mm Extra Draft']],
    ['filaments', ['Generic PLA', 'Generic PETG', 'Generic ABS', 'Generic ASA', 'Generic TPU']]]) {
    compatible[kind] = [];
    for (const name of names) {
      const product = await json(`/api/catalog/${kind}/${encodeURIComponent(name)}`);
      assert.ok(product.tunings.some(t => t.fits.includes(variant.id)), `${name} fits the Micron+ nozzle`);
      compatible[kind].push(name);
    }
  }
  const before = await json('/api/library');
  assert.equal(before.document.printers.length, 0, 'Start with an empty temporary data directory');
  const browser = await chromium.launch({ headless: true, args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] });
  let saved;
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(base);
    await page.waitForFunction(() => window.__plater?.getState().profileLibraryLoad.state === 'ready');
    await page.getByRole('dialog', { name: 'Setup', exact: true }).getByRole('button', { name: 'Close setup', exact: true }).click();
    await page.getByRole('button', { name: 'Library', exact: true }).click();
    await page.locator('button[aria-label="Add printer"]').click();
    await page.getByRole('searchbox', { name: 'Search', exact: true }).fill('Micron');
    const grid = page.getByRole('grid', { name: 'Add printer', exact: true });
    const row = grid.getByRole('row').filter({ hasText: 'Micron+' });
    await row.click();
    await page.screenshot({ path: path.join(output, 'add-printer-micron-plus.png') });
    const write = page.waitForResponse(response => new URL(response.url()).pathname === '/api/library' && response.request().method() === 'PUT' && response.ok());
    await page.locator('.pf-detail-pane').getByRole('button', { name: 'Add printer', exact: true }).click();
    await write;
    await page.waitForFunction(() => window.__plater.getState().records.printers.some(p => p.model === 'Micron+' && !p.archived));
    saved = (await json('/api/library')).document.printers.find(p => p.model === 'Micron+' && !p.archived);
    assert.ok(saved, 'Add Printer persisted the Micron+ in the server library');
    assert.equal(saved.vendor, printer.vendor);
    assert.equal(saved.nozzles.length, 1);
    assert.equal(saved.nozzles[0].variant, variant.id);
    assert.equal(saved.nozzles[0].diameter, 0.4);
    await page.reload();
    await page.waitForFunction(() => window.__plater?.getState().profileLibraryLoad.state === 'ready');
    assert.ok(await page.evaluate(id => window.__plater.getState().records.printers.some(p => p.id === id && p.model === 'Micron+'), saved.id), 'Micron+ survives a reload');
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
  const cube = path.join(output, 'cube.obj'), gcodeFile = path.join(output, 'cube.gcode');
  fs.writeFileSync(cube, 'v 0 0 0\nv 20 0 0\nv 20 20 0\nv 0 20 0\nv 0 0 20\nv 20 0 20\nv 20 20 20\nv 0 20 20\nf 1 3 2\nf 1 4 3\nf 5 6 7\nf 5 7 8\nf 1 2 6\nf 1 6 5\nf 2 3 7\nf 2 7 6\nf 3 4 8\nf 3 8 7\nf 4 1 5\nf 4 5 8\n');
  const args = ['--catalog', path.join(resources, 'catalog'), '--profiles', path.join(resources, 'profiles'),
    '--datadir', path.join(output, 'engine-data'), '--printer', saved.nozzles[0].variant, '--process', standard.id,
    '--filament', 'Generic PLA', '--center', '86.5,90', '--config-out', path.join(output, 'cube.config.json'),
    '--json', path.join(output, 'cube.slice.json'), '-o', gcodeFile, cube];
  const sliced = spawnSync(tool, args, { encoding: 'utf8', timeout: 120000, maxBuffer: 8 * 1024 * 1024 });
  fs.writeFileSync(path.join(output, 'engine.log'), `${sliced.stdout ?? ''}\n${sliced.stderr ?? ''}`);
  assert.equal(sliced.error, undefined);
  assert.equal(sliced.status, 0, sliced.stderr || sliced.stdout);
  const gcode = fs.readFileSync(gcodeFile, 'utf8');
  assert.ok(!/^SET_VELOCITY_LIMIT\b[^;\n]*\bACCEL_TO_DECEL=/m.test(gcode), 'G-code supports current Klipper velocity limits');
  assert.ok(!/^EXCLUDE_OBJECT\b|^EXCLUDE_OBJECT_/m.test(gcode), 'G-code works without optional Klipper object exclusion');
  const start = gcode.match(/^PRINT_START EXTRUDER=(\d+(?:\.\d+)?) BED=(\d+(?:\.\d+)?) CHAMBER=(\d+(?:\.\d+)?)[ \t]*$/m);
  assert.ok(start, 'PRINT_START contains resolved Orca temperature placeholders');
  assert.ok(gcode.slice(0, start.index).endsWith('M104 S0\nM140 S0\n'), 'Heater resets immediately precede PRINT_START in the official order');
  assert.ok(Number(start[1]) > 0 && Number(start[1]) <= 300);
  assert.ok(Number(start[2]) > 0 && Number(start[2]) <= 120);
  const firstExtrusion = gcode.search(/^G1\b[^;\n]*\b[XY][^;\n]*\bE[\d.-]+/m);
  assert.ok(firstExtrusion > start.index, 'PRINT_START precedes cube extrusion');
  assert.ok(gcode.search(/^PRINT_END[ \t]*$/m) > firstExtrusion, 'PRINT_END follows cube extrusion');
  const result = { catalog: printer, persisted: true, reloaded: true, variant: saved.nozzles[0], compatible,
    start: start[0], end: 'PRINT_END', ...gcodeBounds(gcode), gcodeBytes: fs.statSync(gcodeFile).size };
  fs.writeFileSync(path.join(output, 'verification.json'), JSON.stringify(result, null, 2));
  console.log(JSON.stringify(result));
})().catch(error => { console.error(error); process.exitCode = 1; });
