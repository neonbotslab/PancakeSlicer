import type { PresetType } from './api/types';
import { getState, setState, type LibraryTarget } from './state/store';
import { showDevice } from './flow/project';

export function openLibrary(type: PresetType, target: Omit<LibraryTarget, 'type' | 'at'> = {}) {
  setState({ tab: 'library', library: { type, ...target, at: Date.now() } });
}

export function openDevice(printer = getState().printer) {
  showDevice(printer || null);
}

export function openSlideOutEditor(type: PresetType, key: string, setting?: string) {
  if (key) setState({ tab: 'prepare', appDialog: null, slideOut: { type, key, ...(setting ? { setting } : {}), at: Date.now() } });
}
export const closeSlideOut = () => setState({ slideOut: null });
