import { Fragment, useEffect, useLayoutEffect, useRef, useState, type DragEvent, type FormEvent } from 'react';
import { addShelfCopy, captureShelfDestination } from '../shelf-add';
import { getState, setState, useStore } from '../state/store';
import {
  createFolder, editEntry, refreshShelf, removeEntry, restoreEntry, uploadFiles, useShelf, withShelfModel, type ShelfReceipt,
} from './shelf';
import { ShelfDeleteDialog, ShelfMenu, ShelfMoveDialog } from './ShelfPopups';
import { FolderMark, ShelfTile, ShelfUpload, isZip } from './ShelfTile';
import { canMove, descendants, displayName, folderPath, MODEL_EXTENSION, searchTree, type EntryId, type Parent, type ShelfEntry, type ShelfModel } from './tree';
import './shelf.css';

const TILE_MIME = 'application/x-pancakeslicer-shelf-entry';
const ACCEPT = '.stl,.3mf,.step,.stp,.obj,.zip';
const BRIEF_NOTICE = 5000;
const UNDO_NOTICE = 10000;

type EditDraft = { key: number; text: string; busy: boolean; error: string } & (
  { kind: 'new'; parent: Parent } | { kind: 'rename'; entry: ShelfEntry }
);
type Notice = { key: number; text: string; expiresAt: number } & (
  { kind: 'message' } | { kind: 'added' } | { kind: 'deleted'; receipt: ShelfReceipt; busy: boolean }
);
type OpenDialog = { kind: 'move'; entry: ShelfEntry } | { kind: 'delete'; entry: ShelfEntry; question: string; revision: number };
type OpenMenu = { id: EntryId; trigger: HTMLButtonElement };

const messageFrom = (error: unknown, fallback: string) => error instanceof Error && error.message ? error.message : fallback;
const itemCount = (count: number) => `${count} ${count === 1 ? 'item' : 'items'}`;

function NameEditor({ draft, update, save, cancel }: {
  draft: EditDraft; update(text: string): void; save(): void; cancel(): void;
}) {
  const input = useRef<HTMLInputElement>(null);
  useLayoutEffect(() => {
    input.current?.focus();
    input.current?.select();
  }, []);
  useEffect(() => {
    if (draft.error) input.current?.focus();
  }, [draft.error]);
  return <form className="shelf-name-form" onSubmit={(event: FormEvent) => { event.preventDefault(); save(); }}>
    <input ref={input} value={draft.text} aria-label={draft.kind === 'rename' && draft.entry.kind === 'model' ? 'Model name' : 'Folder name'}
      placeholder={draft.kind === 'new' ? 'New folder' : undefined} readOnly={draft.busy}
      aria-invalid={!!draft.error} required onChange={(event) => update(event.currentTarget.value)}
      onKeyDown={(event) => {
        if (event.key === 'Escape') { event.preventDefault(); cancel(); }
        event.stopPropagation();
      }} />
    {draft.error && <span className="shelf-name-error" role="alert">{draft.error}</span>}
  </form>;
}

