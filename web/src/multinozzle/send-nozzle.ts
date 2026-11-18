import type { Tray } from '../printers/connections';
import type { SendFilament } from './ams';
import { isNozzleSide, type NozzleSide } from './printer-filament-context.ts';

export const trayFeedsNozzle = (tray: Tray, extruder: number | undefined) =>
  extruder === undefined || tray.extruder === extruder;

export function sameMappedTrays({ before, after, mapping }: { before: Tray[]; after: Tray[]; mapping: number[] }): boolean {
  return mapping.filter((id) => id >= 0).every((id) => {
    const previous = before.find((tray) => tray.id === id), current = after.find((tray) => tray.id === id);
    return !!previous && !!current && previous.ams === current.ams && previous.slot === current.slot
      && previous.extruder === current.extruder && previous.kind === current.kind
      && previous.type.trim().toUpperCase() === current.type.trim().toUpperCase()
      && previous.color.toUpperCase() === current.color.toUpperCase() && previous.filament_id === current.filament_id;
  });
}

export function nozzleReslice({ filament, trays, platePrinter, sendPrinter }: {
  filament: Pick<SendFilament, 'type' | 'extruder' | 'printable' | 'compatibility'>;
  trays: Tray[]; platePrinter: string; sendPrinter: string;
}): { kind: 'reslice' | 'select-printer' | 'incompatible'; side: NozzleSide } | undefined {
  const { extruder, type, printable = 3, compatibility = 0 } = filament;
  if (!isNozzleSide(extruder) || !type) return undefined;
  const side = extruder === 0 ? 1 : 0;
  if (!trays.some((tray) => tray.extruder === side && tray.type.trim().toUpperCase() === type.trim().toUpperCase())) return undefined;
  if (!(printable & (1 << side)) || ((compatibility >>> (side * 3)) & 7) === 1) return { kind: 'incompatible', side };
  return { kind: platePrinter === sendPrinter ? 'reslice' : 'select-printer', side };
}
