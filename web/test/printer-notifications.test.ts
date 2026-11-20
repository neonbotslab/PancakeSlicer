import assert from 'node:assert/strict';
import { test } from 'node:test';
import { PrinterNotificationTracker } from '../src/notifications/printer-events.ts';
import type { PrinterConnection, PrinterStatus } from '../src/printers/connections.ts';

const printer: PrinterConnection = {
  printer: 'sample-printer', id: 'sample-connection', name: 'Workshop', kind: 'bambu-lan', has_secret: false,
};
const started_at = '2026-01-01T00:00:00Z';
const file = 'Example.gcode.3mf';
const finishedAt = '2026-01-01T01:02:00Z';

function status(state: PrinterStatus['state'], fields: Partial<PrinterStatus> = {}): PrinterStatus {
  return { printer: printer.printer, link: 'online', online: true, state, at: finishedAt, ...fields };
}

function setup(connection = printer) {
  const tracker = new PrinterNotificationTracker();
  return (report: PrinterStatus | undefined) => tracker.observe([connection], report ? [report] : []);
}

function faults(...codes: string[]): Pick<PrinterStatus, 'messages'> {
  return { messages: codes.map((code) => ({ code })) };
}

test('the first online snapshot seeds existing states and blockers', () => {
  for (const state of ['finished', 'failed', 'paused', 'idle'] satisfies PrinterStatus['state'][]) {
    const observe = setup();
    assert.deepEqual(observe(status('connecting', { online: false })), []);
    assert.deepEqual(observe(status(state, { file, ...faults('03009600-00010001') })), []);
    assert.deepEqual(observe(status(state, { file, ...faults('03009600-00010001') })), []);
  }
});

test('an observed finish is delivered once with its file, printer name, and elapsed time', () => {
  const observe = setup();
  const running = status('printing', { file, started_at, at: '2026-01-01T00:15:00Z' });
  const done = status('finished');
  assert.deepEqual(observe(running), []);
  const notices = observe(done);
  assert.equal(notices.length, 1);
  assert.equal(notices[0].title, 'Print finished');
  assert.equal(notices[0].body, 'Example.gcode.3mf · Workshop · 1 h 2 min');
  assert.deepEqual(notices[0].destination, { kind: 'device', printer: printer.printer });
  assert.deepEqual(observe(undefined), []);
  assert.deepEqual(observe(status('connecting')), []);
  assert.deepEqual(observe(running), []);
  assert.deepEqual(observe(done), []);
});

test('a finish first reported after a monitoring gap omits elapsed time', () => {
  const running = status('printing', { file, started_at, at: '2026-01-01T00:15:00Z' });
  for (const gap of [status('offline', { online: false }), status('connecting'), undefined]) {
    const observe = setup();
    observe(running);
    assert.deepEqual(observe(gap), []);
    assert.deepEqual(observe(running), []);
    const done = observe(status('finished'));
    assert.equal(done.length, 1);
    assert.equal(done[0].body, 'Example.gcode.3mf · Workshop');
    assert.deepEqual(observe(status('finished')), []);
  }

  const observe = setup();
  observe(running);
  observe(status('offline', { online: false }));
  observe(status('printing', { file, at: '2026-01-01T00:45:00Z' }));
  assert.equal(observe(status('finished'))[0].body, 'Example.gcode.3mf · Workshop · 1 h 2 min');
});

test('a finish omits absent, invalid, and contradictory timing information', () => {
  for (const value of [undefined, 'bad date', '2026-01-01T01:03:00Z']) {
    const observe = setup();
    assert.deepEqual(observe(status('printing', { file, started_at: value })), []);
    assert.equal(observe(status('finished'))[0].body, 'Example.gcode.3mf · Workshop');
  }
  const observe = setup();
  observe(status('printing', { started_at }));
  assert.equal(observe(status('finished', { at: 'bad date' }))[0].body, 'Workshop');
});

test('a terminal state needs an observed active print with compatible identity', () => {
  const job = { id: 'sample-job', plate: 1, sent_at: started_at };
  for (const conflict of [
    { file: 'Different.gcode.3mf' },
    { started_at: '2026-01-01T01:00:00Z' },
    { job: { ...job, id: 'different-job' } },
  ]) {
    const observe = setup();
    observe(status('printing', { file, started_at, job }));
    assert.deepEqual(observe(status('finished', { file, started_at, job, ...conflict })), []);
  }
  const observe = setup();
  observe(status('idle'));
  assert.deepEqual(observe(status('failed', { file, ...faults('DEADBEEF') })), []);
  observe(status('busy', { detail: 'OTHER' }));
  assert.deepEqual(observe(status('finished', { file })), []);
  observe(status('busy', { file, detail: 'PREPARE, error 00000000' }));
  const failed = observe(status('failed', faults('03004006')));
  assert.equal(failed.length, 1);
  assert.equal(failed[0].title, 'Print failed');
  assert.equal(failed[0].body, 'Example.gcode.3mf · Nozzle clogged');
  assert.deepEqual(observe(status('failed', faults('03004006'))), []);
});

