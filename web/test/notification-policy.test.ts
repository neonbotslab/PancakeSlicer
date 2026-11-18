import assert from 'node:assert/strict';
import test from 'node:test';
import { bodyFacts, eligibleSliceOffer, printerTag, projectTag, shouldNotify, validateDestination } from '../src/notifications/policy.ts';

test('only a hidden, opted-in, supported page with permission can notify', () => {
  const eligible = { hidden: true, enabled: true, permission: 'granted', supported: true } satisfies Parameters<typeof shouldNotify>[0];
  assert.equal(shouldNotify(eligible), true);
  assert.equal(shouldNotify({ ...eligible, hidden: false }), false);
  assert.equal(shouldNotify({ ...eligible, enabled: false }), false);
  assert.equal(shouldNotify({ ...eligible, supported: false }), false);
  assert.equal(shouldNotify({ ...eligible, permission: 'default' }), false);
  assert.equal(shouldNotify({ ...eligible, permission: 'denied' }), false);
  assert.equal(shouldNotify({ ...eligible, permission: 'unsupported' }), false);
});

test('notification groups replace only notices for the same project or printer', () => {
  assert.equal(projectTag('test'), projectTag('test'));
  assert.equal(printerTag('test'), printerTag('test'));
  assert.notEqual(projectTag('test'), projectTag('another'));
  assert.notEqual(printerTag('test'), printerTag('another'));
  assert.notEqual(projectTag('test'), printerTag('test'));
});

test('notification bodies omit missing facts and their separators', () => {
  assert.equal(bodyFacts(['Plate 2', undefined, '  ', null, '12m', '', '5 g']), 'Plate 2 · 12m · 5 g');
  assert.equal(bodyFacts([null, undefined, '']), '');
});

test('a slice is eligible after ten seconds or any time spent hidden', () => {
  assert.equal(eligibleSliceOffer({ elapsedMs: 9_999, wasHidden: false }), false);
  assert.equal(eligibleSliceOffer({ elapsedMs: 10_000, wasHidden: false }), true);
  assert.equal(eligibleSliceOffer({ elapsedMs: 20, wasHidden: true }), true);
});

test('notification destinations require complete scope and carry no URL', () => {
  assert.deepEqual(validateDestination({ kind: 'preview', projectId: 'p1', plate: 2, jobId: 'j2', signature: 'signature-2', url: 'https://example.net' }),
    { kind: 'preview', projectId: 'p1', plate: 2, jobId: 'j2', signature: 'signature-2' });
  assert.deepEqual(validateDestination({ kind: 'slice-error', projectId: 'p1', plate: 1, attempt: 'a1', error: 'Slicing failed' }),
    { kind: 'slice-error', projectId: 'p1', plate: 1, attempt: 'a1', error: 'Slicing failed' });
  assert.deepEqual(validateDestination({ kind: 'device', printer: 'p0' }), { kind: 'device', printer: 'p0' });
  assert.deepEqual(validateDestination({ kind: 'focus' }), { kind: 'focus' });
  assert.equal(validateDestination({ kind: 'preview', projectId: 'p1', plate: -1, jobId: 'j2', signature: 'signature-2' }), null);
  assert.equal(validateDestination({ kind: 'preview', projectId: 'p1', plate: 2.2, jobId: 'j2', signature: 'signature-2' }), null);
  assert.equal(validateDestination({ kind: 'preview', projectId: 'p1', plate: 2 }), null);
  assert.equal(validateDestination({ kind: 'slice-error', projectId: 'p1', plate: 2, attempt: ' ' }), null);
  assert.equal(validateDestination({ kind: 'device', printer: 123 }), null);
  assert.equal(validateDestination({ kind: 'link', url: 'https://example.net' }), null);
});
