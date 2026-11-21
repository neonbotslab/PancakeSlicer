import { onLiveEvent } from '../live-events';
import { createServiceDocument, type JsonValue } from './service-document';
import { requireWorkspaceService } from './service';
export type SavedPreferences = Record<string, JsonValue>;
const preferences = createServiceDocument<SavedPreferences>('/api/preferences', 'PUT', 'preferences', requireWorkspaceService);
let watching = false;
export const servicePreferencesSnapshot = preferences.snapshot;
export const subscribeServicePreferences = preferences.subscribe;
export async function hydrateServicePreferences() {
  await requireWorkspaceService();
  if (!watching) {
    watching = true;
    onLiveEvent('preferences', (data) => {
      let event: unknown;
      try { event = JSON.parse(data); } catch { return; }
      if (typeof event === 'object' && event !== null && 'revision' in event && typeof event.revision === 'number'
        && Number.isSafeInteger(event.revision) && event.revision >= 0) preferences.followRevision(event.revision);
    });
    onLiveEvent('up', () => preferences.followRevision());
  }
  return preferences.hydrate();
}
export const setServicePreferences = preferences.patch;
export const flushServicePreferences = preferences.flush;
export const reloadServicePreferences = preferences.reload;
