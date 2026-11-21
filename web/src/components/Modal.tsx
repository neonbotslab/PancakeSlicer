import { useEffect, useState, useSyncExternalStore, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { Glyph } from './Glyph';
import { Dialog } from 'radix-ui';
import { DeviceSwitchToast, useDeviceSwitchCountdown } from './DeviceSwitchToast';
import './dialog.css';

export type ModalSize = 's' | 'm' | 'l' | 'xl' | 'window';
const FIELD = ':is(input:not([type=hidden], [type=checkbox], [type=radio]), textarea, select):not(:disabled, [aria-hidden=true], [tabindex="-1"])';
function focusFirst(root: HTMLElement) {
  const footer = root.querySelector('.modal-buttons');
  const primary = footer?.querySelector<HTMLElement>('.btn-primary:not(:disabled), .btn-danger:not(:disabled)');
  const target = root.querySelector<HTMLElement>('[data-autofocus]')
    ?? [...root.querySelectorAll<HTMLElement>(FIELD)].find((el) => !el.closest('.modal-title, .modal-buttons') && el.offsetParent !== null)
    ?? (primary?.classList.contains('btn-danger') ? footer?.querySelector<HTMLElement>('button:not(.btn-danger):not(:disabled)') : primary)
    ?? root;
  target.focus({ preventScroll: true });
}

const activeModals = new Set<symbol>();
const modalListeners = new Set<() => void>();
const subscribeModals = (l: () => void) => { modalListeners.add(l); return () => { modalListeners.delete(l); }; };
const notifyModals = () => modalListeners.forEach((l) => l());
export const useModalCount = () => useSyncExternalStore(subscribeModals, () => activeModals.size);

function ModalNotice() {
  const [id] = useState(() => Symbol());
  const top = useSyncExternalStore(subscribeModals, () => [...activeModals].at(-1));
  const seconds = useDeviceSwitchCountdown();
  useEffect(() => {
    activeModals.add(id);
    notifyModals();
    return () => { activeModals.delete(id); notifyModals(); };
  }, [id]);
  return top === id && seconds !== null ? <div className="modal-notice"><DeviceSwitchToast seconds={seconds} /></div> : null;
}
const inTopLayer = (e: Event) => !!(e.target as Element | null)?.closest?.('.top-layer');

export function Modal({ label, onClose, className = '', size = 'window', alert = false, backdropClose = false, returnFocus, onEscapeKeyDown, children }: {
  label: string; onClose(): void; className?: string; size?: ModalSize; alert?: boolean; backdropClose?: boolean; returnFocus?: string;
  onEscapeKeyDown?(event: KeyboardEvent): void; children: ReactNode;
}) {
  return <Dialog.Root open onOpenChange={(open) => { if (!open) onClose(); }}><Dialog.Portal><Dialog.Overlay className="modal-overlay" />
    <Dialog.Content className={`modal ${className}`} data-size={size} role={alert ? 'alertdialog' : 'dialog'} aria-label={label} aria-describedby={undefined}
      onOpenAutoFocus={(e) => { e.preventDefault(); focusFirst(e.currentTarget as HTMLElement); }} onEscapeKeyDown={onEscapeKeyDown}
      onPointerDownOutside={(e) => { if (!backdropClose || inTopLayer(e)) e.preventDefault(); }} onFocusOutside={(e) => { if (inTopLayer(e)) e.preventDefault(); }}
      onCloseAutoFocus={(e) => { const back = returnFocus ? document.querySelector<HTMLElement>(returnFocus) : null; if (back) { e.preventDefault(); back.focus(); } }}
      onKeyDown={(e) => {
        const radioArrow = e.defaultPrevented && e.key.startsWith('Arrow') && e.target instanceof Element && e.target.closest('[role="radiogroup"]');
        if (!radioArrow) e.stopPropagation();
      }}>{children}<ModalNotice /></Dialog.Content></Dialog.Portal></Dialog.Root>;
}
export function ModalTitle({ title, onClose }: { title: ReactNode; onClose(): void }) {
  return <div className="modal-title"><Dialog.Title asChild><h2>{title}</h2></Dialog.Title><button type="button" className="modal-close" aria-label="Close" onClick={onClose}><Glyph name="close" size={14} /></button></div>;
}
export function ModalBody({ className = '', children }: { className?: string; children: ReactNode }) { return <div className={`modal-body ${className}`}>{children}</div>; }
export function ModalFooter({ lead, children }: { lead?: ReactNode; children: ReactNode }) {
  return <div className="modal-buttons">{lead && <div className="modal-lead">{lead}</div>}{children}</div>;
}
export function InTopLayer({ children }: { children: ReactNode }) {
  const open = useModalCount();
  return open ? createPortal(<div className="top-layer">{children}</div>, document.body, String(open)) : <>{children}</>;
}
