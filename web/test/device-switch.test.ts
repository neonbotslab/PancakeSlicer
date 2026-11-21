import assert from 'node:assert/strict';
import test from 'node:test';
import { createDeviceSwitch, openDeviceAfterPrint, openDeviceDelay } from '../src/flow/device-switch.ts';
import type { Transfer } from '../src/flow/transfer.ts';

function setup(enabled: unknown = true, delay: unknown = 5) {
  let now = 0;
  const timers = new Set<{ at: number; run(): void }>();
  const seconds: (number | null)[] = [];
  const opened: { printer: string; transferId: string }[] = [];
  const deviceSwitch = createDeviceSwitch({
    clock: {
      now: () => now,
      after(ms, run) {
        const timer = { at: now + ms, run };
        timers.add(timer);
        return () => { timers.delete(timer); };
      },
    },
    preferences: () => ({ enabled, delay }),
    show: (value) => { seconds.push(value); },
    open: (target) => { opened.push(target); },
  });

  const advance = (ms: number) => {
    const end = now + ms;
    for (;;) {
      const next = [...timers].sort((a, b) => a.at - b.at)[0];
      if (!next || next.at > end) break;
      timers.delete(next);
      now = next.at;
      next.run();
    }
    now = end;
  };
  const submit = (id = 'transfer', start = true) => {
    const attempt = Symbol();
    deviceSwitch.submission({ type: 'submitted', attempt, start, job: 'job', printer: 'printer' });
    return {
      accepted: () => deviceSwitch.submission({ type: 'accepted', attempt, transferId: id }),
      aborted: () => deviceSwitch.submission({ type: 'aborted', attempt }),
    };
  };
  const report = (patch: Partial<Transfer> = {}) => deviceSwitch.observe([{
    id: 'transfer', job: 'job', printer: 'printer', name: 'Fixture printer', kind: 'moonraker',
    start: true, filename: 'print.gcode', state: 'done', outcome: 'printing', sent: 100, total: 100, started: '', ...patch,
  }]);
  return { advance, seconds, opened, report, submit, timers, cancel: deviceSwitch.cancel };
}

test('missing preferences enable the feature with five seconds, and numeric delays round and clamp', () => {
  for (const value of [undefined, null, 'false', 0, {}]) assert.equal(openDeviceAfterPrint(value), true);
  assert.equal(openDeviceAfterPrint(false), false);
  for (const value of [undefined, null, true, '0', NaN, Infinity, -Infinity, {}]) assert.equal(openDeviceDelay(value), 5);
  assert.deepEqual([-1, 0, 0.5, 3.49, 3.5, 60, 61].map(openDeviceDelay), [0, 0, 1, 3, 4, 60, 60]);
});

test('HTTP acceptance, unrelated reports and unconfirmed uploads wait for confirmation of the exact transfer', () => {
  const t = setup();
  t.report();
  const attempt = t.submit();
  t.report();
  attempt.accepted();
  t.report({ id: 'older-transfer' });
  t.report({ printer: 'other-printer' });
  t.report({ job: 'other-job' });
  t.report({ state: 'uploading' });
  t.report({ state: 'starting' });
  t.report({ outcome: 'uploaded' });
  t.report({ outcome: 'unconfirmed' });
  t.advance(20_000);
  assert.deepEqual(t.seconds, []);
  assert.deepEqual(t.opened, []);

  t.report();
  t.advance(2_000);
  t.report();
  t.advance(2_999);
  assert.deepEqual(t.seconds, [5, 4, 3, 2, 1]);
  assert.deepEqual(t.opened, []);
  t.advance(1);
  t.report();
  assert.deepEqual(t.seconds, [5, 4, 3, 2, 1, null]);
  assert.deepEqual(t.opened, [{ printer: 'printer', transferId: 'transfer' }]);
  assert.equal(t.timers.size, 0);
});

test('zero opens immediately after confirmation without a toast, and the countdown is capped at sixty seconds', () => {
  const immediate = setup(true, 0);
  immediate.submit().accepted();
  assert.deepEqual(immediate.opened, []);
  immediate.report();
  assert.deepEqual(immediate.seconds, []);
  assert.deepEqual(immediate.opened, [{ printer: 'printer', transferId: 'transfer' }]);
  assert.equal(immediate.timers.size, 0);

  const clamped = setup(true, 99.5);
  clamped.submit().accepted();
  clamped.report();
  assert.deepEqual(clamped.seconds, [60]);
  clamped.advance(59_999);
  assert.deepEqual(clamped.opened, []);
  clamped.advance(1);
  assert.deepEqual(clamped.opened, [{ printer: 'printer', transferId: 'transfer' }]);
});

