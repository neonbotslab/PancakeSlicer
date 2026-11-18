import assert from 'node:assert/strict';
import test from 'node:test';
import { matchPrinterFilaments } from '../src/multinozzle/printer-filament-context.ts';
import { nozzleReslice, sameMappedTrays, trayFeedsNozzle } from '../src/multinozzle/send-nozzle.ts';
import type { PrinterConnections, Tray } from '../src/printers/connections';

const ams: Tray = { id: 0, ams: 0, slot: 0, extruder: 0, kind: 'ams-2-pro', type: ' PLA ', color: '#123abc' };
const ht: Tray = { id: 128, ams: 128, slot: 0, extruder: 1, kind: 'ams-ht', type: '', color: '#FFFFFF' };
const external: Tray = { id: 255, ams: 255, slot: 0, extruder: 1, kind: 'external', type: 'PETG', color: '#fefefe' };

function match(trays: Tray[] | undefined, overrides: Partial<Parameters<typeof matchPrinterFilaments>[0]> = {}) {
  const connections: PrinterConnections = {
    enabled: true, live: true, transfers: [],
    connections: [
      { printer: 'selected', id: 'first', name: 'Selected', kind: 'bambu-lan', has_secret: true },
      { printer: 'other', id: 'second', name: 'Other', kind: 'bambu-lan', has_secret: true },
    ],
    status: {
      selected: { printer: 'selected', online: true, state: 'idle', at: '', trays },
      other: { printer: 'other', online: true, state: 'idle', at: '', trays: [external] },
    },
  };
  return matchPrinterFilaments({ printer: 'selected', connections, vendor: 'BBL', nozzleCount: 2, mode: 'Auto For Match', supported: true, ...overrides });
}

test('Auto Match takes capacity and loaded materials from the selected printer', () => {
  const trays = [ams, { ...ams, id: 1, slot: 1, type: '   ' }, ht,
    { ...external, extruder: undefined }, { ...ams, ams: 1, id: 4, extruder: 3 },
    { ...ams, ams: 2, id: 8, kind: 'ams', type: 'PETG', color: 'invalid' }];
  const matched = match(trays);
  assert.deepEqual(matched, { kind: 'ready', context: { ams_slots: [8, 1], loaded_filaments: [
    { extruder: 0, type: 'PLA', color: '#123ABC', external: false },
  ] } });
  assert.deepEqual(match([...trays].reverse()), matched);
  assert.deepEqual(match(trays.map((tray) => ({ ...tray, name: 'Unused field', filament_id: 'Unused ID', active: true, remaining: 70 }))), matched);
  assert.deepEqual(match([...trays, ams]), matched);
  assert.notDeepEqual(match(trays.map((tray) => tray === ams ? { ...tray, type: 'ABS' } : tray)), matched);
  assert.notDeepEqual(match([...trays, external]), matched);
  assert.notDeepEqual(match(trays.map((tray) => tray === ams ? { ...tray, extruder: 1 } : tray)), matched);
  const conflict = [...trays, { ...ams, extruder: 1 }, external];
  assert.deepEqual(match(conflict), { kind: 'ready', context: { ams_slots: [4, 1], loaded_filaments: [
    { extruder: 1, type: 'PETG', color: '#FEFEFE', external: true },
  ] } });
  assert.deepEqual(match([...conflict].reverse()), match(conflict));
  assert.deepEqual(match(trays, { printer: 'other' }), { kind: 'ready', context: { ams_slots: [0, 0], loaded_filaments: [
    { extruder: 1, type: 'PETG', color: '#FEFEFE', external: true },
  ] } });
});

test('Auto Match reports missing inventory or engine support and leaves other modes available', () => {
  assert.equal(match(undefined).kind, 'unavailable');
  assert.equal(match([ht]).kind, 'unavailable');
  assert.equal(match([ams], { supported: false }).kind, 'unavailable');
  assert.equal(match([ams], { mode: 'Auto For Flush' }).kind, 'unused');
  assert.equal(match([ams], { mode: 'Manual' }).kind, 'unused');
  assert.equal(match([ams], { nozzleCount: 1 }).kind, 'unused');
});

test('opposite-side filament offers a reslice on the plate printer only', () => {
  const filament = { type: 'PETG', extruder: 0 };
  const target = { filament, trays: [ams, external], platePrinter: 'selected', sendPrinter: 'selected' };
  assert.deepEqual(nozzleReslice(target), { kind: 'reslice', side: 1 });
  assert.deepEqual(nozzleReslice({ ...target, trays: [...target.trays, { ...ams, type: 'PETG' }] }), { kind: 'reslice', side: 1 });
  assert.deepEqual(nozzleReslice({ ...target, sendPrinter: 'other' }), { kind: 'select-printer', side: 1 });
  assert.equal(nozzleReslice({ ...target, filament: { ...filament, extruder: 1 } }), undefined);
  assert.deepEqual(nozzleReslice({ ...target, filament: { ...filament, printable: 1 } }), { kind: 'incompatible', side: 1 });
  assert.equal(nozzleReslice({ ...target, trays: [ams] }), undefined);
});

test('a tray with unknown routing is unavailable when the slice specifies a nozzle', () => {
  const unknown = { ...external, extruder: undefined };
  assert.equal(trayFeedsNozzle(unknown, 0), false);
  assert.equal(trayFeedsNozzle(unknown, 1), false);
  assert.equal(trayFeedsNozzle(unknown, undefined), true);
  assert.equal(trayFeedsNozzle(external, 1), true);
});

test('Manual and Auto Flush sends reject changed selected inventory after confirmation', () => {
  const before = [ams, external], mapping = [ams.id];
  const unchanged = (after: Tray[]) => sameMappedTrays({ before, after, mapping });
  assert.equal(unchanged([external, { ...ams, active: true, remaining: 60 }]), true);
  assert.equal(unchanged([ams, { ...external, extruder: 0, type: 'ABS' }]), true);
  assert.equal(unchanged([external]), false);
  assert.equal(unchanged([{ ...ams, extruder: 1 }, external]), false);
  assert.equal(unchanged([{ ...ams, type: 'PETG' }, external]), false);
  assert.equal(unchanged([{ ...ams, color: '#FFFFFF' }, external]), false);
  assert.equal(unchanged([{ ...ams, filament_id: 'GFA00' }, external]), false);
  const unknownExternal = { ...external, type: '', color: '' };
  assert.equal(sameMappedTrays({ before: [unknownExternal], after: [{ ...unknownExternal, active: true }], mapping: [255] }), true);
});
