import feeds from '../../../server/printer_feeds.json';
import type { PrinterRecord, SlotRecord, SwapMode } from '../profiles/records';
import { randomId } from '../random-id';
import type { Tray } from './connections';

interface SlotSide { model?: string; slots: readonly { tray?: string }[]; units?: FeedUnit[]; swap?: SwapMode }

export interface Feed { unit: string; slots: number; maxUnits: number; external: 'exclusive' | 'perNozzle'; nozzles?: string[]; ht?: { slots: number; maxUnits: number } }
export interface FeedUnit { id?: number; nozzle: number; kind?: 'ams-ht' }
type IdentifiedUnit = FeedUnit & { id: number };
export interface SlotPosition { tray: string; label: string; group?: string }

export const feedOf = (model: string | undefined): Feed | undefined => model ? (feeds.models as Record<string, Feed>)[model] : undefined;

const unitId = (id: number) => Number.isInteger(id) && id >= 0 && id < 254;
const validNozzle = (nozzle: number | undefined, count: number): nozzle is number => nozzle !== undefined && Number.isInteger(nozzle) && nozzle >= 0 && nozzle < count;
const unitCapacity = (feed: Feed, unit: FeedUnit) => unit.kind === 'ams-ht' ? feed.ht?.slots ?? 0 : !unit.kind ? feed.slots : 0;
const identified = (units: readonly FeedUnit[]): IdentifiedUnit[] => units.map((unit, index) => ({ ...unit, id: unit.id ?? index }));

function feedUnits(feed: Feed, units: readonly FeedUnit[], nozzles: number): IdentifiedUnit[] | undefined {
  const entries = identified(units), ids = new Set(entries.map((unit) => unit.id));
  const ht = entries.filter((unit) => unit.kind === 'ams-ht').length;
  if (ids.size !== entries.length || ht > (feed.ht?.maxUnits ?? 0) || entries.length - ht > feed.maxUnits
    || entries.some((unit) => !unitId(unit.id) || unit.id >= 128 && unit.kind !== 'ams-ht' || !unitCapacity(feed, unit) || !validNozzle(unit.nozzle, Math.min(nozzles, feed.nozzles?.length ?? 1)))) return undefined;
  return entries;
}

function parseAMSTray(tray: string): { id: number; slot: number } | undefined {
  const match = /^ams(0|[1-9]\d*)\.([0-3])$/.exec(tray);
  if (!match || match[0] !== tray || !unitId(Number(match[1]))) return undefined;
  return { id: Number(match[1]), slot: Number(match[2]) };
}

export function feedTrayKey(feed: Feed | undefined, tray: string | undefined): string | undefined {
  if (!feed || (tray !== 'ext' && tray !== 'ext.L')) return tray;
  return feed.external === 'perNozzle' ? 'ext.L' : 'ext';
}

export function reportedTrayKey(tray: Tray): string {
  if (tray.kind === 'external') return tray.extruder === 0 ? 'ext.L' : tray.extruder === 1 ? 'ext.R' : 'ext';
  return `ams${tray.ams}.${tray.slot}`;
}

export function reportedUnits(trays: readonly Tray[], nozzles: number): IdentifiedUnit[] {
  const units = new Map<number, { ht: Set<boolean>; nozzles: Set<number> }>();
  for (const tray of trays) {
    if (tray.kind === 'external' || !unitId(tray.ams)) continue;
    let unit = units.get(tray.ams);
    if (!unit) units.set(tray.ams, unit = { ht: new Set(), nozzles: new Set() });
    unit.ht.add(tray.kind === 'ams-ht');
    if (tray.extruder !== undefined) unit.nozzles.add(tray.extruder);
  }
  return [...units].sort(([a], [b]) => a - b).flatMap(([id, unit]): IdentifiedUnit[] => {
    const [nozzle] = nozzles === 1 ? [0] : unit.nozzles;
    if (unit.ht.size !== 1 || (nozzles > 1 && unit.nozzles.size !== 1) || !validNozzle(nozzle, nozzles)) return [];
    return [unit.ht.has(true) ? { id, nozzle, kind: 'ams-ht' } : { id, nozzle }];
  });
}

export function resizeStandardUnits(units: readonly FeedUnit[], count: number, nozzle: number): FeedUnit[] {
  let remaining = count;
  const next = identified(units).filter((unit) => {
    if (unit.kind === 'ams-ht') return true;
    if (remaining <= 0) return false;
    remaining--;
    return true;
  });
  const ids = new Set(next.map((unit) => unit.id));
  for (let id = 0; remaining > 0 && unitId(id); id++) {
    if (ids.has(id)) continue;
    next.push({ id, nozzle });
    remaining--;
  }
  return next;
}

