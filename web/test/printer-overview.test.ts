import assert from 'node:assert/strict';
import { test } from 'node:test';
import type { PrinterStatus } from '../src/printers/connections.ts';
import { overviewStatus } from '../src/printers/overview-status.ts';

const at = '2026-11-19T15:12:00Z';

function report(state: PrinterStatus['state'], fields: Partial<PrinterStatus> = {}): PrinterStatus {
  return { printer: 'workshop', online: true, state, at, ...fields };
}

test('active jobs carry a filename and compact progress with remaining time', () => {
  for (const kind of ['printing', 'paused'] satisfies PrinterStatus['state'][]) {
    const card = overviewStatus(true, report(kind, { file: '/prints/Hinge bracket.gcode.3mf', progress: 0.62, remaining_s: 4440 }));
    assert.deepEqual(card, {
      kind, detail: { kind: 'job', text: 'Hinge bracket.gcode.3mf' }, progress: 0.62, progressText: '62% · 1h 14m left',
    });
  }
  assert.equal(overviewStatus(true, report('busy', { detail: 'PREPARE' })).kind, 'printing');
  assert.equal(overviewStatus(true, report('busy')).kind, 'idle');
});

test('idle suppresses a stale filename and terminal statuses keep the reported job', () => {
  const file = 'Calibration.gcode.3mf';
  assert.deepEqual(overviewStatus(true, report('idle', { file })), { kind: 'idle', detail: { kind: 'job', text: '' } });
  for (const kind of ['finished', 'failed'] satisfies PrinterStatus['state'][]) {
    assert.deepEqual(overviewStatus(true, report(kind, { file, progress: 0.8 })), { kind, detail: { kind: 'job', text: file } });
  }
});

test('offline cards use last seen and suppress stale blocking issues', () => {
  const messages = [{ code: '0300404B' }];
  assert.deepEqual(overviewStatus(true, report('offline', { online: false, last_seen: at, messages })), { kind: 'offline', lastSeen: at });
  assert.deepEqual(overviewStatus(false, report('paused', { messages })), { kind: 'offline', lastSeen: at });
  assert.deepEqual(overviewStatus(true, undefined), { kind: 'offline', lastSeen: undefined });
});

test('blocking errors replace the job line with a readable reason and distinct issue count', () => {
  const status = report('paused', {
    file: 'Hinge bracket.gcode.3mf',
    messages: [
      { code: '07002200-00020001', message: 'Raw firmware text' },
      { code: 'HMS_07002200_00020001' },
      { code: '07002100-00020001' },
      { code: '0300404B' },
      { code: '03008028' },
    ],
  });
  const card = overviewStatus(true, status);
  assert.deepEqual('detail' in card && card.detail, { kind: 'issue', text: 'Filament ran out (AMS 1, slot 3)', more: 2 });
  const runout = overviewStatus(true, report('paused', { messages: status.messages?.slice(0, 2) }));
  assert.deepEqual('detail' in runout && runout.detail, { kind: 'issue', text: 'Filament ran out (AMS 1, slot 3)', more: 0 });
  const unknown = overviewStatus(true, report('printing', { messages: [{ code: 'C0018001' }] }));
  assert.deepEqual('detail' in unknown && unknown.detail, { kind: 'issue', text: 'Printer needs attention', more: 0 });
});

test('user pauses and informational HMS messages do not show a blocking error', () => {
  const file = 'Hinge bracket.gcode.3mf';
  const card = overviewStatus(true, report('paused', { file, messages: [{ code: '03008028' }, { code: '07002000-00030002' }] }));
  assert.equal(card.kind, 'paused');
  assert.deepEqual('detail' in card && card.detail, { kind: 'job', text: file });
});
