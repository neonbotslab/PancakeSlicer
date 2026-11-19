import assert from 'node:assert/strict';
import test from 'node:test';
import { followCameraVisibility, JPEGFrames } from '../src/printers/camera-stream.ts';

test('JPEG frames survive multipart headers and arbitrary transport chunks', () => {
  const frames = new JPEGFrames(), encoder = new TextEncoder();
  const input = encoder.encode('--fixture\r\nContent-Type: image/jpeg\r\nContent-Length: 6\r\n\r\nabcdef\r\n--fixture\r\ncontent-length: 3\r\n\r\nxyz\r\n');
  const received = [...input].flatMap(byte => frames.push(new Uint8Array([byte])));
  assert.deepEqual(received, [encoder.encode('abcdef'), encoder.encode('xyz')]);
});

test('JPEG payload markers and embedded thumbnails do not truncate a frame', () => {
  const frames = new JPEGFrames();
  const header = new TextEncoder().encode('--fixture\r\nContent-Length: 10\r\n\r\n');
  const jpeg = new Uint8Array([0xff, 0xd8, 0xff, 0xe1, 0xff, 0xd9, 0, 1, 0xff, 0xd9]);
  assert.deepEqual(frames.push(header), []);
  assert.deepEqual(frames.push(jpeg), [jpeg]);
});

test('visibility pauses immediately, resumes on return, and disposes its listener', () => {
  const source = Object.assign(new EventTarget(), { visibilityState: 'visible' as DocumentVisibilityState });
  const visible: boolean[] = [];
  const stop = followCameraVisibility(source, (value) => visible.push(value));
  source.visibilityState = 'hidden'; source.dispatchEvent(new Event('visibilitychange'));
  source.visibilityState = 'visible'; source.dispatchEvent(new Event('visibilitychange'));
  assert.deepEqual(visible, [true, false, true]);
  stop(); source.visibilityState = 'hidden'; source.dispatchEvent(new Event('visibilitychange'));
  assert.deepEqual(visible, [true, false, true]);
});

test('an initially hidden tab does not start playback', () => {
  const source = Object.assign(new EventTarget(), { visibilityState: 'hidden' as DocumentVisibilityState });
  const visible: boolean[] = [];
  const stop = followCameraVisibility(source, (value) => visible.push(value));
  assert.deepEqual(visible, [false]);
  stop();
});
