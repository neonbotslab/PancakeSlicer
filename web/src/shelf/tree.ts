declare const folderId: unique symbol;
declare const modelId: unique symbol;
export type FolderId = string & { readonly [folderId]: true };
type ModelId = string & { readonly [modelId]: true };
export type Parent = FolderId | '';

type EntryDetails = { parent: Parent; name: string };
export type ShelfFolder = EntryDetails & { kind: 'folder'; id: FolderId };
export type ShelfModel = EntryDetails & { kind: 'model'; id: ModelId };
export type ShelfEntry = ShelfFolder | ShelfModel;
export type EntryId = ShelfEntry['id'];
export type ShelfTree = {
  revision: number;
  entries: ReadonlyMap<EntryId, ShelfEntry>;
  children: ReadonlyMap<Parent, readonly ShelfEntry[]>;
};

export const MODEL_EXTENSION = /\.(stl|3mf|step|stp|obj)$/i;
export const displayName = (entry: ShelfEntry) => entry.kind === 'model' ? entry.name.replace(MODEL_EXTENSION, '') || entry.name : entry.name;
export const entryPath = (entry: ShelfEntry) => `/api/shelf/${entry.kind}s/${encodeURIComponent(entry.id)}`;

function isFolderId(value: unknown): value is FolderId {
  return typeof value === 'string' && value.startsWith('folder:') && value.length > 'folder:'.length;
}

function isModelId(value: unknown): value is ModelId {
  return typeof value === 'string' && value.startsWith('model:') && value.length > 'model:'.length;
}

export function object(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error('Invalid shelf response.');
  return { ...value };
}

export function rows(value: unknown): unknown[] {
  if (Array.isArray(value)) return value;
  throw new Error('Invalid shelf response.');
}

export function parseRevision(revision: unknown): number {
  if (typeof revision === 'number' && Number.isSafeInteger(revision) && revision >= 0) return revision;
  throw new Error('Invalid shelf revision.');
}

export function parseEntry(value: unknown, kind: 'folder'): ShelfFolder;
export function parseEntry(value: unknown, kind: 'model'): ShelfModel;
export function parseEntry(value: unknown, kind: ShelfEntry['kind']): ShelfEntry {
  const { id, parent, name } = object(value);
  if (!(parent === '' || isFolderId(parent)) || typeof name !== 'string' || !name) throw new Error('Invalid shelf entry.');
  if (kind === 'folder' && isFolderId(id)) return { kind, id, parent, name };
  if (kind === 'model' && isModelId(id)) return { kind, id, parent, name };
  throw new Error('Invalid shelf entry ID.');
}

const byName = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });
function compareEntries(a: ShelfEntry, b: ShelfEntry) {
  if (a.kind !== b.kind) return a.kind === 'folder' ? -1 : 1;
  return byName.compare(displayName(a), displayName(b));
}

export function makeTree(revision: number, values: Iterable<ShelfEntry>): ShelfTree {
  const entries = new Map<EntryId, ShelfEntry>();
  const children = new Map<Parent, ShelfEntry[]>();
  for (const entry of [...values].sort(compareEntries)) {
    entries.set(entry.id, entry);
    const siblings = children.get(entry.parent);
    if (siblings) siblings.push(entry);
    else children.set(entry.parent, [entry]);
  }
  return { revision, entries, children };
}

export function parseTree(value: unknown): ShelfTree {
  const { revision, folders, models } = object(value);
  return makeTree(parseRevision(revision), [
    ...Object.values(object(folders)).map((folder) => parseEntry(folder, 'folder')),
    ...Object.values(object(models)).map((model) => parseEntry(model, 'model')),
  ]);
}

export function folderPath(tree: ShelfTree, id: Parent): ShelfFolder[] {
  const path: ShelfFolder[] = [];
  while (id && path.length < 32) {
    const folder = tree.entries.get(id);
    if (folder?.kind !== 'folder') break;
    path.push(folder);
    id = folder.parent;
  }
  return path.reverse();
}

export function descendants(tree: ShelfTree, parent: FolderId): ShelfEntry[] {
  const entries = [...tree.children.get(parent) ?? []];
  for (const entry of entries) {
    if (entry.kind === 'folder') entries.push(...tree.children.get(entry.id) ?? []);
  }
  return entries;
}

export function searchTree(tree: ShelfTree, query: string) {
  return [...tree.entries.values()]
    .filter((entry) => entry.name.toLocaleLowerCase().includes(query))
    .map((entry) => ({ entry, path: folderPath(tree, entry.parent).map((folder) => folder.name).join(' / ') || 'Shelf' }));
}

export function canMove(tree: ShelfTree, entry: ShelfEntry, destination: Parent) {
  if (destination === entry.parent) return false;
  if (!destination) return true;
  const path = folderPath(tree, destination);
  return path.at(-1)?.id === destination && path.every((folder) => folder.id !== entry.id);
}
