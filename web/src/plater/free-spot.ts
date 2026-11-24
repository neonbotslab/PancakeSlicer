export interface Point2 { x: number; y: number }
export interface Footprint { minX: number; minY: number; maxX: number; maxY: number }
const EPS = 1e-6;

function cross(a: Point2, b: Point2, p: Point2): number {
  return (b.x - a.x) * (p.y - a.y) - (b.y - a.y) * (p.x - a.x);
}

function contains(point: Point2, polygon: readonly Point2[]): boolean {
  let inside = false;
  for (let i = 0, j = polygon.length - 1; i < polygon.length; j = i++) {
    const a = polygon[j], b = polygon[i];
    if (Math.abs(cross(a, b, point)) <= EPS && point.x >= Math.min(a.x, b.x) - EPS && point.x <= Math.max(a.x, b.x) + EPS && point.y >= Math.min(a.y, b.y) - EPS && point.y <= Math.max(a.y, b.y) + EPS) return true;
    if ((a.y > point.y) !== (b.y > point.y) && point.x < (b.x - a.x) * (point.y - a.y) / (b.y - a.y) + a.x) inside = !inside;
  }
  return inside;
}

const corners = (box: Footprint): Point2[] => [
  { x: box.minX, y: box.minY }, { x: box.maxX, y: box.minY },
  { x: box.maxX, y: box.maxY }, { x: box.minX, y: box.maxY },
];

function crosses(a: Point2, b: Point2, c: Point2, d: Point2): boolean {
  return cross(a, b, c) * cross(a, b, d) < -EPS && cross(c, d, a) * cross(c, d, b) < -EPS;
}

function crossesInterior(a: Point2, b: Point2, box: Footprint): boolean {
  let low = 0, high = 1;
  for (const [start, end, min, max] of [[a.x, b.x, box.minX, box.maxX], [a.y, b.y, box.minY, box.maxY]]) {
    const delta = end - start;
    if (Math.abs(delta) < EPS) {
      if (start <= min + EPS || start >= max - EPS) return false;
    } else {
      const t0 = (min + EPS - start) / delta, t1 = (max - EPS - start) / delta;
      low = Math.max(low, Math.min(t0, t1));
      high = Math.min(high, Math.max(t0, t1));
      if (low >= high) return false;
    }
  }
  return low < high;
}

export function footprintFits(box: Footprint, area: readonly Point2[]): boolean {
  if (area.length < 3) return false;
  const points = corners(box);
  if (!points.every((point) => contains(point, area))) return false;
  for (let i = 0; i < area.length; i++) {
    const a = area[i], b = area[(i + 1) % area.length];
    if (crossesInterior(a, b, box)) return false;
  }
  return true;
}

function excludedOverlap(box: Footprint, polygon: readonly Point2[]): boolean {
  if (polygon.length < 3) return false;
  if (corners(box).some((point) => contains(point, polygon))) return true;
  if (polygon.some((p) => p.x >= box.minX && p.x <= box.maxX && p.y >= box.minY && p.y <= box.maxY)) return true;
  const points = corners(box);
  return polygon.some((p, i) => points.some((q, j) => crosses(p, polygon[(i + 1) % polygon.length], q, points[(j + 1) % points.length])));
}

export function findFreeSpot({ width, depth, occupied, area, excluded = [], spacing = 3 }: {
  width: number; depth: number; occupied: readonly Footprint[]; area: readonly Point2[];
  excluded?: readonly (readonly Point2[])[]; spacing?: number;
}): Point2 | null {
  if (area.length < 3 || !Number.isFinite(width + depth) || width <= 0 || depth <= 0) return null;
  const minX = Math.min(...area.map((p) => p.x)) + width / 2;
  const maxX = Math.max(...area.map((p) => p.x)) - width / 2;
  const minY = Math.min(...area.map((p) => p.y)) + depth / 2;
  const maxY = Math.max(...area.map((p) => p.y)) - depth / 2;
  if (minX > maxX + EPS || minY > maxY + EPS) return null;
  const xs = new Set([minX, maxX, (minX + maxX) / 2]);
  const ys = new Set([minY, maxY, (minY + maxY) / 2]);
  for (const point of area) {
    xs.add(point.x - width / 2); xs.add(point.x + width / 2);
    ys.add(point.y - depth / 2); ys.add(point.y + depth / 2);
  }
  for (const polygon of excluded) for (const point of polygon) {
    xs.add(point.x - width / 2 - EPS * 10); xs.add(point.x + width / 2 + EPS * 10);
    ys.add(point.y - depth / 2 - EPS * 10); ys.add(point.y + depth / 2 + EPS * 10);
  }
  for (const box of occupied) {
    xs.add(box.minX - width / 2 - spacing); xs.add(box.maxX + width / 2 + spacing);
    ys.add(box.minY - depth / 2 - spacing); ys.add(box.maxY + depth / 2 + spacing);
  }
  const step = Math.max(2, Math.min(width, depth) / 4, (maxX - minX) / 150, (maxY - minY) / 150);
  for (let x = minX; x <= maxX; x += step) xs.add(x);
  for (let y = minY; y <= maxY; y += step) ys.add(y);
  const center = { x: (minX + maxX) / 2, y: (minY + maxY) / 2 };
  const fits = (point: Point2): boolean => {
    if (point.x < minX - EPS || point.x > maxX + EPS || point.y < minY - EPS || point.y > maxY + EPS) return false;
    const box = { minX: point.x - width / 2, maxX: point.x + width / 2, minY: point.y - depth / 2, maxY: point.y + depth / 2 };
    return footprintFits(box, area) && !excluded.some((polygon) => excludedOverlap(box, polygon)) && !occupied.some((other) =>
      box.minX < other.maxX + spacing - EPS && box.maxX > other.minX - spacing + EPS && box.minY < other.maxY + spacing - EPS && box.maxY > other.minY - spacing + EPS);
  };
  if (fits(center)) return center;
  const columns = [...xs].sort((a, b) => a - b);
  for (const y of [...ys].sort((a, b) => b - a)) {
    for (const x of columns) if (fits({ x, y })) return { x, y };
  }
  return null;
}
