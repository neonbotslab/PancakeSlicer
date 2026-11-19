import assert from 'node:assert/strict';
import test from 'node:test';
import { afterSlicePreference, outputActions } from '../src/after-slice.ts';

test('Export is the default for missing or unrecognized saved preferences', () => {
  for (const value of [undefined, null, '', 'export', 'unknown', true, 1, {}]) assert.equal(afterSlicePreference(value), 'export');
  assert.equal(afterSlicePreference('send'), 'send');
});

test('wide output buttons retain Export then Send while the chosen action becomes primary', () => {
  const options = { configuredPrinters: 1, sendAvailable: true, ready: true, layout: 'wide' } satisfies Omit<Parameters<typeof outputActions>[0], 'preference'>;
  assert.deepEqual(outputActions({ ...options, preference: 'export' }), { primary: 'export', buttons: ['export', 'send'], menu: [] });
  assert.deepEqual(outputActions({ ...options, preference: 'send' }), { primary: 'send', buttons: ['export', 'send'], menu: [] });
  assert.deepEqual(outputActions({ ...options, preference: 'send', configuredPrinters: 0 }), { primary: 'export', buttons: ['export', 'send'], menu: [] });
});

test('compact and folded layouts show the chosen output and put the other in the menu after slicing', () => {
  for (const layout of ['compact', 'folded'] as const) {
    const options = { configuredPrinters: 1, sendAvailable: true, ready: true, layout };
    assert.deepEqual(outputActions({ ...options, preference: 'export' }), { primary: 'export', buttons: ['export'], menu: ['send'] });
    assert.deepEqual(outputActions({ ...options, preference: 'send' }), { primary: 'send', buttons: ['send'], menu: ['export'] });
    assert.deepEqual(outputActions({ ...options, preference: 'send', configuredPrinters: 0 }), { primary: 'export', buttons: ['export'], menu: ['send'] });
  }
});

test('the saved choice does not change output placement or Slice emphasis before a usable slice', () => {
  for (const preference of ['export', 'send']) {
    const options = { preference, configuredPrinters: 1, sendAvailable: true, ready: false };
    assert.deepEqual(outputActions({ ...options, layout: 'wide' }), { primary: 'slice', buttons: ['export', 'send'], menu: [] });
    assert.deepEqual(outputActions({ ...options, layout: 'compact' }), { primary: 'slice', buttons: ['export'], menu: ['send'] });
    assert.deepEqual(outputActions({ ...options, layout: 'folded' }), { primary: 'slice', buttons: [], menu: ['export', 'send'] });
  }
});

test('Export stays available when the printer connection service is unavailable', () => {
  for (const layout of ['wide', 'compact', 'folded'] as const) {
    const options = { preference: 'send', configuredPrinters: 0, sendAvailable: false, ready: true, layout };
    assert.deepEqual(outputActions(options), { primary: 'export', buttons: ['export'], menu: [] });
  }
});
