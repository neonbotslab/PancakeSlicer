import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { canMove, displayName, folderPath, type Parent, type ShelfEntry, type ShelfTree } from './tree';

type MenuAction = { label: string; run(): void; danger?: true };

export function ShelfMenu({ trigger, name, actions, close }: {
  trigger: HTMLButtonElement; name: string; actions: readonly MenuAction[]; close(restoreFocus: boolean): void;
}) {
  const menu = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    const placeMenu = () => {
      trigger.closest('.shelf-tile')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
      const box = trigger.getBoundingClientRect();
      if (!menu.current) return;
      menu.current.style.setProperty('--shelf-menu-top', `${box.bottom + 2}px`);
      menu.current.style.right = `${Math.max(4, Math.min(window.innerWidth - box.right, window.innerWidth - 164))}px`;
    };
    placeMenu();
    menu.current?.querySelector('button')?.focus({ preventScroll: true });
    window.addEventListener('resize', placeMenu);
    window.addEventListener('scroll', placeMenu, true);
    return () => {
      window.removeEventListener('resize', placeMenu);
      window.removeEventListener('scroll', placeMenu, true);
    };
  }, [trigger]);

  useEffect(() => {
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !trigger.contains(event.target) && !menu.current?.contains(event.target)) close(false);
    };
    document.addEventListener('pointerdown', outside);
    return () => document.removeEventListener('pointerdown', outside);
  }, [close, trigger]);

  return <div ref={menu} className="shelf-menu" role="menu" aria-label={`Actions for ${name}`} onKeyDown={(event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      close(true);
      return;
    }
    if (event.key === 'Tab') {
      close(true);
      return;
    }
    const items = [...event.currentTarget.querySelectorAll('button')];
    const index = items.findIndex((item) => item === document.activeElement);
    const count = items.length;
    const next = {
      ArrowDown: (index + 1) % count,
      ArrowUp: (index + count - 1) % count,
      Home: 0,
      End: count - 1,
    };
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp' && event.key !== 'Home' && event.key !== 'End') return;
    event.preventDefault();
    items[next[event.key]]?.focus();
  }}>
    {actions.map(({ label, run, danger }) => <button key={label} className={danger ? 'shelf-danger-text' : undefined}
      type="button" role="menuitem" tabIndex={-1} onClick={() => { close(true); run(); }}>{label}</button>)}
  </div>;
}

function ShelfDialog({ label, alert, close, children }: { label: string; alert?: true; close(): void; children: ReactNode }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useLayoutEffect(() => {
    const node = dialog.current;
    if (!node) return;
    node.showModal();
    return () => node.close();
  }, []);
  return <dialog className="shelf-dialog" ref={dialog} role={alert ? 'alertdialog' : undefined} aria-label={label}
    onCancel={(event) => { event.preventDefault(); close(); }} onKeyDown={(event) => {
      event.stopPropagation();
      if (event.key !== 'Tab') return;
      const controls = [...event.currentTarget.querySelectorAll<HTMLElement>(':is(select, button):not(:disabled)')];
      const first = controls.at(0);
      const last = controls.at(-1);
      const boundary = event.shiftKey ? first : last;
      if (boundary !== document.activeElement) return;
      event.preventDefault();
      (event.shiftKey ? last : first)?.focus();
    }}>
    {children}
  </dialog>;
}

export function ShelfDeleteDialog({ question, close, remove }: { question: string; close(): void; remove(): void }) {
  return <ShelfDialog label={question} alert close={close}>
    <p>{question}</p>
    <div className="shelf-dialog-actions">
      <button type="button" className="shelf-button" onClick={close}>Cancel</button>
      <button type="button" className="shelf-button shelf-danger" onClick={remove}>Delete</button>
    </div>
  </ShelfDialog>;
}

export function ShelfMoveDialog({ tree, entry, close, move }: {
  tree: ShelfTree; entry: ShelfEntry; close(): void; move(parent: Parent): void;
}) {
  const label = `Move ${displayName(entry)}`;
  const selectId = useId();
  const folders = [...tree.entries.values()].flatMap((folder) => {
    if (folder.kind !== 'folder' || !canMove(tree, entry, folder.id)) return [];
    return [{ id: folder.id, path: folderPath(tree, folder.id).map(({ name }) => name).join(' / ') }];
  }).sort((a, b) => a.path.localeCompare(b.path));
  const options: { id: Parent; path: string }[] = canMove(tree, entry, '') ? [{ id: '', path: 'Shelf' }, ...folders] : folders;
  const [parent, setParent] = useState<Parent | null>(() => options[0]?.id ?? null);

  return <ShelfDialog label={label} close={close}>
    <p>{label}</p>
    {options.length ? <div className="shelf-move-folders">
      <label htmlFor={selectId}>Destination folder</label>
      <select id={selectId} size={Math.min(7, Math.max(2, options.length))} value={parent ?? ''}
        onChange={(event) => setParent(options.find((option) => option.id === event.currentTarget.value)?.id ?? null)}
        onDoubleClick={() => { if (parent !== null) move(parent); }}>
        {options.map(({ id, path }) => <option key={id} value={id}>{path}</option>)}
      </select>
    </div> : <p>No other folders</p>}
    <div className="shelf-dialog-actions">
      <button type="button" className="shelf-button" onClick={close}>Cancel</button>
      <button type="button" className="shelf-button shelf-primary" disabled={parent === null}
        onClick={() => { if (parent !== null) move(parent); }}>Move</button>
    </div>
  </ShelfDialog>;
}