export default function ModelsPage() {
  const { tree, read, uploads, skipped, removing } = useShelf();
  const active = useStore((state) => state.tab === 'models');
  const [parent, setParent] = useState<Parent>('');
  const [query, setQuery] = useState('');
  const [selected, setSelected] = useState<EntryId | null>(null);
  const [menu, setMenu] = useState<OpenMenu | null>(null);
  const [dialog, setDialog] = useState<OpenDialog | null>(null);
  const [draft, setDraft] = useState<EditDraft | null>(null);
  const [notices, setNotices] = useState<Notice[]>([]);
  const [added, setAdded] = useState<{ id: EntryId; key: number } | null>(null);
  const [fileDrag, setFileDrag] = useState(false);
  const [dropTarget, setDropTarget] = useState<Parent | null>(null);
  const dragging = useRef<ShelfEntry | null>(null);
  const workingEdits = useRef(new Set<number>());
  const workingUndos = useRef(new Set<number>());
  const sequence = useRef(0);
  const page = useRef<HTMLElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const newFolderButton = useRef<HTMLButtonElement>(null);
  const currentParent = parent && tree?.entries.has(parent) ? parent : '';
  const path = tree ? folderPath(tree, currentParent) : [];
  const folderName = path.at(-1)?.name ?? 'Shelf';

  useEffect(() => {
    if (active) refreshShelf();
    else {
      setMenu(null);
      setDialog(null);
    }
  }, [active]);

  useEffect(() => {
    const firstExpiry = Math.min(...notices.map(({ expiresAt }) => expiresAt));
    if (!Number.isFinite(firstExpiry)) return;
    const timer = window.setTimeout(() => setNotices((items) => items.filter(({ expiresAt }) => expiresAt > Date.now())),
      Math.max(0, firstExpiry - Date.now()));
    return () => window.clearTimeout(timer);
  }, [notices]);

  useEffect(() => {
    if (!added) return;
    const timer = window.setTimeout(() => setAdded(null), 400);
    return () => window.clearTimeout(timer);
  }, [added]);

  useEffect(() => {
    if (!active) return;
    let depth = 0;
    const hasFiles = (event: globalThis.DragEvent) => event.dataTransfer?.types.includes('Files') === true;
    const enter = (event: globalThis.DragEvent) => {
      if (!hasFiles(event)) return;
      event.preventDefault();
      depth += 1;
      if (document.querySelector('dialog[open]')) return;
      setFileDrag(true);
    };
    const over = (event: globalThis.DragEvent) => {
      if (!hasFiles(event)) return;
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy';
      if (document.querySelector('dialog[open]')) return;
      setFileDrag(true);
    };
    const leave = (event: globalThis.DragEvent) => {
      if (!hasFiles(event)) return;
      depth = Math.max(0, depth - 1);
      if (!depth) setFileDrag(false);
    };
    const drop = (event: globalThis.DragEvent) => {
      if (!hasFiles(event)) return;
      event.preventDefault();
      depth = 0;
      setFileDrag(false);
      if (document.querySelector('dialog[open]')) return;
      if (event.dataTransfer) uploadFiles(event.dataTransfer.files, currentParent);
    };
    window.addEventListener('dragenter', enter);
    window.addEventListener('dragover', over);
    window.addEventListener('dragleave', leave);
    window.addEventListener('drop', drop);
    return () => {
      window.removeEventListener('dragenter', enter);
      window.removeEventListener('dragover', over);
      window.removeEventListener('dragleave', leave);
      window.removeEventListener('drop', drop);
      setFileDrag(false);
    };
  }, [active, currentParent]);

  const notify = (text: string, receipt?: ShelfReceipt) => {
    const key = ++sequence.current;
    const notice: Notice = receipt
      ? { key, text, kind: 'deleted', receipt, expiresAt: Math.min(receipt.expiresAt, Date.now() + UNDO_NOTICE), busy: false }
      : { key, text, kind: 'message', expiresAt: Date.now() + BRIEF_NOTICE };
    setNotices((items) => [...items, notice]);
  };

  const focusTile = (id: EntryId) => requestAnimationFrame(() => {
    if (getState().tab !== 'models') return;
    const button = page.current?.querySelector<HTMLButtonElement>(`[data-shelf-id="${CSS.escape(id)}"] .shelf-tile-main`);
    (button ?? newFolderButton.current)?.focus();
  });

  const navigate = (to: Parent) => {
    setParent(to);
    setQuery('');
    setDraft(null);
    setMenu(null);
    setSelected(null);
    requestAnimationFrame(() => {
      const crumb = page.current?.querySelector<HTMLButtonElement>('.shelf-crumb[aria-current="page"]');
      crumb?.focus();
      const body = page.current?.querySelector<HTMLElement>('.shelf-body');
      if (body) body.scrollTop = 0;
    });
  };

  const chooseFiles = () => fileInput.current?.click();
  const beginFolder = () => {
    setQuery('');
    setDraft({ key: ++sequence.current, kind: 'new', parent: currentParent, text: '', busy: false, error: '' });
    const body = page.current?.querySelector<HTMLElement>('.shelf-body');
    if (body) body.scrollTop = 0;
  };
  const beginRename = (entry: ShelfEntry) => {
    setSelected(entry.id);
    setDraft({ key: ++sequence.current, kind: 'rename', entry, text: displayName(entry), busy: false, error: '' });
  };
  const cancelEdit = () => {
    if (!draft || draft.busy) return;
    setDraft(null);
    if (draft.kind === 'rename') focusTile(draft.entry.id);
    else newFolderButton.current?.focus();
  };
  const saveEdit = async () => {
    if (!draft || workingEdits.current.has(draft.key)) return;
    const text = draft.text.trim();
    if (!text) {
      setDraft({ ...draft, error: 'Enter a name.' });
      return;
    }
    workingEdits.current.add(draft.key);
    setDraft({ ...draft, busy: true, error: '' });
    try {
      if (draft.kind === 'new') {
        const folder = await createFolder(draft.parent, text);
        focusTile(folder.id);
      } else {
        const entry = draft.entry;
        const extension = entry.kind === 'model' ? entry.name.match(MODEL_EXTENSION)?.[0] ?? '' : '';
        const name = extension && !MODEL_EXTENSION.test(text) ? text + extension : text;
        if (name !== entry.name) await editEntry(entry, { name });
        focusTile(entry.id);
      }
      setDraft((current) => current?.key === draft.key ? null : current);
    } catch (error) {
      setDraft((current) => current?.key === draft.key ? { ...current, busy: false, error: messageFrom(error, 'Couldn’t save the name.') } : current);
    } finally {
      workingEdits.current.delete(draft.key);
    }
  };
  const renderEditor = (current: EditDraft) => <NameEditor key={current.key} draft={current}
    update={(text) => setDraft({ ...current, text, error: '' })} save={() => void saveEdit()} cancel={cancelEdit} />;

  const deleteItem = async (entry: ShelfEntry, recursive: boolean, confirmedRevision?: number) => {
    setDialog(null);
    setSelected((id) => id === entry.id ? null : id);
    requestAnimationFrame(() => {
      if (getState().tab === 'models' && document.activeElement === document.body) newFolderButton.current?.focus();
    });
    try {
      const receipt = await removeEntry(entry, recursive, confirmedRevision);
      notify(`Deleted ${displayName(entry)}`, receipt);
    } catch (error) {
      notify(messageFrom(error, 'Couldn’t delete this item.'));
      focusTile(entry.id);
    }
  };
  const confirmDelete = (entry: ShelfEntry) => {
    if (!tree) return;
    const count = entry.kind === 'folder' ? descendants(tree, entry.id).length : 0;
    if (!count) {
      void deleteItem(entry, false);
      return;
    }
    setDialog({ kind: 'delete', entry, question: `Delete ${entry.name} and its ${itemCount(count)}?`, revision: tree.revision });
  };
  const doUndo = async (notice: Extract<Notice, { kind: 'deleted' }>) => {
    if (workingUndos.current.has(notice.key)) return;
    workingUndos.current.add(notice.key);
    setNotices((items) => items.map((item) => item.key === notice.key ? { ...notice, busy: true } : item));
    try {
      await restoreEntry(notice.receipt);
      setNotices((items) => items.filter((item) => item.key !== notice.key));
    } catch (error) {
      setNotices((items) => items.map((item) => item.key === notice.key ? { ...notice, busy: false } : item));
      notify(messageFrom(error, 'Couldn’t restore this item.'));
    } finally {
      workingUndos.current.delete(notice.key);
    }
  };

  const moveItem = async (entry: ShelfEntry, to: Parent) => {
    setDialog(null);
    try {
      await editEntry(entry, { parent: to });
    } catch (error) {
      notify(messageFrom(error, 'Couldn’t move this item.'));
    }
  };
  const addModel = async (entry: ShelfModel) => {
    const destination = captureShelfDestination();
    let name = displayName(entry);
    try {
      await withShelfModel(entry, async (model, revision) => {
        name = displayName(model);
        await addShelfCopy(model.id, revision, name, destination);
      });
      const key = ++sequence.current;
      setAdded({ id: entry.id, key });
      const toast: Notice = { key, kind: 'added', text: `Added ${name} to plate`, expiresAt: Date.now() + BRIEF_NOTICE };
      setNotices((items) => [...items.filter((item) => item.kind !== 'added'), toast]);
    } catch (error) {
      notify(messageFrom(error, 'Couldn’t add this model to the plate.'));
    }
  };

  const clearDrag = () => {
    dragging.current = null;
    setDropTarget(null);
  };
  const folderDrop = (to: Parent) => ({
    onDragOver: (event: DragEvent<HTMLElement>) => {
      const entry = dragging.current;
      if (!entry || !event.dataTransfer.types.includes(TILE_MIME) || !tree || !canMove(tree, entry, to)) return;
      event.preventDefault();
      event.stopPropagation();
      event.dataTransfer.dropEffect = 'move';
      setDropTarget(to);
    },
    onDragLeave: (event: DragEvent<HTMLElement>) => {
      if (event.relatedTarget instanceof Node && event.currentTarget.contains(event.relatedTarget)) return;
      setDropTarget((previous) => previous === to ? null : previous);
    },
    onDrop: (event: DragEvent<HTMLElement>) => {
      const entry = dragging.current;
      if (!entry || event.dataTransfer.getData(TILE_MIME) !== entry.id) return;
      event.preventDefault();
      event.stopPropagation();
      clearDrag();
      if (tree && canMove(tree, entry, to)) void moveItem(entry, to);
    },
  });

  const closeMenu = (restoreFocus: boolean) => {
    if (restoreFocus) menu?.trigger.focus({ preventScroll: true });
    setMenu(null);
  };
  const menuEntry = menu && tree?.entries.get(menu.id);
  const search = query.trim().toLocaleLowerCase();
  const results = tree
    ? (search ? searchTree(tree, search) : (tree.children.get(currentParent) ?? []).map((entry) => ({ entry, path: undefined })))
      .filter(({ entry }) => !removing.has(entry.id))
    : [];
  const pending = uploads.filter((tile) => tile.parent === currentParent && (!search || tile.file.name.toLocaleLowerCase().includes(search)));
  const newDraft = draft?.kind === 'new' && draft.parent === currentParent ? draft : null;
  const hasTiles = !!(results.length || pending.length || newDraft);
  const crumbList = [{ id: '', name: 'Shelf' }, ...path] satisfies { id: Parent; name: string }[];

  const renderTile = ({ entry, path }: { entry: ShelfEntry; path?: string }) => {
    const editing = draft?.kind === 'rename' && draft.entry.id === entry.id;
    const meta = path ?? (entry.kind === 'folder' && tree ? itemCount(descendants(tree, entry.id).length) : undefined);
    return <ShelfTile key={entry.id} entry={entry} meta={meta} editing={editing} editor={editing ? renderEditor(draft) : null}
      selected={selected === entry.id} menuOpen={menu?.id === entry.id} added={added?.id === entry.id ? added.key : null}
      dropTarget={entry.kind === 'folder' && dropTarget === entry.id}
      onSelect={() => { if (entry.kind === 'folder') navigate(entry.id); else setSelected(entry.id); }}
      onOpenMenu={(trigger) => {
        setSelected(entry.id);
        setMenu(menu?.id === entry.id ? null : { id: entry.id, trigger });
      }}
      onRename={() => beginRename(entry)} onDelete={() => confirmDelete(entry)}
      onDragStart={(event) => {
        dragging.current = entry;
        event.dataTransfer.setData(TILE_MIME, entry.id);
        event.dataTransfer.effectAllowed = 'move';
        setMenu(null);
      }} onDragEnd={clearDrag}
      {...(entry.kind === 'folder' ? folderDrop(entry.id) : {})} />;
  };

  return <main className="models-page" aria-label="Models" ref={page}>
    <header className="shelf-header">
      {search ? <div className="shelf-header-spacer" aria-hidden="true" /> : <nav className="shelf-breadcrumbs" aria-label="Folder path">
        {crumbList.map((crumb, index) => <Fragment key={crumb.id}>
          {!!index && <span className="shelf-separator" aria-hidden="true">/</span>}
          <button className={`shelf-crumb${dropTarget === crumb.id ? ' shelf-drop-target' : ''}`} type="button"
            aria-current={crumb.id === currentParent ? 'page' : undefined}
            onClick={() => navigate(crumb.id)} {...folderDrop(crumb.id)}>{crumb.name}</button>
        </Fragment>)}
      </nav>}
      <label className="shelf-search">
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" aria-hidden="true"><circle cx="6" cy="6" r="3.5" stroke="currentColor" strokeWidth="1.2" /><path d="m11 11-2.2-2.2" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" /></svg>
        <input type="search" aria-label="Search models" placeholder="Search" value={query} onChange={(event) => setQuery(event.currentTarget.value)} />
      </label>
      <button ref={newFolderButton} className="shelf-button shelf-folder-button" type="button" aria-label="New folder" title="New folder" onClick={beginFolder}>
        <svg viewBox="0 0 18 18" width="18" height="18" fill="none" aria-hidden="true"><path d="M2.5 5h4.3l1.4 1.7H15.5V14.5H2.5V5Z" stroke="currentColor" strokeWidth="1.6" strokeLinejoin="round" /><path d="M9 8.4v4.8M6.6 10.8h4.8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" /></svg>
      </button>
      <button className="shelf-button shelf-primary" type="button" onClick={chooseFiles}>Add</button>
      <input type="file" ref={fileInput} accept={ACCEPT} multiple hidden aria-label="Add models to shelf" onChange={(event) => {
        const input = event.currentTarget;
        if (input.files) uploadFiles(input.files, currentParent);
        input.value = '';
      }} />
    </header>
    {read === 'failed' && <p className="shelf-retry" role="status">Can’t reach the shelf. <button type="button" className="shelf-link" onClick={refreshShelf}>Try again</button></p>}
    <div className="shelf-body">
      {!tree && read === 'loading' && !hasTiles && <div className="shelf-grid" role="status" aria-label="Loading models">
        {Array.from({ length: 7 }, (_, index) => <div key={index} className="shelf-thumb shelf-skeleton" aria-hidden="true"><span className="shelf-shimmer" /></div>)}
      </div>}
      {hasTiles && <div className="shelf-grid">
        {newDraft && <article className="shelf-tile shelf-tile-editing"><div className="shelf-thumb"><FolderMark /></div>{renderEditor(newDraft)}</article>}
        {results.filter(({ entry }) => entry.kind === 'folder').map(renderTile)}
        {pending.filter(({ file }) => isZip(file)).map((tile) => <ShelfUpload key={tile.key} tile={tile} />)}
        {results.filter(({ entry }) => entry.kind === 'model').map(renderTile)}
        {pending.filter(({ file }) => !isZip(file)).map((tile) => <ShelfUpload key={tile.key} tile={tile} />)}
      </div>}
      {!hasTiles && tree && (search || currentParent
        ? <div className="shelf-empty shelf-empty-quiet"><p>{search ? 'No matching models or folders' : 'Nothing in this folder yet'}</p></div>
        : <div className="shelf-empty"><p>Drop models or a zip here</p><button type="button" className="shelf-button shelf-primary" onClick={chooseFiles}>Add</button></div>)}
      {skipped > 0 && <p className="shelf-skipped">Skipped {skipped} {skipped === 1 ? "file that isn't a model" : "files that aren't models"}</p>}
    </div>
    {fileDrag && <div className="shelf-drop-overlay"><span>Drop to add to {folderName}</span></div>}
    {menu && menuEntry && !removing.has(menuEntry.id) && <ShelfMenu trigger={menu.trigger} name={displayName(menuEntry)} close={closeMenu} actions={[
      menuEntry.kind === 'folder'
        ? { label: 'Open', run: () => navigate(menuEntry.id) }
        : { label: 'Add to Plate', run: () => void addModel(menuEntry) },
      { label: 'Rename', run: () => beginRename(menuEntry) },
      { label: 'Move to…', run: () => setDialog({ kind: 'move', entry: menuEntry }) },
      { label: 'Delete', danger: true, run: () => confirmDelete(menuEntry) },
    ]} />}
    {dialog?.kind === 'delete' && <ShelfDeleteDialog question={dialog.question} close={() => setDialog(null)} remove={() => void deleteItem(dialog.entry, true, dialog.revision)} />}
    {dialog?.kind === 'move' && tree && <ShelfMoveDialog tree={tree} entry={dialog.entry} close={() => setDialog(null)} move={(to) => void moveItem(dialog.entry, to)} />}
    {!!notices.length && <div className="shelf-notices">
      {notices.map((notice) => <div key={notice.key} className="shelf-notice" role="status">
        <span id={`shelf-notice-${notice.key}`}>{notice.text}</span>
        {notice.kind === 'deleted' && <button type="button" className="shelf-notice-action" disabled={notice.busy}
          aria-describedby={`shelf-notice-${notice.key}`} onClick={() => void doUndo(notice)}>Undo</button>}
        {notice.kind === 'added' && <button type="button" className="shelf-notice-action" onClick={() => {
          setNotices((items) => items.filter((item) => item.key !== notice.key));
          setState({ tab: 'prepare' });
        }}>View</button>}
      </div>)}
    </div>}
  </main>;
}
