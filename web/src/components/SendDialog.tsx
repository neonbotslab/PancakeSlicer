import { SEP } from '../separator';
import { preprintMaterialChecks } from '../multinozzle/material-checks';
import { useSlicedMapping } from '../multinozzle/sliced-mapping';
import { preprintWarnings } from '../multinozzle/preprint';
import { trayCode, type SendFilament } from '../multinozzle/ams';
import { extruderLabel, valueAt } from '../multinozzle/config';
import { nozzleReslice, sameMappedTrays, trayFeedsNozzle } from '../multinozzle/send-nozzle';
import { effectiveFlowCalibration, FLOW_CALIBRATION_OPTIONS, FLOW_CALIBRATION_PREFERENCE, flowCalibrationPreference, type FlowCalibrationMode } from '../flow-calibration';
import { preference } from '../saved-preferences';
import { DEMO } from '../demo/config';
import { nozzleCount } from '../profiles/view';
import { useEffect, useMemo, useReducer, useRef, useState, type ReactNode } from 'react';
import { SlotChip } from './SlotChip';
import { Skeleton } from './Skeleton';
import { printerModel } from '../printers/capabilities';
import { Check } from './Check';
import { confirmDialog } from './Ask';
import { Dropdown } from './Dropdown';
import { Modal, ModalBody, ModalFooter, ModalTitle } from './Modal';
import { Choice } from './kit';
import { canExport, download, exportFilename, outputKind, slice } from '../actions';
import { printerSend, usePrinterSend } from '../flow/transfer';
import { applyNozzleSlotChange, nozzleSlotChange } from '../flow/nozzle-reslice';
import { filamentColor } from '../plater/filamentColor';
import { effectiveConfig, getState, toast, useStore } from '../state/store';
import { profileInfo, selectionValues, variantOf } from '../profiles/view';
import { lineRead, loadedLine, tuningFor } from '../catalog/read';
import { ConnectionEditor } from '../printers/ConnectionEditor';
import { materialNote } from '../printers/slots';
import { feedOf, feedTrayKey, reportedTrayKey } from '../printers/feeds';
import { setupOf } from '../printers/device-slots';
import { connectionOf, connectionOutput, KIND_LABEL, presenceStatus, readSendPlan, readTrays, type SendPlanSlot, type Tray } from '../printers/connections';
import { printerCapabilities } from '../printers/capabilities';
import { noticeSend, openDevice, sendOutcome } from '../printers/sendOutcome';
import '../printers/printers.css';
import { slotFilaments } from '../profiles/slots';

const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const errorText = (e: unknown) => sentence(e instanceof Error ? e.message : String(e));
const setupName = (id: string) => profileInfo(getState(), 'machine', id).name || id;
function useFilamentLines(products: string[]) {
  const [, bump] = useReducer((x: number) => x + 1, 0), key = products.join('\n');
  useEffect(() => { const stops = products.filter(Boolean).map((p) => lineRead('filaments', p).subscribe(bump)); return () => stops.forEach((stop) => stop()); }, [key]); // eslint-disable-line react-hooks/exhaustive-deps
  return products.map((p) => p ? loadedLine('filaments', p) : undefined);
}

function closeSend(close: () => void) {
  const { value, context } = printerSend.machine.get();
  const t = context.transfer && (getState().printerConnections.transfers.find((x) => x.id === context.transfer!.id) ?? context.transfer);
  if (t && (value === 'sending' || value === 'sent')) noticeSend(t);
  if (value === 'sent' || value === 'failed') printerSend.reset();
  close();
}

