import { useMemo } from 'react';
import { Dropdown } from '../components/Dropdown';
import { FilamentColourMenu } from '../profiles/FilamentColourMenu';
import { toast, useStore } from '../state/store';
import { editSetup, setUnits, setupOf } from './device-slots';
import { feedOf, resizeStandardUnits, slotLabels, unitsOf } from './feeds';
import { NozzleSize } from '../profiles/NozzleSize';
import { ExtruderRows } from '../profiles/ExtruderRows';
import { useAskedValues } from '../profiles/view';

const edit = (...args: Parameters<typeof editSetup>) => { editSetup(...args).catch((e: Error) => toast('deviceslots_failed', { detail: e.message })); };

export function NozzlesCard({ printer }: { printer: string }) {
  useAskedValues({ type: 'machine', id: printer });
  return <section className="device-card device-nozzles"><h2>Nozzles</h2>
    <NozzleSize printerId={printer} label="Size"/>
    <ExtruderRows printerId={printer}/>
  </section>;
}

export function SlotsCard({ printer, connected }: { printer: string; connected: boolean }) {
  const setup = useStore((s) => setupOf(s, printer)), library = useStore((s) => s.records.filaments);
  const filaments = useMemo(() => library.filter((f) => !f.archived).map((f) => ({ value: f.id, label: f.name })).sort((a, b) => a.label.localeCompare(b.label)), [library]);
  if (!setup) return null;
  const model = setup.model, feed = feedOf(model), labels = slotLabels(setup, model, setup.slots.length);
  const units = feed ? unitsOf(feed, setup) : [];
  const standard = units.filter((unit) => unit.kind !== 'ams-ht').length, ht = units.length - standard;
  return <section className="device-card device-slot-map"><h2>Slots</h2>
    {feed && connected && <span className="device-slots-units"><span>{feed.unit}</span><span>{standard ? `${standard} attached` : 'None'}</span></span>}
    {feed && !connected && <label className="device-slots-units"><span>{feed.unit}</span>
      <Dropdown className="combo" aria-label={`${feed.unit} units`} value={String(standard)} onValueChange={(v) => setUnits(printer, resizeStandardUnits(units, Number(v), 0), feed.unit).catch((e: Error) => toast('deviceslots_failed_2', { detail: e.message }))}
        options={Array.from({ length: feed.maxUnits + 1 }, (_, n) => ({ value: String(n), label: n === 0 ? 'None' : feed.maxUnits === 1 ? 'Attached' : `${n} attached` }))}/></label>}
    {ht > 0 && <span className="device-slots-units"><span>AMS HT</span><span>{ht} attached</span></span>}
    {setup.slots.map((slot, i) => <div key={slot.id} className="device-slot-row">
      {labels[i].group && labels[i].group !== labels[i - 1]?.group && <div className="device-slot-group">{labels[i].group}</div>}
      <span className="device-slot-label"><FilamentColourMenu printer={printer} slot={i}/></span>
      <Dropdown className="combo" aria-label={`${labels[i].label} filament`} value={slot.filament} placeholder="Pick a filament…"
        options={[{ value: '', label: 'Pick a filament…' }, ...filaments]}
        onValueChange={(v) => edit(printer, (x) => ({ ...x, slots: x.slots.map((s) => s.id === slot.id ? { ...s, filament: v } : s) }))}/>
    </div>)}
  </section>;
}
