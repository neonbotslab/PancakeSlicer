import { randomId } from '../random-id';
import { documentGeneration } from '../state/history';
import { getState, setState, type JobView } from '../state/store';
import { project, openProject } from '../flow/project';
import { plateJob, selectPlate, setPlateJob } from '../plater/plates';
import { summarize } from '../flow/slice';
import { spacedTime } from '../preview/format';
import { plateSignature } from '../slice-input';
import { recoverySignature } from '../flow/recovery';
import { sliceFailure } from '../notice-text';
import { bodyFacts, eligibleSliceOffer, projectTag, type Destination } from './policy';
import { offerNotifications, publishNotice, setNotificationNavigator } from './browser';

interface SliceScope { projectId: string; generation: number; plate: number; file: string }
const attempts = new Map<number, string>();
let errorSequence = 0;

export function beginSliceNotice(scope: SliceScope) {
  const key = randomId();
  attempts.set(scope.plate, key);
  const started = performance.now();
  let hidden = document.visibilityState === 'hidden', settled = false;
  const observe = () => { hidden ||= document.visibilityState === 'hidden'; };
  document.addEventListener('visibilitychange', observe);
  const valid = () => scope.projectId === project.get().context.id && scope.generation === documentGeneration() && attempts.get(scope.plate) === key;
  const settle = () => {
    if (settled) return false;
    settled = true;
    observe();
    document.removeEventListener('visibilitychange', observe);
    return valid();
  };
  return {
    key,
    cancel() { settle(); },
    ready(job: JobView) {
      if (!settle() || !job.id || !job.plateSignature) return;
      const current = plateJob(getState(), scope.plate);
      if (current.id !== job.id) return;
      setPlateJob(scope.plate, { ...current, ...job, notificationKey: key, notificationPending: false, stale: current.stale });
      if (eligibleSliceOffer({ elapsedMs: performance.now() - started, wasHidden: hidden })) offerNotifications(key);
      const summary = summarize(job);
      void publishNotice({ id: key, title: 'Slice ready', body: bodyFacts([scope.file,
        summary.timeS != null && summary.timeS >= 0 ? spacedTime(summary.timeS) : undefined,
        summary.grams != null && summary.grams >= 0 ? `${summary.grams.toFixed(1)} g` : undefined]),
        tag: projectTag(scope.projectId), destination: { kind: 'preview', projectId: scope.projectId, plate: scope.plate, jobId: job.id, signature: recoverySignature(job.plateSignature) } });
    },
    failed(jobId: string | undefined, error: string) {
      if (!settle()) return;
      const current = plateJob(getState(), scope.plate);
      if (current.notificationKey !== key && (!jobId || current.id !== jobId)) return;
      setPlateJob(scope.plate, { ...current, state: 'failed', error, notificationKey: key, notificationPending: false });
      void publishNotice({ id: key, title: 'Slice failed', body: bodyFacts([scope.file, sliceFailure(error).title.replace(/^Oops!\s*/, '')]),
        tag: projectTag(scope.projectId), destination: { kind: 'slice-error', projectId: scope.projectId, plate: scope.plate, attempt: key, error } });
    },
  };
}

export function notifyExported(file: string, projectId: string) {
  const id = randomId();
  if (project.get().context.id === projectId) setState((s) => ({ job: { ...s.job, exported: file, exportNotificationKey: id } }));
  void publishNotice({ id, title: 'Export ready', body: file,
    tag: projectTag(projectId), destination: { kind: 'focus' } });
}

async function navigate(destination: Destination) {
  if (destination.kind === 'focus') return;
  if (destination.kind === 'device') {
    setState({ tab: 'device', appDialog: null, devicePrinter: destination.printer });
    return;
  }
  if (project.get().context.id !== destination.projectId) {
    await openProject(destination.projectId);
    if (project.get().context.id !== destination.projectId) return;
  }
  if (destination.kind === 'slice-error') {
    setState({ appDialog: null, tab: 'prepare', notificationError: { attempt: destination.attempt, error: destination.error },
      sliceErrorExpanded: { attempt: destination.attempt, sequence: ++errorSequence } });
    return;
  }
  const s = getState();
  if (destination.plate !== s.plateIndex && !s.projectPlates.some((p) => p.index === destination.plate)) return;
  selectPlate(destination.plate);
  if (getState().plateIndex !== destination.plate) return;
  const job = getState().job;
  if (job.state === 'done' && job.id !== destination.jobId) {
    setState({ tab: 'preview', appDialog: null, notificationError: null });
    return;
  }
  if (job.id !== destination.jobId || job.state !== 'done') {
    if (job.state !== 'idle' || recoverySignature(plateSignature(getState())) !== destination.signature) return;
    const status = await getState().api?.status(destination.jobId);
    if (status?.state !== 'done' || project.get().context.id !== destination.projectId || getState().plateIndex !== destination.plate
        || recoverySignature(plateSignature(getState())) !== destination.signature || getState().job.state !== 'idle') return;
    setState({ job: { id: destination.jobId, state: 'done', pct: 100, stage: 'done', status, plateSignature: plateSignature(getState()) } });
  }
  setState({ appDialog: null, tab: 'preview', sliceErrorExpanded: null, notificationError: null });
}

export function startNotificationNavigation(ready: Promise<unknown>) {
  setNotificationNavigator((destination) => { void ready.then(() => navigate(destination)).catch(() => {}); });
}