test('a pause and blocker produce one alert per pause edge with one shared printer tag', () => {
  const observe = setup();
  observe(status('printing', { file, started_at }));
  const paused = status('paused', { file, ...faults('03008015') });
  const first = observe(paused);
  assert.equal(first.length, 1);
  assert.equal(first[0].title, 'Print paused');
  assert.equal(first[0].body, 'Filament ran out');
  assert.deepEqual(observe(status('offline', { online: false })), []);
  assert.deepEqual(observe(paused), []);
  assert.deepEqual(observe(status('printing', { file })), []);
  const second = observe(paused);
  assert.equal(second.length, 1);
  assert.notEqual(first[0].id, second[0].id);
  assert.equal(first[0].tag, second[0].tag);
  const failed = observe(status('failed', { ...faults('03008015', '03001A00-00020002') }));
  assert.equal(failed.length, 1);
  assert.equal(failed[0].body, 'Example.gcode.3mf · Nozzle clogged');
  assert.equal(first[0].tag, failed[0].tag);
});

test('metadata changes during a pause preserve the observed pause edge', () => {
  const observe = setup();
  observe(status('paused', { file, started_at }));
  const newStart = '2026-01-01T01:00:00Z';
  assert.deepEqual(observe(status('paused', { file: 'Updated.gcode.3mf', started_at: newStart, ...faults('03008013') })), []);
  const finished = observe(status('finished'));
  assert.equal(finished.length, 1);
  assert.equal(finished[0].body, 'Updated.gcode.3mf · Workshop · 2 min');
});

test('replayed completion of an identified print stays silent and a real new print can finish', () => {
  const observe = setup();
  const job = { id: 'sample-job', plate: 1, sent_at: started_at };
  const printing = status('printing', { job, file, started_at });
  const finished = status('finished');
  observe(printing);
  assert.equal(observe(finished).length, 1);
  assert.deepEqual(observe(status('offline', { online: false })), []);
  assert.deepEqual(observe(printing), []);
  assert.deepEqual(observe(finished), []);
  observe(status('printing', { job, file, started_at: '2026-01-01T01:00:00Z' }));
  assert.equal(observe(finished)[0].body, 'Example.gcode.3mf · Workshop · 2 min');
});

test('the same transfer and file can start a subsequent print without a start timestamp', () => {
  const observe = setup();
  const job = { id: 'sample-job', plate: 1, sent_at: started_at };
  observe(status('printing', { job, file }));
  assert.equal(observe(status('finished', { job, file }))[0].title, 'Print finished');
  observe(status('busy', { job, file, detail: 'PREPARE' }));
  observe(status('printing', { job, file }));
  assert.equal(observe(status('finished', { job, file }))[0].title, 'Print finished');
});

test('critical codes are stable across formatting, ordering, reconnect, and subset changes', () => {
  const observe = setup();
  observe(status('printing'));
  const door = '03009600-00010001';
  const clog = '03001A00-00020002';
  const first = observe(status('printing', faults(door, clog)));
  assert.equal(first.length, 1);
  assert.equal(first[0].title, 'Printer needs attention');
  assert.equal(first[0].body, 'Nozzle clogged');
  assert.deepEqual(observe(undefined), []);
  assert.deepEqual(observe(status('printing', faults('hms_0300_1a00_0002_0002', door, door))), []);
  assert.deepEqual(observe(status('printing', faults(door))), []);
  const again = observe(status('printing', faults(door, clog)));
  assert.equal(again.length, 1);
  assert.equal(again[0].body, 'Nozzle clogged');
  assert.equal(again[0].tag, first[0].tag);
});

test('known aliases alert again only after their shared blocker clears', () => {
  const observe = setup();
  observe(status('printing'));
  const short = '03008042';
  const long = '03009600-00010001';
  assert.equal(observe(status('printing', faults(short)))[0].body, 'Door open');
  assert.deepEqual(observe(status('printing', faults(short, long))), []);
  assert.deepEqual(observe(status('printing', faults(long))), []);
  assert.deepEqual(observe(status('printing')), []);
  assert.equal(observe(status('printing', faults(long)))[0].body, 'Door open');
});

test('a new blocker takes precedence over a finish in the same snapshot', () => {
  const observe = setup();
  observe(status('printing', { file, started_at }));
  const finished = status('finished', faults('03009600-00010001'));
  const notices = observe(finished);
  assert.equal(notices.length, 1);
  assert.equal(notices[0].title, 'Printer needs attention');
  assert.equal(notices[0].body, 'Door open');
  assert.deepEqual(observe(finished), []);
});

