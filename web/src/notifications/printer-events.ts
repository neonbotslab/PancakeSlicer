import type { PrinterConnection, PrinterStatus } from '../printers/connections.ts';
import { bodyFacts, printerTag, type BrowserNotice } from './policy.ts';

type PrintJob = {
  id: string | undefined;
  file: string | undefined;
  startedAt: number | undefined;
};

type PrintActivity =
  | { kind: 'idle' }
  | { kind: 'active'; job: PrintJob; paused: boolean }
  | { kind: 'terminal'; job: PrintJob };

type Snapshot = {
  at: number | undefined;
  activity: PrintActivity;
  blockers: ReadonlySet<string>;
  monitoringGap: boolean;
};

type Blocker = { code: string; reason: string | undefined };

const ATTENTION = 'Printer needs attention';
const RUNOUT = 'Filament ran out';
const FEED = 'Filament feed problem';
const CLOG = 'Nozzle clogged';
const DOOR = 'Door open';
const SPAGHETTI = 'Spaghetti detected';

const blockingReasons: Readonly<Record<string, string | undefined>> = {
  '03008015': RUNOUT,
  '03004008': 'AMS filament change failed',
  '03004006': CLOG,
  '03008016': CLOG,
  '03001A0000020002': CLOG,
  '0300404B': DOOR,
  '0300800F': DOOR,
  '03008042': DOOR,
  '0300804B': 'Laser safety window open',
  '0300960000010001': DOOR,
  '03008003': SPAGHETTI,
  '0C008042': SPAGHETTI,
  '12008005': 'Filament not inserted',
  '12028005': 'Filament not inserted',
  '12038005': 'Filament not inserted',
  '12FF8005': 'Filament not inserted',
};

const stoppedReasons: Readonly<Record<string, string | undefined>> = {
  '0C00030000030007': 'Possible first-layer defect',
  '0C00030000030008': 'Possible spaghetti defect',
  '0C0003000003001B': 'Possible spaghetti defect',
  '0C00C004': 'Possible spaghetti defect',
};

const nonblockingCodes = new Set([
  '00000000', '0300400C', '03008000', '03008001', '0300800C', '03008013',
  '03008028', '03008064', '0C008016', '0C008018', '0C008034',
  '0C00010000020008', '0C00030000010009', '0C00030000020001',
  '0C00030000020002', '0C00030000020004', '0C00030000020005', '0C0003000002000F',
  '0300AB0000020003', '05008092', '1000C001', '1000C002', '1000C003',
]);

const amsUnit = /^(?:(?:07|18)0[0-7]|120[0-3])$/;
const externalSpool = /^(?:07|12|18)F[EF]$/;

function filamentReason(code: string): string | undefined {
  const unit = code.slice(0, 4);
  const fault = code.slice(4);
  if (amsUnit.test(unit)) {
    if (/^(?:8011|2[0-3]000002000[15]|700000020007)$/.test(fault)) return RUNOUT;
    if (/^(?:8007|7[0-3]0000020003)$/.test(fault)) return 'Filament extrusion failed';
    if (/^(?:800[3456]|801[03]|802[78]|7[0-3]000002000[12456])$/.test(fault)) return FEED;
  }
  if (externalSpool.test(unit)) {
    if (/^(?:8011|200000020001)$/.test(fault) || /^07F[EF][8C]030$/.test(code)) return RUNOUT;
    if (/^(?:800[45D]|801[03])$/.test(fault)) return FEED;
  }
  return undefined;
}

function normalizeCode(value: string): string {
  return value.trim().toUpperCase().replace(/^HMS[_ -]/, '').replaceAll(/[-_]/g, '');
}

function blockersFor(status: PrinterStatus, hasPrint: boolean, stopped: boolean): Blocker[] {
  const codes = [...new Set((status.messages ?? []).map(({ code }) => normalizeCode(code)))].sort();
  const blockers: Blocker[] = [];
  for (const code of codes) {
    if (nonblockingCodes.has(code) || (amsUnit.test(code.slice(0, 4)) && code.slice(4) === 'C008')) continue;
    const stoppedReason = stoppedReasons[code];
    const reason = blockingReasons[code] ?? filamentReason(code) ?? (stopped ? stoppedReason : undefined);
    if (reason || (!stoppedReason && (/^[0-9A-F]{8}000[12][0-9A-F]{4}$/.test(code)
        || (hasPrint && /^[0-9A-F]{4}[48][0-9A-F]{3}$/.test(code))))) {
      blockers.push({ code, reason });
    }
  }
  return blockers;
}

function reasonFor(blockers: Blocker[]): string | undefined {
  const blocker = blockers.find(({ reason }) => reason !== undefined) ?? blockers[0];
  if (!blocker) return undefined;
  const { code, reason } = blocker;
  return reason ?? bodyFacts([ATTENTION, code.length === 16 ? `${code.slice(0, 8)}-${code.slice(8)}` : code]);
}

function blockerIdentity({ code, reason }: Blocker): string {
  if (reason === DOOR || reason === CLOG || reason === SPAGHETTI) return reason;
  if (reason === RUNOUT && (amsUnit.test(code.slice(0, 4)) || externalSpool.test(code.slice(0, 4))))
    return `${reason}:${code.slice(0, 4)}`;
  return code;
}

function timestamp(value: string | undefined): number | undefined {
  const at = Date.parse(value ?? '');
  return Number.isFinite(at) && at > 0 ? at : undefined;
}

