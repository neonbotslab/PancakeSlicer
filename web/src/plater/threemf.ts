import { unzip, strFromU8 } from 'fflate';
import type { Config } from '../api/types';
import type { SeamMode, VolumeType } from '../state/store';

export interface ProjectPlate { index: number; name: string; origin: [number, number]; settings: Config;  locked?: boolean }

export interface ProjectMaterials {
  plates: ProjectPlate[];
  skipped?: string[];
  config: Config;
  colours: string[];
  matrix: number[] | null;
  multiplier: number | null;
  hasConfig: boolean;
  builds: { object: number; instance: number }[];
}

export interface ProjectItem {
  id: string;
  instance: number;
  buildIndex: number;
  plateIndex: number;
  name: string;
  extruder: number;
  printable: boolean;
  settings: Config;
  origin: [number, number, number];
  rotZ: number;
  transform: number[];
  parts: number;
  paint?: ArchivePaint;
  partTris?: number[];
  // The primary mesh of this build item appears first in the items array.
  part?: { parent: number; type: VolumeType; seamMode?: SeamMode };
}

export type ArchivePaint = Partial<Record<'support' | 'seam' | 'fuzzy' | 'color', Record<string, string>>>;
const PAINT_ATTRS = [['support', 'paint_supports'], ['seam', 'paint_seam'], ['fuzzy', 'paint_fuzzy_skin'], ['color', 'paint_color']] as const;

interface Obj {
  name?: string;
  vertices?: Float32Array;
  triangles?: Uint32Array;
  paint?: ArchivePaint;
  components: { objectid: string; path?: string; transform: number[] }[];
}

interface PartSettings {
  name?: string;
  extruder?: number;
  type: VolumeType;
  seamMode?: SeamMode;
}

const PART_TYPES = new Map<string, VolumeType>([
  ['negative_part', 'negative'], ['modifier_part', 'modifier'],
  ['support_blocker', 'support_blocker'], ['support_enforcer', 'support_enforcer'],
]);

interface ObjectSettings {
  name?: string;
  extruder?: number;
  settings: Config;
  volumes: Set<string>;
  parts: Map<string, PartSettings[]>;
}

type Mesh = { id: string; object: Obj; transform: number[] };

const IDENTITY = [1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0];

function parseTransform(t: string | undefined): number[] {
  const v = (t ?? '').trim().split(/\s+/).map(Number);
  return v.length === 12 && v.every(Number.isFinite) ? v : IDENTITY;
}

function compose(a: number[], b: number[]): number[] {
  const out = new Array(12).fill(0);
  for (let r = 0; r < 4; r++)
    for (let c = 0; c < 3; c++) {
      let s = r === 3 ? b[9 + c] : 0;
      for (let k = 0; k < 3; k++) s += a[r * 3 + k] * b[k * 3 + c];
      out[r * 3 + c] = s;
    }
  return out;
}

const attr = (tag: string, name: string) => {
  const m = new RegExp(`\\s${name}\\s*=\\s*([\"'])(.*?)\\1`).exec(tag);
  return m ? unescapeXml(m[2]) : undefined;
};