export function unitsOf(feed: Feed, setup: Pick<SlotSide, 'units' | 'slots'>): FeedUnit[] {
  if (setup.units) return setup.units;
  const externals = feed.external === 'perNozzle' ? feed.nozzles?.length ?? 1 : 1;
  const extra = feed.external === 'perNozzle' ? setup.slots.length - externals : setup.slots.length > 1 ? setup.slots.length : 0;
  return Array.from({ length: Math.min(feed.maxUnits, Math.ceil(Math.max(0, extra) / feed.slots)) }, () => ({ nozzle: 0 }));
}

export function slotPositions(feed: Feed, units: readonly FeedUnit[], nozzles = feed.nozzles?.length ?? 1): SlotPosition[] {
  const attached = feedUnits(feed, units, nozzles);
  if (!attached) return [];
  let n = 0;
  const amsSlots = (unit: IdentifiedUnit) => Array.from({ length: unitCapacity(feed, unit) }, (_, s): SlotPosition => {
    n++;
    return { tray: `ams${unit.id}.${s}`, label: unit.kind === 'ams-ht' ? `AMS HT ${unit.id >= 128 ? unit.id - 127 : unit.id + 1}` : `Slot ${n}` };
  });
  if (feed.external === 'exclusive') return attached.length ? attached.flatMap(amsSlots) : [{ tray: 'ext', label: 'External' }];
  const names = (feed.nozzles ?? ['']).slice(0, nozzles), fed = (nozzle: number) => attached.some((unit) => unit.nozzle === nozzle);
  const order = names.map((_, nozzle) => nozzle).sort((a, b) => Number(fed(b)) - Number(fed(a)) || a - b);
  return order.flatMap((nozzle) => [
    ...attached.flatMap((unit) => unit.nozzle === nozzle ? amsSlots(unit) : []),
    { tray: nozzle === 0 ? 'ext.L' : 'ext.R', label: 'External' },
  ].map((p) => ({ ...p, group: `${names[nozzle]} nozzle` })));
}

export function nextTray(printer: Pick<PrinterRecord, 'name' | 'model' | 'nozzles' | 'swap' | 'units'>, slots: readonly { tray?: string }[]): string | undefined {
  const feed = feedOf(printer.model);
  if (!feed || printer.swap === 'toolheads') return undefined;
  const positions = slotPositions(feed, unitsOf(feed, { units: printer.units, slots }), printer.nozzles.length), held = new Set(slots.map((slot) => feedTrayKey(feed, slot.tray)));
  const free = positions.find((p) => !held.has(p.tray));
  if (!free) throw new Error(`${printer.name} doesn't have any free trays for another slot. Attach another ${feed.unit} unit on Device to add a slot!`);
  return free.tray;
}

export function layoutSetup(printer: PrinterRecord, units?: FeedUnit[], assign = false): PrinterRecord {
  const feed = feedOf(printer.model);
  if (!feed) return printer;
  const attached = units ?? unitsOf(feed, printer);
  const positions = slotPositions(feed, attached, printer.nozzles.length), at = new Set(positions.map((p) => p.tray));
  if (!positions.length) return printer;
  const mixed = mixedSlots(printer);
  const slots: SlotRecord[] = printer.slots.flatMap((slot) => {
    const tray = feedTrayKey(feed, slot.tray);
    if (!tray || (!units && parseAMSTray(tray))) return [slot];
    if (at.has(tray)) return [units && tray !== slot.tray ? { ...slot, tray } : slot];
    const { tray: _gone, ...rest } = slot;
    return slot.filament || mixed.has(slot.id) ? [rest] : [];
  });
  const held = new Set(slots.map((slot) => feedTrayKey(feed, slot.tray)));
  const free = positions.filter((p) => !held.has(p.tray));
  if (assign) for (const [i, slot] of slots.entries()) if (!slot.tray && free.length) slots[i] = { ...slot, tray: free.shift()!.tray };
  const ids = new Set(slots.map((slot) => slot.id));
  for (const p of free) slots.push({ id: ids.has(`slot:${printer.id}:${p.tray}`) ? `slot:${randomId()}` : `slot:${printer.id}:${p.tray}`, filament: '', tray: p.tray });
  const same = slots.length === printer.slots.length && slots.every((s, i) => s === printer.slots[i])
    && JSON.stringify(attached) === JSON.stringify(printer.units);
  return same ? printer : { ...printer, units: attached, slots };
}

const mixedSlots = (printer: PrinterRecord) => new Set(printer.mixes?.flatMap((mix) => mix.components));

