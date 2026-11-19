import type { PrinterStatus } from './connections.ts';
import { isActivePrint, printerBlockingIssue } from '../notifications/printer-events.ts';

type Detail = { kind: 'job'; text: string } | ({ kind: 'issue' } & NonNullable<ReturnType<typeof printerBlockingIssue>>);

type OverviewStatus =
  | { kind: 'offline'; lastSeen: string | undefined }
  | { kind: 'idle' | 'finished' | 'failed'; detail: Detail }
  | { kind: 'printing' | 'paused'; detail: Detail; progress: number | undefined; progressText: string };

function progressText(progress: number | undefined, seconds: number | undefined): string {
  const percent = progress === undefined ? undefined : `${Math.round(progress * 100)}%`;
  if (seconds === undefined || !Number.isFinite(seconds) || seconds < 0) return percent ?? '';
  const minutes = Math.round(seconds / 60);
  const remaining = minutes >= 60 ? `${Math.floor(minutes / 60)}h ${minutes % 60}m left` : `${minutes}m left`;
  return percent ? `${percent} · ${remaining}` : remaining;
}

export function overviewStatus(live: boolean, status: PrinterStatus | undefined): OverviewStatus {
  if (!live || !status?.online || status.state === 'offline' || status.state === 'connecting') {
    const lastSeen = status?.last_seen ?? (status?.online ? status.at : undefined);
    return { kind: 'offline', lastSeen };
  }
  const issue = printerBlockingIssue(status);
  const active = isActivePrint(status);
  const kind = status.state === 'paused' ? 'paused' : active ? 'printing'
    : status.state === 'finished' ? 'finished' : status.state === 'failed' || status.state === 'error' ? 'failed' : 'idle';
  const detail: Detail = issue ? { kind: 'issue', ...issue }
    : { kind: 'job', text: kind === 'idle' ? '' : status.file?.trim().split(/[\\/]/).at(-1) ?? '' };
  if (kind !== 'printing' && kind !== 'paused') return { kind, detail };
  const progress = status.progress !== undefined && Number.isFinite(status.progress)
    ? Math.max(0, Math.min(status.progress, 1)) : undefined;
  return { kind, detail, progress, progressText: progressText(progress, status.remaining_s) };
}
