import { useRef, useState, type DragEventHandler, type KeyboardEvent, type MouseEvent, type ReactNode } from 'react';
import { Glyph } from '../components/Glyph';
import { clearUpload, type UploadTile } from './shelf';
import { displayName, entryPath, MODEL_EXTENSION, type ShelfEntry, type ShelfModel } from './tree';

export function FolderMark() {
  return <svg className="shelf-mark" viewBox="0 0 120 120" aria-hidden="true">
    <path d="M30 36c0-2.2 1.8-4 4-4h18l10 12H30V36Z" fill="var(--shelf-thumb-light)" />
    <path d="M24 44h72c3.3 0 6 2.7 6 6v36c0 3.3-2.7 6-6 6H24c-3.3 0-6-2.7-6-6V50c0-3.3 2.7-6 6-6Z" fill="var(--shelf-thumb-model)" />
    <path d="M18 62h84v24c0 3.3-2.7 6-6 6H24c-3.3 0-6-2.7-6-6V62Z" fill="var(--shelf-thumb-shade)" />
  </svg>;
}

function ZipMark() {
  return <svg className="shelf-mark" viewBox="0 0 120 120" aria-hidden="true">
    <path d="M40 22h28l16 16v60c0 3-2.4 5.5-5.5 5.5h-39C36.4 103.5 34 101 34 98V28c0-3.3 2.7-6 6-6Z" fill="var(--shelf-thumb-model)" />
    <path d="M68 22v16h16Z" fill="var(--shelf-thumb-light)" />
    <path d="M58 28v4M58 36v4M58 44v4M58 52v4M58 60v4" stroke="var(--shelf-thumb-bg)" strokeWidth="2.4" strokeLinecap="round" />
    <rect x="54" y="66" width="8" height="10" rx="1.2" fill="var(--shelf-thumb-light)" />
  </svg>;
}

function ModelPreview({ model }: { model: ShelfModel }) {
  const [failed, setFailed] = useState(false);
  return failed
    ? <span className="shelf-model-mark">
      <svg width="40" height="40" viewBox="0 0 40 40" aria-hidden="true">
        <path d="M8 26 L20 10 L32 26 L20 34 Z" fill="var(--shelf-thumb-model)" />
        <path d="M20 10 L32 26 L20 34 Z" fill="var(--shelf-thumb-light)" />
        <path d="M8 26 L20 10 L20 34 Z" fill="var(--shelf-thumb-shade)" />
      </svg>
    </span>
    : <img src={entryPath(model) + '/thumbnail.png'} alt="" draggable={false} loading="lazy" onError={() => setFailed(true)} />;
}

type TileProps = {
  entry: ShelfEntry;
  meta?: string;
  selected: boolean;
  menuOpen: boolean;
  editing: boolean;
  added: boolean;
  dropTarget: boolean;
  editor: ReactNode;
  onSelect(): void;
  onOpenMenu(trigger: HTMLButtonElement): void;
  onRename(): void;
  onDelete(): void;
  onDragStart: DragEventHandler<HTMLElement>;
  onDragEnd: DragEventHandler<HTMLElement>;
  onDragOver?: DragEventHandler<HTMLElement>;
  onDragLeave?: DragEventHandler<HTMLElement>;
  onDrop?: DragEventHandler<HTMLElement>;
};

export function ShelfTile({
  entry, meta, selected, menuOpen, editing, added, dropTarget, editor, onSelect, onOpenMenu, onRename, onDelete,
  onDragStart, onDragEnd, onDragOver, onDragLeave, onDrop,
}: TileProps) {
  const trigger = useRef<HTMLButtonElement>(null);
  const name = displayName(entry);
  const openMenu = (event: MouseEvent | KeyboardEvent) => {
    if (!trigger.current || editing) return;
    event.preventDefault();
    event.stopPropagation();
    onOpenMenu(trigger.current);
  };
  return <article data-shelf-id={entry.id} className={`shelf-tile${selected ? ' shelf-selected' : ''}${menuOpen ? ' shelf-menu-open' : ''}${added ? ' shelf-added' : ''}${dropTarget ? ' shelf-drop-target' : ''}${editing ? ' shelf-tile-editing' : ''}`}
    draggable={!editing} onDragStart={onDragStart} onDragEnd={onDragEnd} onDragOver={onDragOver} onDragLeave={onDragLeave} onDrop={onDrop}
    onContextMenu={openMenu}
    onKeyDown={(event) => {
      if (editing || event.ctrlKey || event.altKey || event.metaKey) return;
      if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) {
        openMenu(event);
      } else if (event.key === 'F2' || event.key === 'Delete') {
        event.preventDefault();
        event.stopPropagation();
        if (event.key === 'F2') onRename();
        else onDelete();
      }
    }}>
    <button className="shelf-tile-main" type="button" onClick={onSelect} tabIndex={editing ? -1 : undefined}
      aria-label={name} aria-pressed={entry.kind === 'model' ? selected : undefined}>
      <span className="shelf-thumb">
        {entry.kind === 'folder' ? <FolderMark /> : <ModelPreview model={entry} />}
        {added && <span className="shelf-check" aria-label="Added to plate">✓</span>}
      </span>
      {!editing && <span className="shelf-caption" title={name}>{name}</span>}
      {!editing && meta && <span className="shelf-meta" title={meta}>{meta}</span>}
    </button>
    {editing ? editor : <button ref={trigger} type="button" className="shelf-more" aria-label={`Actions for ${name}`}
      aria-haspopup="menu" aria-expanded={menuOpen} onClick={openMenu}><Glyph name="more" size={14} /></button>}
  </article>;
}

export const isZip = (file: File) => /\.zip$/i.test(file.name);

export function ShelfUpload({ tile }: { tile: UploadTile }) {
  const zip = isZip(tile.file);
  const name = zip ? tile.file.name : tile.file.name.replace(MODEL_EXTENSION, '') || tile.file.name;
  const { state } = tile;
  return <article data-upload-id={tile.key} className="shelf-tile shelf-upload">
    <div className="shelf-thumb">
      {state.kind === 'uploading' && <>
        {zip && <ZipMark />}
        <div className={`shelf-progress${zip ? ' shelf-progress-archive' : ''}`}>
          <progress max={1} value={state.progress ?? undefined} aria-label={`Uploading ${tile.file.name}`} />
        </div>
      </>}
      {state.kind === 'preview' && <div className="shelf-tile-status" role="status"><span className="shelf-shimmer" />Making preview</div>}
      {state.kind === 'failed' && <div className="shelf-tile-status" title={state.detail}>
        <span>Couldn’t read this file</span>
        <button type="button" className="shelf-link" onClick={() => clearUpload(tile.key)} aria-label={`Remove ${tile.file.name}`}>Remove</button>
      </div>}
    </div>
    <div className="shelf-caption" title={name}>{name}</div>
  </article>;
}