function parseModel(text: string): { objects: Map<string, Obj>; build: { objectid: string; transform: number[]; printable: boolean }[] } {
  const unit = attr(/<model\b([^>]*)>/.exec(text)?.[1] ?? '', 'unit') ?? 'millimeter';
  const factor = ({ micron: 0.001, millimeter: 1, centimeter: 10, inch: 25.4, foot: 304.8, meter: 1000 } as Record<string, number>)[unit];
  if (!factor) throw new Error(`Unsupported measurement unit in 3MF file: ${unit}`);
  const transform = (value: string | undefined) => parseTransform(value).map((v, i) => i >= 9 ? v * factor : v);
  const objects = new Map<string, Obj>();
  const objRe = /<object\b([^>]*)>([\s\S]*?)<\/object>/g;
  for (let m; (m = objRe.exec(text)); ) {
    const head = m[1], body = m[2];
    const id = attr(head, 'id');
    if (!id) continue;
    const obj: Obj = { name: attr(head, 'name'), components: [] };
    const vStart = body.indexOf('<vertices');
    if (vStart >= 0) {
      const vEnd = body.indexOf('</vertices>', vStart);
      const vText = body.slice(vStart, vEnd);
      const verts: number[] = [];
      for (const v of vText.matchAll(/<vertex\b([^>]*)\/?>/g)) {
        const xyz = ['x', 'y', 'z'].map((key) => Number(attr(v[1], key)) * factor);
        if (!xyz.every(Number.isFinite)) throw new Error('Corrupted vertex data in 3MF file');
        verts.push(...xyz);
      }
      const tText = body.slice(body.indexOf('<triangles', vEnd), body.indexOf('</triangles>', vEnd));
      const tris: number[] = [];
      for (const t of tText.matchAll(/<triangle\b([^>]*)\/?>/g)) {
        if (t[1].includes('paint_')) for (const [kind, key] of PAINT_ATTRS) {
          const value = attr(t[1], key);
          if (value) ((obj.paint ??= {})[kind] ??= {})[String(tris.length / 3)] = value;
        }
        const indices = ['v1', 'v2', 'v3'].map((key) => Number(attr(t[1], key)));
        if (!indices.every((v) => Number.isInteger(v) && v >= 0 && v < verts.length / 3)) throw new Error('Corrupted triangle data in 3MF file');
        tris.push(...indices);
      }
      obj.vertices = new Float32Array(verts);
      obj.triangles = new Uint32Array(tris);
    }
    const cRe = /<component\b([^>]*)\/?>/g;
    for (let c; (c = cRe.exec(body)); ) {
      const objectid = attr(c[1], 'objectid');
      if (objectid) obj.components.push({ objectid, path: attr(c[1], '(?:[\\w-]+:)?path'), transform: transform(attr(c[1], 'transform')) });
    }
    objects.set(id, obj);
  }
  const build: { objectid: string; transform: number[]; printable: boolean }[] = [];
  const buildText = /<build\b[^>]*>([\s\S]*?)<\/build>/.exec(text)?.[1] ?? '';
  const iRe = /<item\b([^>]*)\/?>/g;
  for (let i; (i = iRe.exec(buildText)); ) {
    const objectid = attr(i[1], 'objectid');
    if (objectid) build.push({ objectid, transform: transform(attr(i[1], 'transform')), printable: attr(i[1], 'printable') !== '0' });
  }
  return { objects, build };
}

function normalizePath(path: string): string {
  const parts: string[] = [];
  for (const part of path.split('/')) {
    if (part === '..') parts.pop();
    else if (part && part !== '.') parts.push(part);
  }
  return parts.join('/');
}

function parseSettings(text: string | undefined): Map<string, ObjectSettings> {
  const out = new Map<string, ObjectSettings>();
  if (!text) return out;
  for (const object of text.matchAll(/<object\b([^>]*)>([\s\S]*?)<\/object>/g)) {
    const id = attr(object[1], 'id');
    if (!id) continue;
    const own = object[2].split(/<(?:part|volume)\b/)[0];
    const metadata = (body: string): Config => {
      const values: Config = {};
      for (const m of body.matchAll(/<metadata\b([^>]*?)(?:\/>|>([\s\S]*?)<\/metadata>)/g)) {
        const key = attr(m[1], 'key');
        if (key) values[key] = attr(m[1], 'value') ?? unescapeXml(m[2] ?? '');
      }
      return values;
    };
    const values = metadata(own);
    const parts = new Map<string, PartSettings[]>();
    const volumes = new Set<string>();
    for (const part of object[2].matchAll(/<part\b([^>]*?)(?:\/>|>([\s\S]*?)<\/part>)/g)) {
      const partId = attr(part[1], 'id');
      if (!partId) continue;
      const subtype = attr(part[1], 'subtype') ?? '';
      if (subtype !== 'normal_part') volumes.add(partId);
      const values = metadata(part[2] ?? '');
      const extruder = Number(values.extruder);
      const seam = /^precise_seam_(center|left|right|enforced|blocked|neutral)$/.exec(subtype);
      const partSettings: PartSettings = {
        name: values.name === undefined ? undefined : String(values.name),
        extruder: Number.isSafeInteger(extruder) && extruder > 0 ? extruder : undefined,
        type: seam ? 'seam' : PART_TYPES.get(subtype) ?? 'part',
        ...(seam ? { seamMode: seam[1] as SeamMode } : {}),
      };
      const matches = parts.get(partId);
      if (matches) matches.push(partSettings);
      else parts.set(partId, [partSettings]);
    }
    const firstVolume = /<volume\b[^>]*>([\s\S]*?)<\/volume>/.exec(object[2])?.[1];
    const extruder = Number(values.extruder ?? (firstVolume ? metadata(firstVolume).extruder : undefined)) || undefined;
    out.set(id, { name: values.name === undefined ? undefined : String(values.name), extruder, parts,
      settings: Object.fromEntries(Object.entries(values).filter(([k]) => k !== 'name' && k !== 'extruder')), volumes });
  }
  return out;
}