function differentJob(a: PrintJob, b: PrintJob): boolean {
  return (a.id !== undefined && b.id !== undefined && a.id !== b.id)
    || (a.file !== undefined && b.file !== undefined && a.file !== b.file)
    || (a.startedAt !== undefined && b.startedAt !== undefined && a.startedAt !== b.startedAt);
}

function fileName(file: string | undefined): string | undefined {
  return file?.split(/[\\/]/).at(-1);
}

function duration(startedAt: number | undefined, endedAt: number | undefined): string | undefined {
  if (startedAt === undefined || endedAt === undefined || endedAt < startedAt) return undefined;
  const seconds = Math.floor((endedAt - startedAt) / 1000);
  if (seconds < 60) return seconds === 0 ? '<1 s' : `${seconds} s`;
  const minutes = Math.floor(seconds / 60);
  return minutes < 60 ? `${minutes} min` : `${Math.floor(minutes / 60)} h ${minutes % 60} min`;
}

export class PrinterNotificationTracker {
  private readonly snapshots = new Map<string, Snapshot>();
  private nextNotice = 0;

  observe(connections: readonly PrinterConnection[], statuses: readonly PrinterStatus[]): BrowserNotice[] {
    const notices: BrowserNotice[] = [];
    const statusByPrinter = new Map(statuses.map((status) => [status.printer, status]));
    const keys = new Set<string>();
    for (const connection of connections) {
      if (connection.kind !== 'bambu-lan' || !connection.printer) continue;
      const key = JSON.stringify([connection.id, connection.printer]);
      keys.add(key);
      const status = statusByPrinter.get(connection.printer);
      if (!status?.online || status.state === 'offline' || status.state === 'connecting') {
        const previous = this.snapshots.get(key);
        if (previous?.activity.kind === 'active') previous.monitoringGap = true;
        continue;
      }
      const notice = this.update(key, connection, status);
      if (notice) notices.push(notice);
    }
    for (const key of this.snapshots.keys()) {
      if (!keys.has(key)) this.snapshots.delete(key);
    }
    return notices;
  }

  private update(key: string, connection: PrinterConnection, status: PrinterStatus): BrowserNotice | undefined {
    const previous = this.snapshots.get(key);
    const at = timestamp(status.at);
    if (at !== undefined && previous?.at !== undefined && at < previous.at) return undefined;

    const { state } = status;
    const active = state === 'printing' || state === 'paused'
      || (state === 'busy' && /^(?:PREPARE|SLICING)(?:, error [0-9A-F]{8})?$/.test(status.detail ?? ''));
    const terminal = state === 'finished' || state === 'failed';
    const reportedJob: PrintJob = {
      id: status.job?.id.trim() || undefined,
      file: status.file?.trim() || undefined,
      startedAt: timestamp(status.started_at),
    };
    const previousActivity = previous?.activity;
    if (active && state !== 'busy' && previousActivity?.kind === 'terminal'
        && reportedJob.startedAt !== undefined && reportedJob.startedAt === previousActivity.job.startedAt
        && !differentJob(previousActivity.job, reportedJob)) return undefined;
    const sameActive = previousActivity?.kind === 'active' && !differentJob(previousActivity.job, reportedJob)
      ? previousActivity : undefined;
    const previousJob = sameActive?.job ?? (terminal && previousActivity?.kind === 'terminal'
      && !differentJob(previousActivity.job, reportedJob) ? previousActivity.job : undefined);
    const job: PrintJob = {
      id: reportedJob.id ?? previousJob?.id,
      file: reportedJob.file ?? previousJob?.file,
      startedAt: reportedJob.startedAt ?? previousJob?.startedAt,
    };
    const blockers = blockersFor(status, active || (terminal && sameActive !== undefined), state === 'paused' || state === 'failed');
    this.snapshots.set(key, {
      at: at ?? previous?.at,
      activity: active ? { kind: 'active', job, paused: state === 'paused' }
        : terminal ? { kind: 'terminal', job } : { kind: 'idle' },
      blockers: new Set(blockers.map(blockerIdentity)),
      monitoringGap: active && sameActive !== undefined && previous?.monitoringGap === true
        && (at === undefined || at === previous.at),
    });
    if (!previous) return undefined;

    const newBlockers = blockers.filter((blocker) => !previous.blockers.has(blockerIdentity(blocker)));
    const attention = reasonFor(newBlockers);
    const tag = printerTag(connection.id);
    const notice = (title: string, body: string): BrowserNotice => ({
      id: `${tag}:${++this.nextNotice}`, title, body, tag,
      destination: { kind: 'device', printer: connection.printer },
    });

    if (state === 'finished' && sameActive && attention === undefined) {
      const elapsed = previous.monitoringGap ? undefined : duration(job.startedAt, at);
      return notice('Print finished', bodyFacts([fileName(job.file), connection.name, elapsed]));
    }
    const reason = attention ?? reasonFor(blockers);
    if (state === 'failed' && sameActive) {
      return notice('Print failed', bodyFacts([fileName(job.file), reason ?? 'The printer did not report a reason']));
    }
    if (state === 'paused' && (previousActivity?.kind !== 'active' || !previousActivity.paused)) {
      const codes = status.messages?.map(({ code }) => normalizeCode(code));
      const pauseReason = codes?.includes('03008013') ? 'Paused by user'
        : codes?.includes('03008001') ? 'Pause command in the print file' : 'Waiting to resume';
      return notice('Print paused', reason ?? pauseReason);
    }
    return attention === undefined ? undefined : notice(ATTENTION, attention);
  }
}
