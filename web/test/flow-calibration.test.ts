import assert from 'node:assert/strict';
import test from 'node:test';
import { effectiveFlowCalibration, flowCalibrationPreference } from '../src/flow-calibration.ts';

test('Flow calibration defaults to Auto and keeps each valid saved choice', () => {
  for (const value of [undefined, null, '', 'unknown', true, 1, {}]) assert.equal(flowCalibrationPreference(value), 'auto');
  assert.equal(flowCalibrationPreference('off'), 'off');
  assert.equal(flowCalibrationPreference('auto'), 'auto');
  assert.equal(flowCalibrationPreference('on'), 'on');
});

test('Auto falls back to On only for printers without Auto support', () => {
  assert.equal(effectiveFlowCalibration('auto', true), 'auto');
  assert.equal(effectiveFlowCalibration('auto', false), 'on');
  for (const support of [true, false]) {
    assert.equal(effectiveFlowCalibration('off', support), 'off');
    assert.equal(effectiveFlowCalibration('on', support), 'on');
  }
});
