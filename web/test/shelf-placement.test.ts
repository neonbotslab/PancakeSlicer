import assert from 'node:assert/strict';
import { test } from 'node:test';
import { findFreeSpot, footprintFits, type Footprint, type Point2 } from '../src/plater/free-spot.ts';

const square = [{ x: 0, y: 0 }, { x: 100, y: 0 }, { x: 100, y: 100 }, { x: 0, y: 100 }];
const bounds = (at: Point2, width: number, depth = width): Footprint => ({
  minX: at.x - width / 2, maxX: at.x + width / 2, minY: at.y - depth / 2, maxY: at.y + depth / 2,
});

test('successive copies fit without moving or overlapping existing footprints', () => {
  const occupied = [bounds({ x: 50, y: 50 }, 30)];
  const original = structuredClone(occupied);
  for (let i = 0; i < 5; i++) {
    const at = findFreeSpot({ width: 20, depth: 20, occupied, area: square });
    assert.ok(at);
    const next = bounds(at, 20);
    assert.ok(footprintFits(next, square));
    for (const box of occupied) assert.ok(next.maxX <= box.minX - 3 + 1e-6 || next.minX >= box.maxX + 3 - 1e-6 || next.maxY <= box.minY - 3 + 1e-6 || next.minY >= box.maxY + 3 - 1e-6);
    occupied.push(next);
  }
  assert.deepEqual(occupied[0], original[0]);
});

test('a full bed fails and an exact empty fit succeeds', () => {
  assert.equal(findFreeSpot({ width: 10, depth: 10, occupied: [bounds({ x: 50, y: 50 }, 100)], area: square }), null);
  assert.deepEqual(findFreeSpot({ width: 100, depth: 100, occupied: [], area: square }), { x: 50, y: 50 });
  assert.equal(findFreeSpot({ width: 101, depth: 100, occupied: [], area: square }), null);
});

test('a concave notch cannot cross a placed rectangle even with all corners inside', () => {
  const area = [
    { x: 0, y: 0 }, { x: 100, y: 0 }, { x: 100, y: 100 },
    { x: 55, y: 100 }, { x: 55, y: 40 }, { x: 45, y: 40 }, { x: 45, y: 100 }, { x: 0, y: 100 },
  ];
  assert.equal(footprintFits({ minX: 20, minY: 20, maxX: 80, maxY: 80 }, area), false);
  const at = findFreeSpot({ width: 20, depth: 20, occupied: [], area });
  assert.ok(at);
  assert.ok(footprintFits(bounds(at, 20), area));
});

test('excluded areas remain clear and edge space is available', () => {
  const at = findFreeSpot({ width: 30, depth: 30, occupied: [], area: square, excluded: [[{ x: 25, y: 25 }, { x: 75, y: 25 }, { x: 75, y: 75 }, { x: 25, y: 75 }]] });
  assert.equal(at, null);
  const edge = findFreeSpot({ width: 20, depth: 20, occupied: [{ minX: 0, minY: 0, maxX: 77, maxY: 100 }], area: square });
  assert.ok(edge);
  assert.equal(edge.x, 90);
});

test('a notch entering through footprint corners still prevents placement', () => {
  const area = [[-2, -2], [4, -2], [4, 4], [3, 4], [1, 0], [-1, 4], [-2, 4]].map(([x, y]) => ({ x, y }));
  assert.equal(footprintFits({ minX: 0, minY: 0, maxX: 2, maxY: 2 }, area), false);
  const at = findFreeSpot({ width: 2, depth: 2, occupied: [], area });
  assert.ok(at);
  assert.ok(footprintFits(bounds(at, 2), area));
});

test('empty and invalid bed inputs do not produce a placement', () => {
  for (const area of [[], square.slice(0, 2)]) assert.equal(findFreeSpot({ width: 20, depth: 20, occupied: [], area }), null);
  for (const width of [0, -1, Infinity, NaN]) assert.equal(findFreeSpot({ width, depth: 20, occupied: [], area: square }), null);
});

test('narrow space bounded by bed vertices remains available', () => {
  const area = [[0, 0], [10, 0], [10, 1], [2.8, 1], [2.8, 4], [0.6, 4], [0.6, 1], [0, 1]].map(([x, y]) => ({ x, y }));
  const at = findFreeSpot({ width: 2, depth: 2, occupied: [], area });
  assert.ok(at);
  assert.ok(footprintFits(bounds(at, 2), area));
});
