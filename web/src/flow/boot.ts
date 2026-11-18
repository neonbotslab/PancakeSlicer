import { getState, setState, subscribeStore, toast } from '../state/store';
import { documentGeneration, onLeaveDocument } from '../state/history';
import { plateSignature } from '../slice-input';
import { followJob, job, jobTerminal, type TrackedJob } from './job';
import { project } from './project';
import { probeEngine } from './engine-health';
import { startProfileEdits } from './profile-edit';
import { initialiseRecovery, readRecovery, recoverySignature, rememberRecovery } from './recovery';
import { restorePrinterSend, printerSend } from './transfer';
import { startSliceTracking } from './slice';
import { loadSource } from './viewer';

let adoptTracking = () => {};
let cancelNotification = () => {};
let started = false, active: ReturnType<typeof followJob> | null = null;
export async function retryTrackedJob() {
  try { adoptTracking(); await active?.retry(); } catch (e) { toast(e instanceof Error ? e.message : 'Retry failed'); }
}
export async function cancelTrackedJob() {
  cancelNotification();
  try { await active?.cancel(); } catch (e) { toast(e instanceof Error ? e.message : 'Cancel failed'); }
}
export async function startFlows() {
  if (started) return;
  started = true;
  try { await initialiseRecovery(); } catch (error) { started = false; toast(error instanceof Error ? error.message : 'Oops! Couldn\'t load the recovery files for this project.'); return; }
  const { beginSliceNotice } = await import('../notifications/slice-events');
  const recovery = readRecovery();
  let seenId: string | undefined, generation = documentGeneration();
  const watch = (tracked: TrackedJob, seq = -1, restoring = false) => {
    const api = getState().api;
    if (!api || api.mock) return;
    active?.stop(); seenId = tracked.id;
    let recovering = restoring;
    let notification: ReturnType<typeof beginSliceNotice> | null = null;
    const beginNotification = () => {
      notification?.cancel();
      const s = getState();
      notification = beginSliceNotice({ projectId: tracked.projectId, generation: documentGeneration(), plate: s.plateIndex, file: s.project?.name ?? s.objects[0]?.name ?? '' });
      setState({ job: { ...s.job, notificationKey: notification.key } });
    };
    adoptTracking = () => { recovering = true; beginNotification(); };
    cancelNotification = () => notification?.cancel();
    if (restoring) beginNotification();
    const ownGeneration = documentGeneration(), ownProject = tracked.projectId;
    const current = () => ownGeneration === documentGeneration() && project.get().context.id === ownProject && (!recovering || getState().job.state !== 'canceled');
    active = followJob(api, tracked, { machine: job, seq,
      checkpoint: (last, stage) => {
        if (!current()) return;
        const tracked = job.get().context.job;
        if (!tracked) return;
        seenId = tracked.id;
        rememberRecovery({ job: { ...tracked, stage, seq: last } });
      },
      progress: (p) => {
        if (!current() || !recovering) return;
        const id = job.get().context.job?.id;
        setState((s) => ({ job: { ...s.job, id, state: 'running', pct: p.pct, stage: p.stage } }));
      },
      done: (status) => {
        if (!current()) return;
        rememberRecovery({ job: undefined });
        if (!recovering) return;
        const signature = plateSignature(getState());
        const key = notification?.key;
        setState({ job: { id: status.id, state: 'done', pct: 100, stage: 'done', status, notificationKey: key,
          plateSignature: recoverySignature(signature) === tracked.signature ? signature : undefined, stale: recoverySignature(signature) !== tracked.signature } });
        notification?.ready(getState().job);
      },
      failed: (message, status) => {
        if (!current()) return;
        if (recovering) {
          setState((s) => ({ job: { ...s.job, state: 'failed', error: message } }));
          if (status?.state === 'failed') notification?.failed(status.id, message);
          else notification?.cancel();
        }
        toast(message, { label: 'Retry', run: () => void retryTrackedJob() }, 15_000);
      },
    });
  };
  const saved = recovery.job;
  if (saved && saved.projectId === project.get().context.id && getState().api && !getState().api?.mock) {
    seenId = saved.id;
    setState({ job: { id: saved.id, state: 'running', pct: 0, stage: `Reconnecting (${saved.stage})` } });
    watch(saved, saved.seq, true);
  }
  const observe = () => {
    const s = getState();
    if (generation !== documentGeneration()) { generation = documentGeneration(); seenId = undefined; active?.stop(); active = null; job.send({ type: 'RESET' }); }
    if (s.job.id && s.job.id !== seenId && s.job.state === 'running') {
      watch({ id: s.job.id, projectId: project.get().context.id,
        signature: recoverySignature(s.job.plateSignature ?? plateSignature(s)) });
    }
    if (s.job.id === job.get().context.job?.id) {
      if (s.job.state === 'canceled') void cancelTrackedJob();
      if (s.job.stale && job.get().value === 'done' && s.job.status)
        job.send({ type: 'STATUS', status: { ...s.job.status, state: 'stale' } });
    }
  };
  subscribeStore(observe); observe();
  project.subscribe(({ context }) => rememberRecovery({ projectId: context.id }));
  rememberRecovery({ projectId: project.get().context.id });
  onLeaveDocument(() => {
    if (active && !jobTerminal(job.get().value)) void active.cancel().catch(() => {});
    active?.stop(); active = null; seenId = undefined;
    if (!jobTerminal(job.get().value)) job.send({ type: 'RESET' });
    rememberRecovery({ job: undefined, send: undefined });
    printerSend.reset(); loadSource(null);
  });
  startSliceTracking(); startProfileEdits(); restorePrinterSend(); void probeEngine();
}
