import { useSyncExternalStore } from 'react';
import { cancelDeviceSwitch, deviceSwitchCountdown, subscribeDeviceSwitch } from '../printers/device-switch';
import { Glyph } from './Glyph';

export const useDeviceSwitchCountdown = () => useSyncExternalStore(subscribeDeviceSwitch, deviceSwitchCountdown);

export function DeviceSwitchToast({ seconds }: { seconds: number }) {
  return <div className="toast toast-info device-switch-toast">
    <div className="toast-text" role="status">Opening Device in {seconds}s</div>
    <button type="button" className="btn-ghost toast-action" onClick={cancelDeviceSwitch}>Cancel</button>
    <button type="button" className="toast-close icon-btn" aria-label="Close countdown" onClick={cancelDeviceSwitch}><Glyph name="close" size={14} /></button>
  </div>;
}