export default function SendDialog({ close: dismiss }: { close: () => void }) {
  const close = () => closeSend(dismiss);
  const plate = useStore((s) => s.printer);
  const choices = useStore((s) => {
    const variant = variantOf(s, s.printer).id;
    return [s.printer, ...s.printerConnections.connections.filter((c) => c.printer && c.printer !== s.printer
      && !!variant && variantOf(s, c.printer).id === variant).map((c) => c.printer)].join('\n');
  }).split('\n');
  const [target, setTarget] = useState(plate);
  const printer = choices.includes(target) ? target : plate;
  const name = useStore(() => setupName(printer));
  const connection = useStore((s) => connectionOf(s.printerConnections, printer));
  const status = useStore((s) => s.printerConnections.status[printer]);
  const [editing, setEditing] = useState(false);
  const title = !connection || editing ? 'Printer connection' : 'Send to printer';
  const live = useStore((s) => s.printerConnections.live);
  const presence = connection && presenceStatus(live, connection, status);
  const head = <>
    <div className="send-target">
      {choices.length > 1
        ? <Dropdown className="combo" value={printer} aria-label="Printer" onValueChange={setTarget} options={choices.map((k) => ({ value: k, label: setupName(k) }))} />
        : <span className="send-target-name">{name}</span>}
      <span className="send-target-state status-text" data-tone={presence?.tone ?? 'offline'}>{presence?.label ?? 'Not connected'}</span>
      {connection && <button type="button" className="btn send-action" onClick={() => setEditing(!editing)}>{editing ? 'Done' : 'Connection'}</button>}
    </div>
    {(!connection || editing) && <ConnectionEditor key={printer} printer={printer} name={name} onSaved={() => setEditing(false)} />}
  </>;
  return (
    <Modal size="m" className="send-dialog" label={title} onClose={close}>
      <ModalTitle title={title} onClose={close} />
      {connection && !editing ? <SendView key={printer} close={close} printer={printer} head={head} edit={() => setEditing(true)} />
        : <ModalBody className="send-body">{head}</ModalBody>}
    </Modal>
  );
}