test('disabled preference and upload-only submissions never arm a timer', () => {
  for (const t of [setup(false), setup()]) {
    t.submit('transfer', false).accepted();
    t.report();
    assert.equal(t.timers.size, 0);
    assert.deepEqual(t.seconds, []);
    assert.deepEqual(t.opened, []);
  }
  const t = setup(false);
  t.submit().accepted();
  t.report();
  assert.equal(t.timers.size, 0);

  const upload = setup();
  upload.submit().accepted();
  upload.report({ start: false });
  upload.report();
  assert.equal(upload.timers.size, 0);
  assert.deepEqual(upload.opened, []);
});

test('confirmation times out two minutes after submission, even when the POST resolves late', () => {
  for (const bindFirst of [true, false]) {
    const t = setup();
    const attempt = t.submit();
    t.advance(119_999);
    if (bindFirst) attempt.accepted();
    assert.equal(t.timers.size, 1);
    t.advance(1);
    if (!bindFirst) attempt.accepted();
    t.report();
    assert.equal(t.timers.size, 0);
    assert.deepEqual(t.seconds, []);
    assert.deepEqual(t.opened, []);
  }
});

test('cancel or leaving the page permanently dismisses a waiting or confirmed attempt', () => {
  for (const confirmed of [false, true]) {
    const t = setup();
    t.submit().accepted();
    if (confirmed) t.report();
    t.cancel();
    t.report();
    t.advance(120_000);
    assert.deepEqual(t.seconds, confirmed ? [5, null] : []);
    assert.deepEqual(t.opened, []);
    assert.equal(t.timers.size, 0);
  }
});

test('failed or cancelled transfer and print outcomes prevent navigation before and during the countdown', () => {
  const failures: Partial<Transfer>[] = [
    { state: 'failed' }, { state: 'cancelled' }, { outcome: 'failed' }, { job_end: 'failed' }, { job_end: 'cancelled' },
  ];
  for (const patch of failures) {
    for (const confirmed of [false, true]) {
      const t = setup();
      t.submit().accepted();
      if (confirmed) t.report();
      t.report(patch);
      t.report();
      t.advance(120_000);
      assert.deepEqual(t.seconds, confirmed ? [5, null] : []);
      assert.deepEqual(t.opened, []);
      assert.equal(t.timers.size, 0);
    }
  }
});

test('a cancelled upload or rejected POST discards later confirmation', () => {
  const t = setup();
  const attempt = t.submit();
  attempt.aborted();
  attempt.accepted();
  t.report();
  t.advance(120_000);
  assert.deepEqual(t.seconds, []);
  assert.deepEqual(t.opened, []);
});

test('withdrawn printer confirmation cancels an existing countdown', () => {
  for (const patch of [{ outcome: 'unconfirmed' }, { outcome: 'uploaded' }, { state: 'starting' }] satisfies Partial<Transfer>[]) {
    const t = setup();
    t.submit().accepted();
    t.report();
    t.advance(1_000);
    t.report(patch);
    t.report();
    t.advance(120_000);
    assert.deepEqual(t.seconds, [5, 4, null]);
    assert.deepEqual(t.opened, []);
    assert.equal(t.timers.size, 0);
  }
});

test('a new send replaces the old countdown and late callbacks belong to their original attempt', () => {
  const t = setup();
  const first = t.submit('old');
  first.accepted();
  t.report({ id: 'old' });
  const lateTick = [...t.timers][0];
  assert.ok(lateTick);
  const second = t.submit();
  lateTick.run();
  first.accepted();
  first.aborted();
  t.report({ id: 'old' });
  t.report();
  assert.deepEqual(t.seconds, [5, null]);
  assert.deepEqual(t.opened, []);
  second.accepted();
  t.report();
  t.advance(5_000);
  assert.deepEqual(t.opened, [{ printer: 'printer', transferId: 'transfer' }]);

  t.submit().accepted();
  t.report();
  t.submit('upload-only', false).accepted();
  t.advance(120_000);
  assert.deepEqual(t.opened, [{ printer: 'printer', transferId: 'transfer' }]);
});
