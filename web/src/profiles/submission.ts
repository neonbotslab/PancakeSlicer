import type { PresetType } from '../api/types';
import { sliceUsedSlots } from '../printers/slot-check';
import type { State } from '../state/store';
import { selectCompatibility } from './compatibility';
import { recordKey } from './records';
import { recordKind, recordOf } from './view';
import { slotFilaments } from './slots';

export function validateProfileSubmission(s: State) {
  const selections: [PresetType, string, number?][] = [['machine', s.printer], ['process', s.process]];
  const used = sliceUsedSlots(s);
  slotFilaments(s).forEach((key, i) => {
    if (key || used.has(i + 1)) selections.push(['filament', key, i + 1]);
  });
  return selections.map(([type, key, slot]) => {
    const record = recordOf(s, type, key);
    if (!record) throw Object.assign(new Error('Please save or restore the selected profile before slicing.'), {
      code: 'profile_unknown', recovery: 'save_profile', profile: { kind: type, name: key, vendor: '', slot },
    });
    const compatibility = selectCompatibility(s, type, key);
    if (!compatibility.ok) throw Object.assign(new Error(`${record.name}: ${compatibility.reason}`), {
      code: 'profile_incompatible', recovery: 'repair_profile', profile: { kind: type, name: record.name, vendor: '', slot },
    });
    return { key: recordKey(recordKind(type), record.id), revision: record.revision };
  });
}
