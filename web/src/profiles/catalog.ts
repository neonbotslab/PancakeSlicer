import type { PresetType } from '../api/types';
import { type State } from '../state/store';
import { NAME_MAX } from './records';
import { selectCompatibility } from './compatibility';
const recordsOf = (s: State, type: PresetType) => type === 'machine' ? s.records.printers : type === 'process' ? s.records.processes : s.records.filaments;

export interface ProfileRow { key: string; name: string; mine: true }

const profileRows = new WeakMap<State, Map<PresetType, ProfileRow[]>>();
export function listProfiles(s: State, type: PresetType, filters: { browseAll?: boolean } = {}): ProfileRow[] {
  let cache = profileRows.get(s);
  if (!cache) profileRows.set(s, (cache = new Map()));
  if (!filters.browseAll && cache.has(type)) return cache.get(type)!;
  const rows: ProfileRow[] = recordsOf(s, type).filter((r) => !r.archived && (filters.browseAll || selectCompatibility(s, type, r.id).ok))
    .map((r) => ({ key: r.id, name: r.name, mine: true as const }));
  if (!filters.browseAll) cache.set(type, rows);
  return rows;
}
export function availableName(taken: ReadonlySet<string>, desired: string): string {
  let name = desired;
  for (let n = 2; taken.has(name); n++) {
    const suffix = ` (${n})`;
    name = [...desired].slice(0, NAME_MAX - suffix.length).join('') + suffix;
  }
  return name;
}
export function uniqueName(s: State, type: PresetType, desired: string): string {
  return availableName(new Set(recordsOf(s, type).filter((r) => !r.archived).map((r) => r.name)), desired);
}
