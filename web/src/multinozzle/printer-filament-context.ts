import type { PrinterConnections, Tray } from '../printers/connections';

export type NozzleSide = 0 | 1;
export const isNozzleSide = (side: number | undefined): side is NozzleSide => side === 0 || side === 1;

export interface PrinterFilamentContext {
  ams_slots: [number, number];
  loaded_filaments: { extruder: NozzleSide; type: string; color: string; external: boolean }[];
}

type PrinterFilamentMatch =
  | { kind: 'unused' }
  | { kind: 'unavailable'; reason: string }
  | { kind: 'ready'; context: PrinterFilamentContext };

export function matchPrinterFilaments({ printer, connections, vendor, nozzleCount, mode, supported }: {
  printer: string; connections: PrinterConnections; vendor: string | undefined; nozzleCount: number;
  mode: unknown; supported: boolean;
}): PrinterFilamentMatch {
  if (vendor !== 'BBL' || nozzleCount !== 2 || mode !== 'Auto For Match') return { kind: 'unused' };
  if (!supported) return { kind: 'unavailable', reason: 'Auto (best match) requires a newer slicing engine. Choose Auto (best for flushing) or Manual.' };
  const status = connections.status[printer];
  const connected = connections.live && connections.connections.some((c) => c.printer === printer && c.kind === 'bambu-lan');
  const context = connected && status?.online && status.trays ? filamentContext(status.trays) : undefined;
  return context?.loaded_filaments.length ? { kind: 'ready', context }
    : { kind: 'unavailable', reason: 'Auto (best match) needs loaded filament with a known nozzle assignment on the selected printer. Check the printer connection and filament, or choose Auto (best for flushing) or Manual.' };
}

const amsCapacity = new Map([['ams', 4], ['ams-lite', 4], ['ams-2-pro', 4], ['ams-lite-mixed', 4], ['ams-ht', 1]]);
const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0;
const encoder = new TextEncoder();

function filamentContext(trays: Tray[]): PrinterFilamentContext {
  const units = new Map<number, { extruder: NozzleSide; capacity: number } | null>();
  const slots = new Map<string, { ams: number; filament: PrinterFilamentContext['loaded_filaments'][number] } | null>();
  for (const tray of trays) {
    const { ams, slot, extruder } = tray;
    if (!isNozzleSide(extruder)) continue;
    const external = tray.kind === 'external';
    const capacity = amsCapacity.get(tray.kind ?? '');
    if (!external) {
      if (capacity === undefined || !Number.isInteger(ams) || ams < 0 || ams > 255 || !Number.isInteger(slot) || slot < 0 || slot >= capacity) continue;
      const unit = units.get(ams);
      if (unit === undefined) units.set(ams, { extruder, capacity });
      else if (unit && (unit.extruder !== extruder || unit.capacity !== capacity)) units.set(ams, null);
    }
    const type = tray.type.trim();
    if (!type || encoder.encode(type).length > 64 || /[\x00-\x1f\x7f]/.test(type) || !/^#[0-9a-f]{6}$/i.test(tray.color)) continue;
    const key = external ? `external:${extruder}` : `${ams}:${slot}`;
    const color = tray.color.toUpperCase();
    const previous = slots.get(key);
    if (previous === undefined) slots.set(key, { ams, filament: { extruder, type, color, external } });
    else if (previous && (previous.filament.extruder !== extruder || previous.filament.type !== type || previous.filament.color !== color)) slots.set(key, null);
  }
  const ams_slots: [number, number] = [0, 0];
  for (const unit of units.values()) if (unit) ams_slots[unit.extruder] += unit.capacity;
  const loaded_filaments = [...slots.values()].flatMap((row) => row && (row.filament.external || units.get(row.ams)) ? [row.filament] : []);
  loaded_filaments.sort((a, b) => a.extruder - b.extruder || Number(a.external) - Number(b.external) || compare(a.type, b.type) || compare(a.color, b.color));
  return ams_slots.some((slots) => slots > 64) || loaded_filaments.length > 128
    ? { ams_slots: [0, 0], loaded_filaments: [] } : { ams_slots, loaded_filaments };
}
