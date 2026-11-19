export const FLOW_CALIBRATION_PREFERENCE = 'orca.web.flowCalibration';
export type FlowCalibrationMode = 'off' | 'auto' | 'on';

export const FLOW_CALIBRATION_OPTIONS: readonly { id: FlowCalibrationMode; label: string }[] = [
  { id: 'off', label: 'Off' }, { id: 'auto', label: 'Auto' }, { id: 'on', label: 'On' },
];

export const flowCalibrationPreference = (value: unknown): FlowCalibrationMode => value === 'off' || value === 'on' ? value : 'auto';

export const effectiveFlowCalibration = (mode: FlowCalibrationMode, supportsAuto: boolean): FlowCalibrationMode =>
  mode === 'auto' && !supportsAuto ? 'on' : mode;