function SendView({ close, printer, head, edit }: { close: () => void; printer: string; head: ReactNode; edit(): void }) {
  const connection = useStore((s) => connectionOf(s.printerConnections, printer))!;
  const status = useStore((s) => s.printerConnections.status[printer]);
  const jobId = useStore((s) => s.job.id);
  const jobReady = useStore(canExport);
  const platePrinter = useStore((s) => s.printer);
  const live = useStore((s) => s.printerConnections.live);
  const plateIndex = useStore((s) => s.plateIndex);
  const output = useStore(outputKind);
  const kind = connection.kind, bambu = kind === 'bambu-lan';
  const fits = connectionOutput(kind) === output;
  const machineConfig = useStore((s) => effectiveConfig(s, 'machine'));
  const record = useStore((s) => setupOf(s, printer));
  const savedRecord = useStore((s) => s.records.printers.find((p) => p.id === printer));
  const capabilities = printerCapabilities(useStore((s) => printerModel(s, printer)) || connection.model || '');
  const elegoo = kind === 'elegoolink';
  const [leveling, setLeveling] = useState(kind !== 'elegoolink');
  const [flowCalibration, setFlowCalibration] = useState(() => flowCalibrationPreference(preference(FLOW_CALIBRATION_PREFERENCE)));
  const supportsAutoFlow = connection.auto_flow_calibration === true;
  const flowCali = effectiveFlowCalibration(flowCalibration, supportsAutoFlow);
  const [vibration, setVibration] = useState(true);
  const [timelapse, setTimelapse] = useState(false);
  const products = useStore((s) => slotFilaments(s).map((id) => profileInfo(s, 'filament', id).product ?? '').join('\n')).split('\n');
  const lines = useFilamentLines(products), variant = useStore((s) => variantOf(s, printer).id);
  const filamentKey = useStore((s) => JSON.stringify(slotFilaments(s).map((key, i) => {
    const config = effectiveConfig(s, 'filament', i), line = lines[i];
    return { key, type: valueAt(config.filament_type, 0), color: filamentColor(s, i + 1), name: products[i] || profileInfo(s, 'filament', key).name,
      vendor: valueAt(config.filament_vendor, 0), id: line && tuningFor(line, variant)?.source.filament_id, requiredHRC: Number(valueAt(config.required_nozzle_HRC, 0, '0')),
      printable: Number(valueAt(config.filament_printable, 0, '3')), compatibility: Number(valueAt(config.filament_extruder_compatibility, 0, '0')) };
  })));
  const resolvedTypes = (JSON.parse(useStore((s) => JSON.stringify(slotFilaments(s).map((_, i) => { const v = selectionValues(s, 'filament', i); return v.state === 'ready' ? valueAt(v.values.filament_type, 0) : null; })))) as (string | null)[]).map((t) => t ?? undefined);
  const count = useStore((st) => nozzleCount(st, printer)), multiNozzle = count > 1;
  const sliced = useSlicedMapping(jobId, bambu && jobReady);
  const filaments = useMemo(() => (JSON.parse(filamentKey) as SendFilament[]).map((filament, i) => {
    const nozzle = sliced?.mapping?.filament_map[i];
    const material = sliced?.mapping?.materials.find((row) => row.slot === i + 1);
    return { ...filament, ...material, hasFilamentSwitch: sliced?.mapping?.has_filament_switch,
      ...(multiNozzle && nozzle !== undefined && Number.isInteger(nozzle) && nozzle >= 1 && nozzle <= count ? { extruder: nozzle - 1 } : {}) };
  }), [filamentKey, sliced?.mapping, count, multiNozzle]);
  const liveTrays = live && status?.online ? status.trays : undefined;
  const [readTraysList, setReadTrays] = useState<Tray[] | null>(null);
  const trays = liveTrays ?? readTraysList;
  const [amsError, setAmsError] = useState('');
  const [plan, setPlan] = useState<SendPlanSlot[] | null>(null);
  const [planError, setPlanError] = useState('');
  const trayKey = JSON.stringify(trays?.map((t) => [t.id, t.ams, t.slot, t.kind, t.extruder, t.type, t.color, t.filament_id, t.spool]) ?? null);
  useEffect(() => {
    setPlan(null); setPlanError('');
    if (!bambu || !jobId || !jobReady) return;
    const request = new AbortController();
    readSendPlan(jobId, printer, request.signal).then((rows) => { if (!request.signal.aborted) setPlan(rows); }, (e) => {
      if (!request.signal.aborted) setPlanError(errorText(e));
    });
    return () => request.abort();
  }, [bambu, jobId, jobReady, printer, trayKey]);
  const feed = feedOf(record?.model);
  const trayKeyOf = (tray: Tray) => feedTrayKey(feed, reportedTrayKey(tray));
  const trayOf = (row: SendPlanSlot | undefined) => row?.tray ? trays?.find((t) => trayKeyOf(t) === feedTrayKey(feed, row.tray)) : undefined;
  const shown = (plan ?? []).map((row) => row.used ? trayOf(row)?.id ?? -1 : -1);
  const where = (row: SendPlanSlot) => { const t = trayOf(row); return t && trays ? trayCode(t, trays) : row.tray ?? ''; };
  const used = (plan ?? []).filter((row) => row.used);
  const tpuFlowNote = flowCali !== 'off' && used.some((row) => filaments[row.filament - 1]?.type.toUpperCase() === 'TPU');
  const nozzleIssues = multiNozzle ? used.flatMap((row) => {
    const tray = trayOf(row), filament = filaments[row.filament - 1];
    if (filament?.extruder === undefined) return [];
    const wrongNozzle = !!tray && !trayFeedsNozzle(tray, filament.extruder);
    if (tray && !wrongNozzle && row.state !== 'mismatch' && row.state !== 'ambiguous' && row.state !== 'remembered') return [];
    const otherTrays = new Set(used.filter((other) => other.slot !== row.slot).map((other) => feedTrayKey(feed, other.tray)));
    const candidates = (trays ?? []).filter((candidate, _, all) =>
      !otherTrays.has(trayKeyOf(candidate)) && all.filter((t) => trayKeyOf(t) === trayKeyOf(candidate)).length === 1);
    const recovery = nozzleReslice({ filament, trays: candidates, platePrinter, sendPrinter: printer });
    if (!wrongNozzle && !recovery) return [];
    const change = recovery?.kind === 'reslice' && savedRecord && trays
      ? nozzleSlotChange({ record: savedRecord, slot: row.slot, filament, trays, candidates, filaments, resolvedTypes }) : undefined;
    return [{ row, tray, filament, wrongNozzle, recovery, change }];
  }) : [];
  const unmapped = used.flatMap((row) => row.state === 'unassigned' ? [`Filament ${row.filament}'s slot isn't on a tray of this printer.`]
    : row.state === 'ambiguous' ? [`The printer reports conflicting details for filament ${row.filament}'s tray (${where(row)}). Wait for a consistent report.`]
    : row.state === 'mismatch' && !row.reported?.type ? [`Filament ${row.filament}'s tray (${where(row)}) ${row.reported ? 'is empty' : "isn't reported by the printer"}.`] : []);
  const mismatched = used.filter((row) => row.state === 'mismatch' && !!row.reported?.type);
  const sendState = usePrinterSend();
  const { transfer, error } = sendState.context;
  const busy = sendState.value === 'connecting' || sendState.value === 'sending';
  const [working, setWorking] = useState('');
  const activeSend = useRef<AbortController | null>(null);

  useEffect(() => () => {
    activeSend.current?.abort();
    activeSend.current = null;
  }, [jobId]);

  useEffect(() => {
    const current = printerSend.machine.get().context;
    if (current.jobId && (current.jobId !== jobId || current.target !== printer) && printerSend.machine.get().value !== 'sending') printerSend.reset();
  }, [jobId, printer]);
  useEffect(() => {
    setReadTrays(null); setAmsError('');
    if (!bambu || liveTrays) return;
    let live = true;
    readTrays(printer).then((list) => { if (live) setReadTrays(list); }, (e) => { if (live) setAmsError(errorText(e)); });
    return () => { live = false; };
  }, [bambu, printer, !!liveTrays]);

  const usedNumbers = new Set(used.map((row) => row.filament));
  const materials = bambu && record ? preprintMaterialChecks(record, status, filaments, shown, trays ?? [], usedNumbers) : [];
  const materialNotes = filaments.flatMap((f, i) => { const tray = trays?.find((t) => t.id === shown[i]), note = tray && materialNote(tray.type, f.name || 'This filament', resolvedTypes[i]); return note ? [`Filament ${i + 1}: ${note}`] : []; });
  const remembered = used.filter((row) => row.state === 'remembered').map((row) => `${where(row)} uses saved ${row.remembered?.type || row.expected.type}. The printer has not reported its material.`);
  const warnings = bambu && record ? [...preprintWarnings(record, machineConfig, status, filaments, shown, trays ?? [], usedNumbers), ...materials.filter((row) => !row.blocked).map((row) => row.message), ...materialNotes, ...remembered] : [];
  const materialBlocks = materials.filter((row) => row.blocked);
  const send = async (start: boolean) => {
    if (DEMO) return void toast('not_available_demo_mode');
    if (!jobId || activeSend.current || blocked || start && printBlocked) return;
    const request = new AbortController();
    activeSend.current = request;
    try { await prepareSend(start, request.signal); }
    finally { if (activeSend.current === request) activeSend.current = null; }
  };
  const prepareSend = async (start: boolean, signal: AbortSignal) => {
    if (!jobId) return;
    if (start && warnings.length && !await confirmDialog('Check printer setup', warnings.join('\n'), ['Print anyway', 'Cancel'])) return;
    if (start && mismatched.length && !await confirmDialog('Print from these trays?', mismatched.map((row) =>
      `Filament ${row.filament} (${where(row)}): the printer reports ${row.reported!.type} there; this print expects ${row.expected.type || 'another material'}.`).join('\n'), ['Print anyway', 'Cancel'])) return;
    if (signal.aborted) return;
    if (start && bambu) {
      try {
        const currentPlan = await readSendPlan(jobId, printer, signal);
        if (signal.aborted) return;
        if (JSON.stringify(currentPlan) !== JSON.stringify(plan)) {
          setPlan(currentPlan);
          toast('senddialog_failed', { detail: 'The printer slots changed. Check the tray mapping before sending.' });
          return;
        }
      } catch (error) {
        if (!signal.aborted) toast('senddialog_failed', { detail: errorText(error) });
        return;
      }
    }
    const current = getState(), currentStatus = current.printerConnections.status[printer];
    if (current.job.id !== jobId || !canExport(current)) {
      toast('export_stale_slice');
      return;
    }
    if (start && bambu && (liveTrays && (!current.printerConnections.live || !currentStatus?.online)
        || trays && current.printerConnections.live && currentStatus?.online && (liveTrays || currentStatus.trays)
          && !sameMappedTrays({ before: trays, after: currentStatus.trays ?? [], mapping: shown }))) {
      toast('senddialog_failed', { detail: 'The printer filament setup changed. Check the tray mapping before sending.' });
      return;
    }
    if (start && currentStatus?.online && ['printing', 'paused', 'busy'].includes(currentStatus.state)) {
      toast('senddialog_failed', { detail: 'The printer is busy. Wait for the current print to finish before starting another.' });
      return;
    }
    const options = elegoo ? { bed_leveling: leveling, timelapse } : !bambu ? {} : { bed_leveling: leveling, flow_cali_mode: flowCali, vibration_cali: vibration, timelapse: capabilities.timelapse && timelapse,
      ...(start && plan ? { expected_plan: plan.map((row) => row.proof) } : {}),
      ...(start && mismatched.length ? { override: mismatched.map((row) => row.slot) } : {}) };
    await printerSend.submit(printer, jobId, { start, filename: exportFilename(), ...options });
  };
  const handOff = async () => {
    if (!jobId) return;
    setWorking('connect');
    try { await window.orcaDesktop!.bambuConnect!(jobId, exportFilename()); toast('opened_bambu_connect'); }
    catch (e) { toast('senddialog_failed', { detail: errorText(e) }); } finally { setWorking(''); }
  };

  const mine = transfer && transfer.printer === printer && transfer.job === jobId ? transfer : null;
  const shownTransfer = useStore((s) => (mine && s.printerConnections.transfers.find((t) => t.id === mine.id)) || mine);
  const outcome = shownTransfer ? sendOutcome(shownTransfer) : busy ? { tone: 'progress' as const, title: `Sending to ${connection.name}`, detail: 'Connecting to the printer…' } : null;
  const running = status?.online && (status.state === 'printing' || status.state === 'paused' || status.state === 'busy');
  const blocked = !jobId || !jobReady ? 'Slice your plate first before sending it to the printer.' : !fits ? `${KIND_LABEL[kind]} takes .${connectionOutput(kind)} files; this printer profile exports .${output}.` : '';
  const needsNozzleMap = bambu && multiNozzle && (!sliced?.mapping || used.some((row) => filaments[row.filament - 1]?.extruder === undefined));
  const needsMap = needsNozzleMap || bambu && (!plan || unmapped.length > 0 || nozzleIssues.some((issue) => issue.wrongNozzle)
    || multiNozzle && used.some((row) => !trayOf(row)));
  const printBlocked = needsMap || running || materialBlocks.length > 0;
  const file = <p className="send-file" title={exportFilename()}><span>{exportFilename()}</span><span>Plate {plateIndex}</span></p>;

  if (outcome && (busy || sendState.value === 'sent' || mine?.state === 'failed' || mine?.state === 'cancelled')) {
    const t = shownTransfer, settled = !busy, failedSend = outcome.tone === 'error';
    const jobLine = settled && t?.outcome === 'printing' && status?.online ? status.state === 'busy' ? 'Preparing to print…'
      : status.state === 'printing' || status.state === 'paused' ? [status.state === 'paused' ? 'Paused' : '', status.progress != null ? `${Math.round(status.progress * 100)}%` : '',
        status.remaining_s ? `${Math.round(status.remaining_s / 60)} min left` : ''].filter(Boolean).join(SEP) : '' : '';
    const bar = !t ? 0 : t.state === 'uploading' ? t.sent / Math.max(1, t.total) : t.state === 'starting' ? 1
      : jobLine && status?.progress != null && (status.state === 'printing' || status.state === 'paused') ? status.progress : null;
    const meter = busy ? outcome.detail : jobLine;
    return <>
      <ModalBody className="send-body send-state">
        <div className="send-state-text" role={failedSend ? 'alert' : 'status'}>
          <h3 data-tone={outcome.tone}>{outcome.title}</h3>
          {file}
          {!busy && outcome.detail && <p>{outcome.detail}</p>}
          {failedSend && t?.recovery && <p className="send-recovery">{t.recovery}</p>}
        </div>
        {(bar != null || meter) && <div className="send-meter">
          {bar != null && <div className="progress"><div className="progress-fill" style={{ width: `${100 * bar}%` }} /></div>}
          {meter && <span>{meter}</span>}
        </div>}
      </ModalBody>
      <ModalFooter>
        {busy && t?.state !== 'starting' ? <button type="button" className="btn" disabled={t?.state !== 'uploading'} onClick={() => printerSend.cancel()}>Cancel upload</button> : busy ? <button type="button" className="btn-primary" onClick={close}>Done</button> : failedSend ? <>
          {t?.reason === 'credentials' && <button type="button" className="btn" onClick={() => { printerSend.reset(); edit(); }}>Connection settings</button>}
          <button type="button" className="btn-primary" disabled={!!blocked || !!t?.start && printBlocked} onClick={() => void send(!!t?.start)}>Retry</button>
        </> : <>
          <button type="button" className="btn" onClick={() => { close(); openDevice(printer); }}>Open in Device</button>
          <button type="button" className="btn-primary" onClick={close}>Done</button>
        </>}
      </ModalFooter>
    </>;
  }
  return <>
    <ModalBody className="send-body">
      {head}
      {file}
      {jobReady && needsNozzleMap && <div className={sliced?.error ? 'warning-text' : 'send-meta'} role={sliced?.error ? 'alert' : 'status'}>{sliced?.error ?? 'Checking nozzle settings from slice…'}</div>}
      {blocked && <div className="warning-text">{blocked}</div>}
      {bambu && fits && <div className="send-options" role="group" aria-label="Print options">
        <label className="modal-check"><Check checked={leveling} disabled={busy} onCheckedChange={setLeveling} />Bed leveling</label>
        <label className="modal-check"><Check checked={vibration} disabled={busy} onCheckedChange={setVibration} />Vibration calibration</label>
        {capabilities.timelapse && <label className="modal-check"><Check checked={timelapse} disabled={busy} onCheckedChange={setTimelapse} />Timelapse</label>}
        <div className="send-flow-calibration"><span>Flow calibration</span>
          <Choice<FlowCalibrationMode> label="Flow calibration" value={flowCali} disabled={busy} onChange={setFlowCalibration}
            options={supportsAutoFlow ? FLOW_CALIBRATION_OPTIONS : FLOW_CALIBRATION_OPTIONS.filter(({ id }) => id !== 'auto')}
            describedBy={tpuFlowNote ? 'send-flow-calibration-note' : undefined} />
        </div>
        {tpuFlowNote && <p id="send-flow-calibration-note" className="send-note">For TPU, Auto/On reuse previous or default values.</p>}
      </div>}
      {elegoo && fits && <div className="send-options" role="group" aria-label="Print options">
        <label className="modal-check"><Check checked={leveling} disabled={busy} onCheckedChange={setLeveling} />Bed leveling</label>
        <label className="modal-check"><Check checked={timelapse} disabled={busy} onCheckedChange={setTimelapse} />Timelapse</label>
      </div>}
      {bambu && fits && (
        <section className="send-section" aria-label="Filaments"><div className="send-section-head"><h3>Filaments</h3></div>
          {plan ? <ul className="send-filaments">{used.map((row) => {
            const f = filaments[row.filament - 1], t = trayOf(row);
            return <li key={row.slot}><SlotChip n={row.filament} colour={f?.color ?? row.expected.color} /><span className="send-filament-name" title={f?.name}>{f?.name || row.expected.type}</span>
              {f?.extruder !== undefined && <span className="send-meta">{extruderLabel(f.extruder, count, true)}</span>}
              {row.tray && <><span className="send-arrow" aria-hidden>→</span>{t && <span className="ams-swatch" style={{ background: t.color }} aria-hidden />}{where(row)}</>}</li>;
          })}</ul> : planError || amsError ? <p className="warning-text">{`Oops! Couldn't read your printer's slots: ${planError || amsError}`}</p>
            : <ul className="send-filaments" role="status" aria-busy="true" aria-label="Checking your printer's slots…">{filaments.map((f, i) => <li key={i}><SlotChip n={i + 1} colour={f.color} /><span className="send-filament-name">{f.name || f.type}</span><Skeleton width={96} height={12}/></li>)}</ul>}
          {unmapped.map((line) => <p key={line} className="warning-text">{line} <button type="button" className="link" onClick={() => { close(); openDevice(printer); }}>Change slots</button></p>)}
          {nozzleIssues.map(({ row, tray, filament, wrongNozzle, recovery, change }) => {
            const destination = change && trayCode(change.tray, trays ?? []);
            const selectedName = change?.replacement && (profileInfo(getState(), 'filament', change.replacement).name || change.replacement);
            return <div key={row.slot} className="send-nozzle-recovery">
              <p className="warning-text">Filament {row.filament} was sliced for the {extruderLabel(filament.extruder ?? 0, count, true).toLowerCase()}. {wrongNozzle
                ? tray?.extruder === undefined ? 'The printer did not report which nozzle this tray feeds.'
                  : `The selected tray feeds the ${extruderLabel(tray.extruder, count, true).toLowerCase()}.`
                : `Its assigned tray does not report the expected ${filament.type}.`}</p>
              {recovery?.kind === 'incompatible' && <p className="send-meta">This filament is incompatible with the {extruderLabel(recovery.side, count, true).toLowerCase()}.</p>}
              {recovery?.kind === 'select-printer' && <p className="send-meta">Select {setupName(printer)} in Prepare before slicing again.</p>}
              {destination && <p className="send-meta">Use {destination}{selectedName && ` with its saved ${selectedName} profile`}.</p>}
              {change ? <button type="button" className="btn send-action" disabled={busy || !!blocked || !!working} onClick={async () => {
                setWorking('nozzle');
                try {
                  if (selectedName && !await confirmDialog('Use the tray’s saved filament profile?', `This project will use ${selectedName} for filament ${row.filament} on ${destination} and slice again.`, ['Change nozzle and reslice', 'Cancel'])) return;
                  const current = getState(), status = current.printerConnections.status[printer];
                  if (current.job.id !== jobId || current.printer !== printer || !canExport(current)) return;
                  if (liveTrays && (!current.printerConnections.live || !status?.online
                    || !sameMappedTrays({ before: [change.tray], after: status.trays ?? [], mapping: [change.tray.id] }))) return;
                  if (!await applyNozzleSlotChange(current, change)) return;
                  close();
                  void slice();
                } catch (error) { toast('senddialog_failed', { detail: errorText(error) }); }
                finally { setWorking(''); }
              }}>Change nozzle and reslice</button> : <button type="button" className="link" onClick={() => { close(); openDevice(printer); }}>Change slots</button>}
            </div>;
          })}
        </section>
      )}
      {materialBlocks.map((row) => <div key={row.message} className="warning-text" role="alert">{row.message}</div>)}
      {warnings.map((warning) => <div key={warning} className="warning-text">{warning}</div>)}
      {running && <p className="send-note" role="status">{connection.name} is {status!.state === 'paused' ? 'paused mid-print' : 'printing'}. You can start a new print once it finishes, or use Upload only right now.</p>}
      {error && <div role="alert" className="warning-text">{sentence(error)}</div>}
    </ModalBody>
    <ModalFooter>
      {sendState.value === 'failed' && <button type="button" className="btn" onClick={() => void printerSend.retry()}>Reconnect</button>}
      {kind === 'bambu-connect' ? (window.orcaDesktop?.bambuConnect
        ? <button type="button" className="btn-primary" disabled={!!blocked || !!working} onClick={() => void handOff()}>Open in Bambu Connect</button>
        : <button type="button" className="btn-primary" disabled={!!blocked} onClick={() => void download()}>Download .gcode.3mf</button>) : <>
        <button type="button" className="btn" disabled={busy || !!blocked || !!working} onClick={() => void send(false)}>Upload only</button>
        <button type="button" className="btn-primary" disabled={busy || !!blocked || !!working || printBlocked} onClick={() => void send(true)}>Print</button>
      </>}
    </ModalFooter>
  </>;
}
