import { useSyncExternalStore } from 'react';
import { failureCode, mutation, responseError } from '../api/access';
import {
  entryPath, makeTree, object, parseEntry, parseRevision, parseTree, rows,
  type EntryId, type Parent, type ShelfEntry, type ShelfModel, type ShelfTree,
} from './tree';

type UploadState = { kind: 'uploading'; progress: number | null } | { kind: 'preview' } | { kind: 'failed'; detail: string };
export type UploadTile = { key: number; file: File; parent: Parent; state: UploadState };
export type ShelfReceipt = { token: string; expiresAt: number };
type ShelfSnapshot = {
  tree: ShelfTree | null;
  read: 'loading' | 'ready' | 'failed';
  uploads: readonly UploadTile[];
  skipped: number;
  removing: ReadonlySet<EntryId>;
};

let snapshot: ShelfSnapshot = { tree: null, read: 'loading', uploads: [], skipped: 0, removing: new Set() };
const listeners = new Set<() => void>();
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
};
const shelfSnapshot = () => snapshot;
export const useShelf = () => useSyncExternalStore(subscribe, shelfSnapshot);
function update(patch: Partial<ShelfSnapshot>) {
  snapshot = { ...snapshot, ...patch };
  listeners.forEach((listener) => listener());
}

let queue: Promise<unknown> = Promise.resolve();
function serialize<T>(operation: () => Promise<T>): Promise<T> {
  const next = queue.then(operation);
  queue = next.catch(() => undefined);
  return next;
}

async function readTree() {
  try {
    const response = await fetch('/api/shelf?tree=1', { cache: 'no-store' });
    if (!response.ok) throw await responseError(response);
    const tree = parseTree(await response.json());
    update({ tree, read: 'ready' });
    return tree;
  } catch (error) {
    update({ read: 'failed' });
    throw error;
  }
}

let readPending = false;
export function refreshShelf() {
  if (readPending) return;
  readPending = true;
  update({ read: 'loading' });
  void serialize(readTree).catch(() => undefined).finally(() => { readPending = false; });
}

function write<T>(operation: (tree: ShelfTree) => Promise<T>) {
  return serialize(async () => {
    const tree = snapshot.tree ?? await readTree();
    try {
      return await operation(tree);
    } catch (error) {
      if (failureCode(error) === 'revision_conflict') await readTree().catch(() => undefined);
      throw error;
    }
  });
}

const currentEntry = (tree: ShelfTree, entry: ShelfEntry) => {
  const current = tree.entries.get(entry.id);
  if (!current) throw new Error('This item is no longer on the shelf.');
  return current;
};

async function jsonRequest(path: string, method: string, body: object): Promise<unknown> {
  const response = await mutation(path, {
    method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  }, false, undefined, undefined, true);
  return response.json();
}

const replaceTree = (tree: ShelfTree) => update({ tree, read: 'ready' });

export function createFolder(parent: Parent, name: string) {
  return write(async (tree) => {
    const response = object(await jsonRequest('/api/shelf/folders', 'POST', { name, parent, expectedRevision: tree.revision }));
    const folder = parseEntry(response.folder, 'folder');
    replaceTree(makeTree(parseRevision(response.revision), [...tree.entries.values(), folder]));
    return folder;
  });
}

export function editEntry(entry: ShelfEntry, change: { name: string } | { parent: Parent }) {
  return write(async (tree) => {
    const current = currentEntry(tree, entry);
    replaceTree(parseTree(await jsonRequest(entryPath(current), 'PATCH', { ...change, expectedRevision: tree.revision })));
  });
}

function parseReceipt(value: unknown): ShelfReceipt {
  const { token, expiresAt } = object(value);
  const expiry = typeof expiresAt === 'string' ? Date.parse(expiresAt) : NaN;
  if (typeof token !== 'string' || !token || !Number.isFinite(expiry)) throw new Error('Invalid shelf Undo receipt.');
  return { token, expiresAt: expiry };
}

function markRemoving(id: EntryId, removing: boolean) {
  const next = new Set(snapshot.removing);
  if (removing) next.add(id);
  else next.delete(id);
  update({ removing: next });
}

export async function removeEntry(entry: ShelfEntry, recursive: boolean, confirmedRevision?: number) {
  markRemoving(entry.id, true);
  try {
    return await write(async (tree) => {
      if (confirmedRevision !== undefined && tree.revision !== confirmedRevision) throw new Error('The shelf changed. Check the folder contents before deleting it again.');
      const current = currentEntry(tree, entry);
      const path = entryPath(current) + (recursive ? '?recursive=true' : '');
      const response = await mutation(path, { method: 'DELETE', headers: { 'If-Match': String(tree.revision) } },
        false, undefined, undefined, true);
      const result = object(await response.json());
      const receipt = parseReceipt(result.undo);
      replaceTree(parseTree(result.tree));
      return receipt;
    });
  } finally {
    markRemoving(entry.id, false);
  }
}

export function restoreEntry(receipt: ShelfReceipt) {
  return write(async (tree) => {
    const path = `/api/shelf/trash/${encodeURIComponent(receipt.token)}/restore`;
    replaceTree(parseTree(await jsonRequest(path, 'POST', { expectedRevision: tree.revision })));
  });
}

export async function withShelfModel(model: ShelfModel, operation: (model: ShelfModel, revision: number) => Promise<void>) {
  const selected = await write(async (tree) => {
    const current = currentEntry(tree, model);
    if (current.kind !== 'model') throw new Error('This model is no longer on the shelf.');
    return { model: current, revision: tree.revision };
  });
  await operation(selected.model, selected.revision);
}

function uploadState(key: number, state: UploadState) {
  update({ uploads: snapshot.uploads.map((tile) => tile.key === key ? { ...tile, state } : tile) });
}

export function clearUpload(key: number) {
  update({ uploads: snapshot.uploads.filter((tile) => tile.key !== key) });
}

function uploadTile(tile: UploadTile) {
  const body = new FormData();
  body.append('file', tile.file);
  const path = `/api/shelf/uploads?folder=${encodeURIComponent(tile.parent)}`;
  void write(async (tree) => {
    const response = await mutation(path, { method: 'POST', headers: { 'If-Match': String(tree.revision) }, body },
      false, undefined, (progress) => uploadState(tile.key, progress >= 1 ? { kind: 'preview' } : { kind: 'uploading', progress }), true);
    const result = object(await response.json());
    const added = [
      ...rows(result.folders).map((folder) => parseEntry(folder, 'folder')),
      ...rows(result.imported).map((item) => parseEntry(object(item).model, 'model')),
    ];
    const next = makeTree(parseRevision(result.revision), [...tree.entries.values(), ...added]);
    update({
      tree: next,
      read: 'ready',
      skipped: snapshot.skipped + rows(result.skipped).length,
      uploads: snapshot.uploads.filter((item) => item.key !== tile.key),
    });
  }).catch((error: unknown) => uploadState(tile.key, { kind: 'failed', detail: error instanceof Error ? error.message : '' }));
}

let nextUpload = 0;
export function uploadFiles(files: Iterable<File>, parent: Parent) {
  const tiles = Array.from(files, (file): UploadTile => ({ key: ++nextUpload, file, parent, state: { kind: 'uploading', progress: null } }));
  if (!tiles.length) return;
  const skipped = snapshot.uploads.some((tile) => tile.state.kind !== 'failed') ? snapshot.skipped : 0;
  update({ uploads: [...snapshot.uploads, ...tiles], skipped });
  tiles.forEach(uploadTile);
}
