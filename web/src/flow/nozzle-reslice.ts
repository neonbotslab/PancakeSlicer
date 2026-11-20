import type { Config } from '../api/types';
import { valueAt } from '../multinozzle/config';
import { nozzleReslice } from '../multinozzle/send-nozzle';
import type { SendFilament } from '../multinozzle/ams';
import { listedPlates } from '../plater/plates';
import type { Tray } from '../printers/connections';
import { feedOf, feedTrayKey, liveSlotSetup, reportedTrayKey, slotExtruder } from '../printers/feeds';
import { sameRecord, type PrinterRecord } from '../profiles/records';
import { stripOfRecord } from '../profiles/slots';
import { documentGeneration, undoable } from '../state/history';
import { effectiveConfig, getState, setState, type PlateObject, type State } from '../state/store';
import { followPlacement } from './project-placement';

interface NozzleSlotChange {
  before: PrinterRecord;
  record: PrinterRecord;
  tray: Tray;
  positions: number[];
  source: number;
  target: number;
  replacement: string;
}

export function nozzleSlotChange({
  record, slot, filament, trays, candidates, filaments, resolvedTypes,
}: {
  record: PrinterRecord; slot: string; filament: SendFilament; trays: Tray[]; candidates: Tray[];
  filaments: SendFilament[]; resolvedTypes: (string | undefined)[];
}): NozzleSlotChange | undefined {
  const source = record.slots.findIndex((s) => s.id === slot), key = filament.key;
  if (source < 0 || !key || record.slots[source].filament !== key) return undefined;
  const feed = feedOf(record.model);
  const trayKey = (tray: Tray) => feedTrayKey(feed, reportedTrayKey(tray));
  const ordered = candidates.flatMap((tray) => {
    const held = record.slots.findIndex((s) => feedTrayKey(feed, s.tray) === trayKey(tray));
    const profile = record.slots[held]?.filament;
    const replacement = profile && profile !== key ? profile : '';
    if (replacement) {
      const target = { ...filaments[held], type: resolvedTypes[held] ?? '', extruder: filament.extruder };
      if (nozzleReslice({ filament: target, trays: [tray], platePrinter: record.id, sendPrinter: record.id })?.kind !== 'reslice') return [];
    }
    return [{ tray, replacement, priority: held === source ? 0 : profile === key ? 1 : replacement ? 3 : 2 }];
  }).sort((a, b) => a.priority - b.priority);
  for (const { tray, replacement } of ordered) {
    if (nozzleReslice({ filament, trays: [tray], platePrinter: record.id, sendPrinter: record.id })?.kind !== 'reslice') continue;
    let next;
    try { next = liveSlotSetup(record, tray, trays, replacement || key); }
    catch { continue; }
    const target = next.slots.findIndex((s) => feedTrayKey(feed, s.tray) === trayKey(tray));
    if (target < 0 || slotExtruder(next, next.nozzles.length, target) !== tray.extruder) continue;
    const previous = stripOfRecord(record).ids, ids = stripOfRecord(next).ids;
    const positions = previous.map((id) => ids.indexOf(id));
    if (positions.some((i) => i < 0)) continue;
    const changedMix = record.mixes?.some((mix) => mix.components.some((id) => {
      const at = record.slots.findIndex((s) => s.id === id);
      return slotExtruder(record, record.nozzles.length, at) !== slotExtruder(next, next.nozzles.length, positions[at]);
    }));
    if (changedMix) continue;
    return { before: record, record: next, tray, positions, source, target, replacement };
  }
}

const SLOT_OPTIONS = ['extruder', 'wall_filament', 'sparse_infill_filament', 'solid_infill_filament', 'support_filament', 'support_interface_filament',
  'wipe_tower_filament', 'outer_wall_filament_id', 'inner_wall_filament_id', 'sparse_infill_filament_id', 'internal_solid_filament_id',
  'top_surface_filament_id', 'bottom_surface_filament_id'];

export function nozzleSlotPatch(state: State, change: Pick<NozzleSlotChange, 'source' | 'target' | 'positions' | 'replacement'>): Partial<State> {
  const { source, target, positions, replacement } = change;
  const map = positions.map((at, i) => (i === source ? target : at) + 1);
  const move = (n: number) => map[n - 1] ?? n;
  const changedSettings = (config: Config) => {
    const patch: Config = {};
    for (const key of SLOT_OPTIONS) {
      const value = config[key], n = Number(valueAt(value, 0));
      if (move(n) !== n && Number.isInteger(n)) patch[key] = typeof value === 'number' ? move(n) : String(move(n));
    }
    return patch;
  };
  const settings = (config: Config) => ({ ...config, ...changedSettings(config) });
  const plate = (o: PlateObject): PlateObject => ({
    ...o, settings: settings(o.settings), ranges: o.ranges?.map((r) => ({ ...r, settings: settings(r.settings) })),
    volumes: o.volumes?.map((v) => ({ ...v, extruder: v.type === 'part' ? v.extruder : move(v.extruder), settings: settings(v.settings) })),
  });
  const reindex = <T,>(values: T[], empty: T): T[] => {
    const out: T[] = Array.from({ length: Math.max(values.length, ...map) }, () => empty);
    for (const [i, value] of values.entries()) out[positions[i] ?? i] = value;
    if (!replacement && values[source] !== undefined) out[target] = values[source];
    return out;
  };
  const changed = map.some((at, i) => at !== i + 1);
  const remapped = changed ? followPlacement(state, map) : {};
  const objects = changed ? (remapped.objects ?? state.objects).map(plate) : state.objects;
  const events = remapped.events ?? state.events;
  const plates = listedPlates({ ...state, ...remapped, objects, events }).map((p) => {
    const active = p.index === state.plateIndex;
    return { ...p, objects: changed && !active ? p.objects.map(plate) : p.objects,
      settings: { ...(changed ? settings(p.settings) : p.settings), ...(active ? { filament_map_mode: 'Manual' } : {}) } };
  });
  const process = changedSettings(effectiveConfig(state, 'process'));
  return {
    ...remapped, objects, events, projectPlates: plates,
    ...(changed && Object.keys(process).length ? { dirty: { ...state.dirty, process: { ...state.dirty.process, ...process } } } : {}),
    ...(changed && state.projectPresets ? { projectPresets: { ...state.projectPresets, filaments: reindex(state.projectPresets.filaments, {}) } } : {}),
    ...(changed && state.projectSlotValues ? { projectSlotValues: reindex(state.projectSlotValues, null) } : {}),
  };
}

export async function applyNozzleSlotChange(before: State, change: NozzleSlotChange): Promise<boolean> {
  const generation = documentGeneration();
  const recordOf = (s: State) => s.records.printers.find((p) => p.id === before.printer);
  if (!sameRecord(recordOf(before), change.before)) return false;
  const patch = nozzleSlotPatch(before, change);
  if (!sameRecord(change.before, change.record)) {
    const { commitRecords } = await import('../profiles/transactions');
    await commitRecords([{ kind: 'printer', record: change.record, base: change.before }], { label: 'Printer setup' });
  }
  const current = getState();
  if (generation !== documentGeneration() || current.printer !== before.printer || current.plateIndex !== before.plateIndex
    || current.job.id !== before.job.id || current.objects !== before.objects || current.events !== before.events
    || current.projectPlates !== before.projectPlates || current.projectPresets !== before.projectPresets
    || current.projectSlotValues !== before.projectSlotValues || current.dirty !== before.dirty
    || !sameRecord(recordOf(current), change.record)) return false;
  undoable('Change nozzle', () => setState(patch));
  return true;
}