export function liveSlotSetup(printer: PrinterRecord, tray: Tray, trays: readonly Tray[], filament: string): PrinterRecord {
  const feed = feedOf(printer.model), nozzles = printer.nozzles.length;
  const key = feedTrayKey(feed, reportedTrayKey(tray));
  const unknownRoute = () => new Error('The tray nozzle is unknown. Wait for the printer to report its layout and try again.');
  const unavailable = () => new Error('The reported tray is unavailable in the saved slot layout.');
  if (nozzles > 1) {
    if (!validNozzle(tray.extruder, nozzles)) throw unknownRoute();
    if (tray.kind === 'external' && trays.some((t) => t.kind === 'external' && t.ams === tray.ams && t.extruder !== undefined && t.extruder !== tray.extruder)) throw unknownRoute();
  }
  let setup = printer;
  if (feed) {
    const known = feedUnits(feed, printer.units ?? [], nozzles);
    if (!known) throw new Error('The saved feeder layout is invalid.');
    const reported = reportedUnits(trays, nozzles);
    if (tray.kind !== 'external' && !reported.some((unit) => unit.id === tray.ams && (nozzles === 1 || unit.nozzle === tray.extruder))) throw unknownRoute();
    const mixed = mixedSlots(printer);
    const occupied = printer.slots.filter((slot) => slot.tray && (slot.filament || mixed.has(slot.id)));
    const held = new Set(occupied.flatMap((slot) => {
      const ams = parseAMSTray(slot.tray ?? '');
      return ams ? [ams.id] : [];
    }));
    const reportedIds = new Set(trays.filter((t) => t.kind !== 'external').map((t) => t.ams));
    const attachedById = new Map(reported.map((unit) => [unit.id, unit]));
    if (feed.external === 'perNozzle') for (const unit of known) {
      if (!attachedById.has(unit.id) && (held.has(unit.id) || reportedIds.has(unit.id))) attachedById.set(unit.id, unit);
      else if (held.has(unit.id) && unit.id !== tray.ams && attachedById.get(unit.id)?.nozzle !== unit.nozzle) attachedById.set(unit.id, unit);
    }
    const attached = [...attachedById.values()].sort((a, b) => a.id - b.id);
    const positions = new Set(slotPositions(feed, attached, nozzles).map((position) => position.tray));
    if (!positions.size) throw new Error('The printer reported an unsupported feeder layout.');
    for (const slot of feed.external === 'perNozzle' ? occupied : []) {
      const physical = feedTrayKey(feed, slot.tray);
      const from = trayNozzle(feed, known, nozzles, slot.tray);
      const to = trayNozzle(feed, attached, nozzles, slot.tray);
      const selectedUnit = tray.kind !== 'external' && parseAMSTray(slot.tray ?? '')?.id === tray.ams;
      if (!physical || !positions.has(physical) || (from !== undefined && from !== to && (!selectedUnit || to !== tray.extruder))) {
        throw new Error('The printer reported a feeder layout that would move a saved filament slot.');
      }
    }
    if (!key || !positions.has(key)) throw unavailable();
    setup = layoutSetup(printer, attached);
  }
  const targets = setup.slots.filter((slot) => feedTrayKey(feed, slot.tray) === key);
  if (targets.length !== 1) throw unavailable();
  return { ...setup, slots: setup.slots.map((slot) => slot === targets[0] ? { ...slot, filament } : slot) };
}

export function slotLabels(setup: SlotSide | undefined, model: string | undefined, count: number): SlotPosition[] {
  const feed = feedOf(model), positions = feed && setup ? slotPositions(feed, unitsOf(feed, setup)) : [];
  return Array.from({ length: count }, (_, i) => {
    const tray = setup?.slots[i]?.tray ?? '';
    const position = positions.find((p) => p.tray === feedTrayKey(feed, tray));
    return position ? { ...position, tray } : { tray, label: feed && i < (setup?.slots.length ?? 0) ? 'No tray' : `Slot ${i + 1}` };
  });
}

function trayNozzle(feed: Feed | undefined, units: readonly IdentifiedUnit[], nozzles: number, tray: string | undefined): number | undefined {
  if (tray === 'ext' || tray === 'ext.L') return nozzles >= 1 ? 0 : undefined;
  if (tray === 'ext.R') return nozzles >= 2 ? 1 : undefined;
  const ams = parseAMSTray(tray ?? '');
  if (!ams || !feed) return undefined;
  const [unit, duplicate] = units.filter((u) => u.id === ams.id);
  if (!unit || duplicate || ams.slot >= unitCapacity(feed, unit) || !validNozzle(unit.nozzle, Math.min(nozzles, feed.nozzles?.length ?? 1))) return undefined;
  return unit.nozzle;
}

export function slotExtruder(setup: SlotSide | undefined, nozzles: number, i: number): number | undefined {
  const slot = setup?.slots[i];
  if (!slot || nozzles < 1) return undefined;
  if (nozzles === 1 && (setup?.swap === 'toolheads' || !slot.tray || !setup?.units?.some((unit) => unit.id != null || unit.kind))) return 0;
  if (setup?.swap === 'toolheads') return i >= 0 && i < nozzles ? i : undefined;
  const feed = feedOf(setup?.model);
  return trayNozzle(feed, identified(setup?.units ?? []), nozzles, slot.tray);
}
