import { useEffect, useState } from 'react';
import { mutation, responseError } from '../api/access';
import { useStore } from '../state/store';
import { isExternalTray } from '../multinozzle/ams';
import type { Tray } from './connections';

export type SlotRule = 'spool' | 'remembered' | 'exact' | 'type' | 'generic' | 'unassigned';
export interface SlotMatch {
  slot: string; label: string;
  tray: { extruder?: number; type: string; colour: string; name: string; filament_id: string };
  filament: string | null; catalog?: string; name: string; rule: SlotRule; colour: string;
  remaining: number | null; spool: string | null;
}
export interface PrinterSlots { online: boolean; updated: string; slots: SlotMatch[]; error?: string }

export const slotOf = (t: Tray) => isExternalTray(t.id) ? t.extruder === undefined ? 'ext' : t.extruder === 0 ? 'ext.L' : 'ext.R' : `ams${t.ams}.${t.slot}`;

export function matchedTrays(trays: Tray[], matches?: readonly SlotMatch[]): Tray[] {
  return trays.map((tray) => {
    if (tray.type || !isExternalTray(tray.id)) return tray;
    const match = matches?.find((m) => m.slot === slotOf(tray));
    return match?.filament ? { ...tray, type: match.tray.type, name: match.name } : tray;
  });
}

export const ruleText = (m: SlotMatch) => ({ spool: 'Your choice for this spool', remembered: 'Your choice', exact: 'Same filament',
  type: `Matched by type`, generic: 'Generic (not in My filaments)', unassigned: 'Material not reported by printer' })[m.rule];

const path = (printer: string, slot = '') => `/api/printer-slots/${encodeURIComponent(printer)}${slot && `/${encodeURIComponent(slot)}`}`;

export async function readPrinterSlots(printer: string): Promise<PrinterSlots> {
  const r = await fetch(path(printer));
  if (!r.ok) throw await responseError(r);
  return await r.json() as PrinterSlots;
}

export async function chooseSlot(printer: string, slot: string, filament: string | null): Promise<PrinterSlots> {
  const r = await mutation(path(printer, slot), filament
    ? { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ filament }) }
    : { method: 'DELETE' });
  if (!r.ok) throw await responseError(r);
  return await r.json() as PrinterSlots;
}

export function usePrinterSlots(printer: string | undefined): { slots?: PrinterSlots; error?: string; set(next: PrinterSlots): void } {
  const trays = useStore((s) => (printer ? JSON.stringify(s.printerConnections.status[printer]?.trays ?? null) : ''));
  const library = useStore((s) => s.profileLibraryRevision);
  const [state, setState] = useState<{ key: string; slots?: PrinterSlots; error?: string }>({ key: '' });
  const key = `${printer}\n${trays}\n${library}`;
  useEffect(() => {
    if (!printer) return;
    let live = true;
    readPrinterSlots(printer).then((slots) => live && setState({ key, slots }), (e: unknown) => live && setState({ key, error: e instanceof Error ? e.message : String(e) }));
    return () => { live = false; };
  }, [key]);
  const current = state.key.startsWith(`${printer}\n`) ? state : undefined;
  return { slots: current?.slots, error: current?.error, set: (slots) => setState({ key, slots }) };
}
