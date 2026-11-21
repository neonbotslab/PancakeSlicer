import { createDeviceSwitch, OPEN_DEVICE_AFTER_PRINT, OPEN_DEVICE_DELAY } from '../flow/device-switch';
import { currentRoute, routing } from '../flow/routes';
import { printerSend } from '../flow/transfer';
import { openDevice } from '../navigation';
import { preference } from '../saved-preferences';
import { onLeaveDocument } from '../state/history';
import { getState, subscribeStore } from '../state/store';

let countdown: number | null = null;
const listeners = new Set<() => void>();
const deviceSwitch = createDeviceSwitch({
  clock: {
    now: () => performance.now(),
    after(ms, callback) { const timer = setTimeout(callback, ms); return () => clearTimeout(timer); },
  },
  preferences: () => ({ enabled: preference(OPEN_DEVICE_AFTER_PRINT), delay: preference(OPEN_DEVICE_DELAY) }),
  show(seconds) { countdown = seconds; listeners.forEach((listener) => listener()); },
  open({ printer, transferId }) {
    if (printerSend.machine.get().context.transfer?.id === transferId) printerSend.reset();
    openDevice(printer);
  },
});

export const deviceSwitchCountdown = () => countdown;
export const subscribeDeviceSwitch = (listener: () => void) => {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
};
export const cancelDeviceSwitch = deviceSwitch.cancel;

export function followDeviceSwitch() {
  currentRoute();
  let { tab, printerConnections: { transfers } } = getState();
  const stopRoute = routing.subscribe(cancelDeviceSwitch);
  const stopDocument = onLeaveDocument(cancelDeviceSwitch);
  window.addEventListener('pagehide', cancelDeviceSwitch);
  const stopStore = subscribeStore(() => {
    const state = getState();
    if (state.tab !== tab) {
      tab = state.tab;
      cancelDeviceSwitch();
    }
    if (state.printerConnections.transfers !== transfers) {
      transfers = state.printerConnections.transfers;
      deviceSwitch.observe(transfers);
    }
  });
  const stopSend = printerSend.subscribeSubmission((event) => {
    deviceSwitch.submission(event);
    if (event.type === 'accepted') deviceSwitch.observe(getState().printerConnections.transfers);
  });
  return () => {
    stopSend(); stopStore(); stopRoute(); stopDocument();
    window.removeEventListener('pagehide', cancelDeviceSwitch);
    cancelDeviceSwitch();
  };
}
