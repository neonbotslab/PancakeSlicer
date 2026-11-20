export type Destination =
  | { kind: 'preview'; projectId: string; plate: number; jobId: string; signature: string }
  | { kind: 'slice-error'; projectId: string; plate: number; attempt: string; error: string; code?: string }
  | { kind: 'device'; printer: string }
  | { kind: 'focus' };

export type BrowserNotice = {
  id: string;
  title: string;
  body: string;
  tag: string;
  destination: Destination;
};

export function shouldNotify({ hidden, enabled, permission, supported }: {
  hidden: boolean;
  enabled: boolean;
  permission: NotificationPermission | 'unsupported';
  supported: boolean;
}): boolean {
  return hidden && enabled && permission === 'granted' && supported;
}

export function projectTag(id: string): string {
  return `orca-project:${id}`;
}

export function printerTag(id: string): string {
  return `orca-printer:${id}`;
}

export function eligibleSliceOffer({ elapsedMs, wasHidden }: { elapsedMs: number; wasHidden: boolean }): boolean {
  return wasHidden || elapsedMs >= 10_000;
}

export function bodyFacts(parts: readonly (string | null | undefined)[]): string {
  return parts.map((part) => part?.trim()).filter(Boolean).join(' · ');
}

function isId(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0;
}

export function validateDestination(value: unknown): Destination | null {
  if (!value || typeof value !== 'object' || !('kind' in value)) return null;
  if (value.kind === 'focus') return { kind: 'focus' };
  if (value.kind === 'device' && 'printer' in value && isId(value.printer)) {
    return { kind: 'device', printer: value.printer };
  }
  if (!('projectId' in value) || !isId(value.projectId)
      || !('plate' in value) || typeof value.plate !== 'number'
      || !Number.isSafeInteger(value.plate) || value.plate < 1) return null;
  const { projectId, plate } = value;
  if (value.kind === 'preview' && 'jobId' in value && isId(value.jobId) && 'signature' in value && isId(value.signature)) {
    return { kind: 'preview', projectId, plate, jobId: value.jobId, signature: value.signature };
  }
  if (value.kind === 'slice-error' && 'attempt' in value && isId(value.attempt) && 'error' in value && typeof value.error === 'string') {
    return { kind: 'slice-error', projectId, plate, attempt: value.attempt, error: value.error,
      ...('code' in value && typeof value.code === 'string' ? { code: value.code } : {}) };
  }
  return null;
}
