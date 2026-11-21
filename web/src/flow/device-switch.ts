import type { SendSubmission, Transfer } from './transfer';

export const OPEN_DEVICE_AFTER_PRINT = 'orca.web.openDeviceAfterPrint';
export const OPEN_DEVICE_DELAY = 'orca.web.openDeviceDelay';
export const openDeviceAfterPrint = (value: unknown) => typeof value === 'boolean' ? value : true;
export const openDeviceDelay = (value: unknown) => typeof value === 'number' && Number.isFinite(value)
  ? Math.max(0, Math.min(60, Math.round(value))) : 5;

interface Clock {
  now(): number;
  after(ms: number, callback: () => void): () => void;
}

type Accepted = { attempt: symbol; job: string; printer: string; transferId: string };
type State =
  | { kind: 'idle' }
  | { kind: 'waiting'; attempt: symbol; job: string; printer: string; transferId: string | null; deadline: number; delay: number }
  | ({ kind: 'countdown'; deadline: number } & Accepted);

const CONFIRMATION_TIMEOUT = 120_000;

export function createDeviceSwitch({ clock, preferences, show, open }: {
  clock: Clock;
  preferences(): { enabled: unknown; delay: unknown };
  show(seconds: number | null): void;
  open(target: Pick<Accepted, 'printer' | 'transferId'>): void;
}) {
  let state: State = { kind: 'idle' };
  let clearTimer = () => {};
  let visibleSeconds: number | null = null;

  function showCountdown(seconds: number | null) {
    if (visibleSeconds === seconds) return;
    visibleSeconds = seconds;
    show(seconds);
  }

  function cancel() {
    clearTimer();
    clearTimer = () => {};
    state = { kind: 'idle' };
    showCountdown(null);
  }

  function schedule(ms: number, callback: () => void) {
    clearTimer();
    const scheduled = state;
    clearTimer = clock.after(ms, () => { if (state === scheduled) callback(); });
  }

  function tick() {
    if (state.kind !== 'countdown') return;
    const remaining = state.deadline - clock.now();
    if (remaining <= 0) {
      const { printer, transferId } = state;
      cancel();
      open({ printer, transferId });
    } else {
      const seconds = Math.ceil(remaining / 1000);
      schedule(remaining - (seconds - 1) * 1000, tick);
      showCountdown(seconds);
    }
  }

  return {
    submission(event: SendSubmission) {
      if (event.type === 'submitted') {
        cancel();
        if (!event.start) return;
        const { enabled, delay } = preferences();
        if (!openDeviceAfterPrint(enabled)) return;
        const { attempt, job, printer } = event;
        state = { kind: 'waiting', attempt, job, printer, transferId: null, delay: openDeviceDelay(delay), deadline: clock.now() + CONFIRMATION_TIMEOUT };
        schedule(CONFIRMATION_TIMEOUT, cancel);
      } else if (state.kind !== 'idle' && state.attempt === event.attempt) {
        if (event.type === 'aborted') cancel();
        else if (state.kind === 'waiting' && state.transferId === null) {
          const remaining = state.deadline - clock.now();
          if (remaining <= 0 || !event.transferId) cancel();
          else {
            state = { ...state, transferId: event.transferId };
            schedule(remaining, cancel);
          }
        }
      }
    },
    observe(transfers: readonly Transfer[]) {
      if (state.kind === 'idle' || state.transferId === null) return;
      const { transferId, job, printer, attempt } = state;
      const transfer = transfers.find((t) => t.id === transferId && t.job === job && t.printer === printer);
      if (!transfer) return;
      if (!transfer.start || transfer.state === 'failed' || transfer.state === 'cancelled' || transfer.outcome === 'failed' ||
        transfer.job_end === 'failed' || transfer.job_end === 'cancelled' ||
        state.kind === 'countdown' && (transfer.state !== 'done' || transfer.outcome !== 'printing')) {
        cancel();
      } else if (state.kind === 'waiting' && transfer.state === 'done' && transfer.outcome === 'printing') {
        const now = clock.now();
        if (now >= state.deadline) { cancel(); return; }
        state = { kind: 'countdown', attempt, job, printer, transferId, deadline: now + state.delay * 1000 };
        tick();
      }
    },
    cancel,
  };
}
