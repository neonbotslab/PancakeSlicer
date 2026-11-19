import { useLayoutEffect, useRef, type ComponentProps } from 'react';
import { showDevice } from '../flow/project';
import { deviceHref } from '../flow/routes';
import type { PrinterConnections } from './connections';
import { OverviewCamera } from './DeviceCamera';
import { overviewStatus } from './overview-status';

type OverviewPrinter = { printer: string; name: string; model: string };

const labels = {
  printing: 'Printing', paused: 'Paused', finished: 'Finished', idle: 'Idle', failed: 'Failed', offline: 'Offline',
};
const tones = { printing: 'printing', paused: 'paused', finished: 'online', idle: undefined, failed: 'error', offline: 'offline' };
const SCROLL = 'printerOverviewScroll';
const FROM_OVERVIEW = 'fromPrinterOverview';
const clock = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });

export function DeviceLink({ printer = null, onClick, ...props }: Omit<ComponentProps<'a'>, 'href'> & { printer?: string | null }) {
  return <a {...props} href={deviceHref(printer)} onClick={(event) => {
    if (event.button !== 0 || event.metaKey || event.altKey || event.ctrlKey || event.shiftKey) return;
    onClick?.(event);
    if (event.defaultPrevented) return;
    event.preventDefault();
    const scroll: unknown = history.state?.[SCROLL];
    showDevice(printer);
    if (typeof scroll === 'number') history.replaceState({ ...history.state, [SCROLL]: scroll }, '');
  }} />;
}

export function AllPrintersLink() {
  return <DeviceLink className="link" onClick={(event) => {
    if (history.state?.[FROM_OVERVIEW] !== true) return;
    event.preventDefault();
    history.back();
  }}>← All printers</DeviceLink>;
}

export function DeviceOverview({ printers, connections }: { printers: readonly OverviewPrinter[]; connections: PrinterConnections }) {
  const pane = useRef<HTMLElement>(null);
  useLayoutEffect(() => {
    const scroll: unknown = history.state?.[SCROLL];
    if (pane.current && typeof scroll === 'number') pane.current.scrollTop = scroll;
  }, []);

  return <main ref={pane} className="printer-overview" aria-labelledby="printer-overview-heading">
    <h1 id="printer-overview-heading">All printers</h1>
    <div className="printer-overview-grid">
      {printers.map(({ printer, name, model }) => {
        const card = overviewStatus(connections.live, connections.status[printer]);
        const offline = card.kind === 'offline';
        const issue = !offline && card.detail.kind === 'issue' ? card.detail : undefined;
        const lastSeen = offline && card.lastSeen ? Date.parse(card.lastSeen) : NaN;
        const line = offline ? Number.isFinite(lastSeen) ? `Last seen ${clock.format(lastSeen)}` : 'Last seen unknown' : card.detail.text;
        return <DeviceLink className="printer-overview-card" key={printer} printer={printer} data-offline={offline || undefined}
          onClick={(event) => {
            event.preventDefault();
            const scroll = pane.current?.scrollTop ?? 0;
            history.replaceState({ ...history.state, [SCROLL]: scroll }, '');
            showDevice(printer);
            history.replaceState({ ...history.state, [SCROLL]: scroll, [FROM_OVERVIEW]: true }, '');
          }}>
          <OverviewCamera printer={printer} offline={offline} />
          <div className="printer-overview-info">
            <div className="printer-overview-line">
              <span className="printer-overview-identity">
                {issue && <span aria-hidden="true" className="printer-overview-error-dot" />}
                <strong title={name}>{name}</strong>
                {model && <span className="printer-overview-model" title={model}>{model}</span>}
              </span>
              <span className="status-text" data-tone={issue ? 'error' : tones[card.kind]}>{labels[card.kind]}</span>
            </div>
            <div className="printer-overview-line printer-overview-secondary">
              <span title={line || undefined}>{line}</span>
              {!!issue?.more && <span>+{issue.more} more</span>}
            </div>
            <div className="printer-overview-line printer-overview-progress">
              {(card.kind === 'printing' || card.kind === 'paused') && <>
                {card.progress !== undefined && <progress max="1" value={card.progress} aria-label="Print progress" />}
                {card.progressText && <span>{card.progressText}</span>}
              </>}
            </div>
          </div>
        </DeviceLink>;
      })}
    </div>
  </main>;
}
