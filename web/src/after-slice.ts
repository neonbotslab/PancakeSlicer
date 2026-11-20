export const AFTER_SLICE_PREFERENCE = 'orca.web.afterSlice';
export type AfterSliceAction = 'export' | 'send';

export const afterSlicePreference = (value: unknown): AfterSliceAction => value === 'send' ? 'send' : 'export';

interface OutputActions {
  primary: 'slice' | AfterSliceAction;
  buttons: AfterSliceAction[];
  menu: AfterSliceAction[];
}

export function outputActions({ preference, configuredPrinters, sendAvailable, ready, layout }: {
  preference: unknown;
  configuredPrinters: number;
  sendAvailable: boolean;
  ready: boolean;
  layout: 'wide' | 'compact' | 'folded';
}): OutputActions {
  const selected = afterSlicePreference(preference) === 'send' && sendAvailable && configuredPrinters > 0 ? 'send' : 'export';
  const primary = ready ? selected : 'slice';
  const send: AfterSliceAction[] = sendAvailable ? ['send'] : [];
  if (layout === 'wide') return { primary, buttons: ['export', ...send], menu: [] };
  if (primary !== 'slice') return { primary, buttons: [primary], menu: primary === 'send' ? ['export'] : send };
  if (layout === 'compact') return { primary, buttons: ['export'], menu: send };
  return { primary, buttons: [], menu: ['export', ...send] };
}