const unescapeXml = (s: string) => s.replace(/&quot;/g, '"').replace(/&apos;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');

export async function readProject(file: File, objectsOnly = false): Promise<{ items: ProjectItem[]; materials: ProjectMaterials; mesh(i: number): Float32Array | null }> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  const zip = await new Promise<Record<string, Uint8Array>>((resolve, reject) => unzip(bytes, (err, data) => (err ? reject(err) : resolve(data))));
  const entry = (p: string) => zip[p.replace(/^\//, '')] ?? Object.entries(zip).find(([k]) => k.toLowerCase() === p.replace(/^\//, '').toLowerCase())?.[1];
  const rels = entry('_rels/.rels');
  const rootPath = (rels && /Target="([^"]+\.model)"/i.exec(strFromU8(rels))?.[1]) || '/3D/3dmodel.model';
  const rootBytes = entry(rootPath);
  if (!rootBytes) throw new Error(`${file.name}: no 3D models found inside this archive`);
  const models = new Map<string, ReturnType<typeof parseModel>>();
  const model = (path: string) => {
    let m = models.get(path);
    if (!m) {
      const bytes = entry(path);
      m = bytes ? parseModel(strFromU8(bytes)) : { objects: new Map(), build: [] };
      models.set(path, m);
    }
    return m;
  };
  const root = model(rootPath);
  const settingsBytes = entry('Metadata/model_settings.config') ?? entry('Metadata/Slic3r_PE_model.config');
  const settings = parseSettings(settingsBytes && strFromU8(settingsBytes));

  function* meshes(path: string, id: string, t: number[], depth = 0): Generator<Mesh> {
    const o = model(path).objects.get(id);
    if (!o) throw new Error(`Missing 3D mesh object #${id} in 3MF file`);
    if (depth > 64) throw new Error('This 3MF file has circular or overly nested part references.');
    if (o.vertices && o.triangles?.length) yield { id, object: o, transform: t };
    for (const c of o.components) {
      const childPath = c.path ? normalizePath(/^(?:\/|3D\/)/i.test(c.path) ? c.path : `${path.slice(0, path.lastIndexOf('/') + 1)}${c.path}`) : path;
      yield* meshes(childPath, c.objectid, compose(c.transform, t), depth + 1);
    }
  }

  const configBytes = entry('Metadata/project_settings.config');
  let config: Config = {};
  try { config = configBytes ? JSON.parse(strFromU8(configBytes)) : {}; }
  catch (error) { if (!objectsOnly) throw error; }
  const skipped = new Set<string>();
  if (configBytes || entry('Metadata/Slic3r_PE.config') || settingsBytes) skipped.add('settings');
  if (Object.keys(zip).some((p) => /\.(png|jpg|jpeg)$/i.test(p))) skipped.add('thumbnails');
  const metadata = entry('Metadata/model_settings.config');
  const plateText = metadata ? strFromU8(metadata) : '';
  const membership = new Map<string, number>();
  const plates: ProjectPlate[] = [];
  for (const p of plateText.matchAll(/<plate\b[^>]*>([\s\S]*?)<\/plate>/g)) {
    const own = p[1].split(/<model_instance\b/)[0];
    const values: Config = {};
    for (const m of own.matchAll(/<metadata\b([^>]*)\/?>/g)) {
      const key = attr(m[1], 'key'), value = attr(m[1], 'value');
      if (key && value !== undefined) values[key] = unescapeXml(value);
    }
    const index = Number(values.plater_id);
    if (!Number.isInteger(index) || index < 1) continue;
    const settings = { ...values };
    for (const [stored, native] of [['filament_maps', 'filament_map'], ['filament_volume_maps', 'filament_volume_map'], ['filament_nozzle_maps', 'filament_nozzle_map']]) {
      if (settings[stored] !== undefined) { settings[native] = settings[stored]; delete settings[stored]; }
    }
    if (settings.bed_type && settings.bed_type !== 'Default Plate') settings.curr_bed_type = settings.bed_type;
    delete settings.bed_type;
    for (const k of ['plater_id', 'plater_name', 'locked']) delete settings[k];
    plates.push({ index, name: String(values.plater_name ?? ''), origin: [0, 0], settings, ...(values.locked === 'true' ? { locked: true } : {}) });
    for (const inst of p[1].matchAll(/<model_instance\b[^>]*>([\s\S]*?)<\/model_instance>/g)) {
      const v = (key: string) => new RegExp(`<metadata\\s+key="${key}"\\s+value="([^"]*)"`).exec(inst[1])?.[1];
      membership.set(`${v('object_id')}:${v('instance_id')}`, index);
    }
  }
  if (!plates.length) plates.push({ index: 1, name: '', origin: [0, 0], settings: {} });
  plates.sort((a, b) => a.index - b.index);
  const points = (Array.isArray(config.printable_area) ? config.printable_area : String(config.printable_area ?? '').split(',')).map((s) => String(s).split('x').map(Number));
  const width = Math.max(...points.map((p: number[]) => p[0])) - Math.min(...points.map((p: number[]) => p[0]));
  const depth = Math.max(...points.map((p: number[]) => p[1])) - Math.min(...points.map((p: number[]) => p[1]));
  const columns = Math.ceil(Math.sqrt(Math.max(...plates.map((p) => p.index))));
  for (const p of plates) p.origin = [((p.index - 1) % columns) * width * 1.2 || 0, -Math.floor((p.index - 1) / columns) * depth * 1.2 || 0];
  const counts = new Map<string, number>();
  const build = root.build;
  const items: ProjectItem[] = [];
  const itemMeshes: Mesh[][] = [];
  for (const [i, b] of build.entries()) {
    const meta = settings.get(b.objectid);
    const instance = counts.get(b.objectid) ?? 0;
    counts.set(b.objectid, instance + 1);
    const plateIndex = membership.get(`${b.objectid}:${instance}`) ?? (objectsOnly || plates.length === 1 ? plates[0].index : 0);
    if (!plateIndex && !objectsOnly) throw new Error('Couldn\'t match every model in the file to a saved build plate.');
    const plate = plates.find((p) => p.index === plateIndex);
    const list = [...meshes(rootPath, b.objectid, b.transform)];
    const item = {
      id: b.objectid, instance, buildIndex: i, plateIndex,
      printable: b.printable,
      settings: objectsOnly ? {} : meta?.settings ?? {},
      name: meta?.name ?? root.objects.get(b.objectid)?.name ?? `Object ${i + 1}`,
      extruder: meta?.extruder ?? 1,
      origin: [b.transform[9] - (plate?.origin[0] ?? 0), b.transform[10] - (plate?.origin[1] ?? 0), b.transform[11]] as [number, number, number],
      rotZ: Math.atan2(b.transform[1], b.transform[0]) * 180 / Math.PI,
      transform: loadedItem(b),
    };
    if (!objectsOnly || !list.length) {
      items.push({ ...item, ...volumesOf(b.objectid, list.map((leaf) => leaf.object)) });
      itemMeshes.push(list);
      continue;
    }
    const partIndices = new Map<string, number>();
    const parts = list.map((leaf) => {
      const index = partIndices.get(leaf.id) ?? 0;
      partIndices.set(leaf.id, index + 1);
      const matches = meta?.parts.get(leaf.id);
      return { leaf, settings: matches?.[index] ?? matches?.[0] };
    });
    const main = parts.findIndex(({ settings }) => !settings || settings.type === 'part');
    if (main < 0) {
      items.push({ ...item, parts: 0 });
      itemMeshes.push([]);
      continue;
    }
    const parent = items.length;
    const primary = parts[main];
    const extruder = primary.settings?.extruder ?? item.extruder;
    items.push({ ...item, extruder, parts: parts.length, paint: primary.leaf.object.paint });
    itemMeshes.push([primary.leaf]);
    for (const [k, { leaf, settings }] of parts.entries()) {
      if (k === main) continue;
      const type = settings?.type ?? 'part';
      const fallback = type === 'part' || type === 'modifier' ? item.extruder : extruder;
      items.push({ ...item, name: settings?.name || leaf.object.name || `Part ${k + 1}`,
        extruder: settings?.extruder ?? (fallback !== extruder ? fallback : 0),
        parts: 1, paint: leaf.object.paint, part: { parent, type, seamMode: settings?.seamMode } });
      itemMeshes.push([leaf]);
    }
  }
  function volumesOf(id: string, list: Obj[]): { parts: number; paint?: ArchivePaint; partTris?: number[] } {
    if (list.length === 1) return { parts: 1, ...(list[0].paint ? { paint: list[0].paint } : {}) };
    const nonParts = settings.get(id)?.volumes;
    const comps = root.objects.get(id)?.components ?? [];
    if (!list.length || root.objects.get(id)?.vertices || comps.length !== list.length || comps.some((c) => nonParts?.has(c.objectid)))
      return { parts: list.length };
    const paint: ArchivePaint = {};
    let offset = 0;
    for (const leaf of list) {
      for (const [kind, map] of Object.entries(leaf.paint ?? {}) as [keyof ArchivePaint, Record<string, string>][])
        for (const [k, v] of Object.entries(map)) (paint[kind] ??= {})[String(Number(k) + offset)] = v;
      offset += leaf.triangles!.length / 3;
    }
    return { parts: list.length, partTris: list.map((leaf) => leaf.triangles!.length / 3), ...(Object.keys(paint).length ? { paint } : {}) };
  }
  function loadedItem(b: { objectid: string; transform: number[] }) {
    const comps = root.objects.get(b.objectid)?.components ?? [];
    if (comps.length !== 1) return b.transform;
    const t = compose(comps[0].transform, b.transform);
    return [...b.transform.slice(0, 9), t[9], t[10], t[11]];
  }
  const mesh = (i: number) => {
    const tris: number[] = [];
    const origin = plates.find((p) => p.index === items[i].plateIndex)?.origin ?? [0, 0];
    for (const { object, transform: t } of itemMeshes[i]) {
      const v = object.vertices!, triangles = object.triangles!;
      const mirrored = t[0] * (t[4] * t[8] - t[5] * t[7]) - t[3] * (t[1] * t[8] - t[2] * t[7]) + t[6] * (t[1] * t[5] - t[2] * t[4]) < 0;
      for (let corner = 0; corner < triangles.length; corner++) {
        const k = triangles[mirrored && corner % 3 !== 0 ? corner + (corner % 3 === 1 ? 1 : -1) : corner];
        const x = v[3 * k], y = v[3 * k + 1], z = v[3 * k + 2];
        tris.push(x * t[0] + y * t[3] + z * t[6] + t[9] - origin[0], x * t[1] + y * t[4] + z * t[7] + t[10] - origin[1], x * t[2] + y * t[5] + z * t[8] + t[11]);
      }
    }
    return tris.length ? new Float32Array(tris) : null;
  };
  const numbers = (v: unknown): number[] | null => {
    const list = Array.isArray(v) ? v.map(Number) : typeof v === 'string' ? v.split(/[,;]/).map(Number) : [];
    return list.length && list.every(Number.isFinite) ? list : null;
  };
  const colourValue = config.filament_colour;
  const colours = (Array.isArray(colourValue) ? colourValue : typeof colourValue === 'string' ? colourValue.split(';') : [])
    .map(String);
  const materials = { plates, config, colours, matrix: numbers(config.flush_volumes_matrix), multiplier: numbers(config.flush_multiplier)?.[0] ?? null,
    hasConfig: !!configBytes || !!entry('Metadata/Slic3r_PE.config'), builds: items.filter((it) => !it.part).map((it) => ({ object: Number(it.id), instance: it.instance })) };
  if (objectsOnly) {
    materials.config = {}; materials.colours = []; materials.matrix = null; materials.multiplier = null; materials.hasConfig = false;
    materials.plates = plates.map((p) => ({ index: p.index, name: '', origin: p.origin, settings: {} }));
    return { items, materials: { ...materials, skipped: [...skipped] }, mesh };
  }
  return { items, materials, mesh };
}