test('known stop codes show a plain reason for LAN and H2D filament, door, and AI events', () => {
  const cases = [
    ['07008011', 'Filament ran out'],
    ['07032100-00020001', 'Filament ran out'],
    ['07FE8011', 'Filament ran out'],
    ['07FF2000-00020001', 'Filament ran out'],
    ['18FF8011', 'Filament ran out'],
    ['07FEC030', 'Filament ran out'],
    ['07008006', 'Filament feed problem'],
    ['07027100-00020002', 'Filament feed problem'],
    ['07FE8005', 'Filament feed problem'],
    ['07FF8004', 'Filament feed problem'],
    ['07007000-00020003', 'Filament extrusion failed'],
    ['12008005', 'Filament not inserted'],
    ['12018005', 'Filament feed problem'],
    ['03008016', 'Nozzle clogged'],
    ['0300800F', 'Door open'],
    ['0300804B', 'Laser safety window open'],
    ['03009600-00010001', 'Door open'],
    ['03008003', 'Spaghetti detected'],
    ['0C008042', 'Spaghetti detected'],
  ];
  for (const [code, reason] of cases) {
    const observe = setup();
    observe(status('printing'));
    const notices = observe(status('paused', faults(code)));
    assert.equal(notices.length, 1, code);
    assert.equal(notices[0].body, reason, code);
  }
});

test('common AI detections supply a reason only when the print stops', () => {
  for (const [code, reason] of [
    ['0C000300-00030007', 'Possible first-layer defect'],
    ['0C000300-00030008', 'Possible spaghetti defect'],
    ['0C00C004', 'Possible spaghetti defect'],
  ]) {
    const observe = setup();
    observe(status('printing'));
    assert.deepEqual(observe(status('printing', faults(code))), []);
    assert.equal(observe(status('paused', faults(code)))[0].body, reason);
  }
});

test('unknown fatal and serious codes display the code without device message text', () => {
  const observe = setup();
  observe(status('idle'));
  for (const code of ['DEADBEEF-0001AA01', 'DEADBEEF-0002AA01']) {
    const notices = observe(status('idle', { messages: [{ code, message: 'Arbitrary printer text' }] }));
    assert.equal(notices.length, 1);
    assert.equal(notices[0].title, 'Printer needs attention');
    assert.equal(notices[0].body, `Printer needs attention · ${code}`);
  }
  assert.deepEqual(observe(status('idle', faults('DEAD8EEF', 'DEADBEEF-0003AA01', 'DEADBEEF-0004AA01'))), []);
  assert.deepEqual(observe(status('printing', faults('DEADCEEF'))), []);
  const notices = observe(status('printing', faults('DEAD8EEF')));
  assert.equal(notices.length, 1);
  assert.equal(notices[0].body, 'Printer needs attention · DEAD8EEF');
});

test('recovered and advisory codes stay silent even when they mention blocking events', () => {
  const observe = setup();
  observe(status('printing'));
  assert.deepEqual(observe(status('printing', faults(
    '07002000-00030002', '0300800C', '0700C008', '03009600-00030001', '03009E00-00030001',
    '0C000100-00020008', '0C008034', '03008013', '0300AB00-00020003', '1000C001',
  ))), []);
  assert.deepEqual(observe(status('idle', { detail: 'cancelled', ...faults('0300400C') })), []);
  assert.deepEqual(observe(status('finished')), []);
});

test('unsupported, unassigned and replaced connections never inherit another print', () => {
  for (const connection of [
    { ...printer, kind: 'bambu-connect' },
    { ...printer, kind: 'moonraker' },
    { ...printer, printer: '' },
  ] satisfies PrinterConnection[]) {
    const observe = setup(connection);
    observe(status('printing', { printer: connection.printer }));
    assert.deepEqual(observe(status('paused', { printer: connection.printer, ...faults('03008015') })), []);
    assert.deepEqual(observe(status('finished', { printer: connection.printer })), []);
  }
  const tracker = new PrinterNotificationTracker();
  const replacement = { ...printer, id: 'replacement-connection' };
  tracker.observe([printer], [status('printing')]);
  assert.deepEqual(tracker.observe([replacement], [status('finished')]), []);
  assert.deepEqual(tracker.observe([printer], [status('finished')]), []);
});

test('each LAN connection has its own baseline, notification tag, and destination', () => {
  const second = { ...printer, printer: 'second-printer', id: 'second-connection', name: 'Studio' };
  const tracker = new PrinterNotificationTracker();
  tracker.observe([printer, second], [status('printing'), status('printing', { printer: second.printer })]);
  const notices = tracker.observe([printer, second], [status('finished'), status('finished', { printer: second.printer })]);
  assert.deepEqual(notices.map(({ body, destination }) => ({ body, destination })), [
    { body: 'Workshop', destination: { kind: 'device', printer: printer.printer } },
    { body: 'Studio', destination: { kind: 'device', printer: second.printer } },
  ]);
  assert.equal(new Set(notices.map(({ tag }) => tag)).size, 2);
});


test('distinct AMS faults still notify when another unit has the same reason', () => {
  const observe = setup();
  observe(status('printing'));
  assert.equal(observe(status('printing', faults('07008006'))).length, 1);
  assert.equal(observe(status('printing', faults('07008006', '07018006'))).length, 1);
  assert.deepEqual(observe(status('printing', faults('07018006', '07008006'))), []);
});
