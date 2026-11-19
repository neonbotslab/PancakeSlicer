import { useCallback, useEffect, useRef, useState } from 'react';
import { responseError } from '../api/access';
import { Icon } from '../components/Icon';
import { Glyph } from '../components/Glyph';
import { LiveCamera } from './LiveCamera';
import { followCameraVisibility, readCameras, type Camera } from './camera-stream';

export type { Camera } from './camera-stream';

type CameraState = { kind: 'loading' | 'off' | 'reconnecting' } | { kind: 'ready'; cameras: Camera[]; camera: Camera };

function useCamera(printer: string, enabled: boolean) {
  const [visible, setVisible] = useState(() => document.visibilityState === 'visible');
  const [state, setCamera] = useState<CameraState>({ kind: 'loading' });
  const [selected, select] = useState(''), [attempt, retry] = useState(0);
  const [ratio, setRatio] = useState(16 / 9);
  const lastFrame = useRef(0), failed = useRef(false);
  useEffect(() => followCameraVisibility(document, setVisible), []);
  const active = enabled && visible;
  const reconnect = useCallback(() => { failed.current = true; setCamera({ kind: 'reconnecting' }); }, []);
  const frame = useCallback(() => { lastFrame.current = Date.now(); }, []);
  useEffect(() => {
    if (!active) { setCamera({ kind: 'loading' }); return; }
    const abort = new AbortController();
    failed.current = false; lastFrame.current = Date.now();
    setCamera({ kind: attempt ? 'reconnecting' : 'loading' });
    void fetch(`/api/printer-connections/${encodeURIComponent(printer)}/camera`, { signal: abort.signal }).then(async (response) => {
      if (!response.ok) throw await responseError(response);
      const cameras = readCameras(await response.json());
      if (abort.signal.aborted || failed.current) return;
      const camera = cameras.find((item) => item.id === selected) ?? cameras[0];
      setCamera(camera ? { kind: 'ready', cameras, camera } : { kind: 'off' });
    }).catch(() => { if (!abort.signal.aborted) reconnect(); });
    return () => abort.abort();
  }, [printer, active, attempt, selected, reconnect]);
  useEffect(() => {
    if (!active || state.kind !== 'ready') return;
    const timer = window.setInterval(() => { if (Date.now() - lastFrame.current > 12000) reconnect(); }, 1000);
    return () => window.clearInterval(timer);
  }, [active, state.kind, reconnect]);
  useEffect(() => {
    if (!active || state.kind !== 'reconnecting') return;
    const timer = window.setTimeout(() => retry((n) => n + 1), 2000);
    return () => window.clearTimeout(timer);
  }, [active, state.kind, attempt]);
  const stream = active && state.kind === 'ready'
    ? <LiveCamera key={`${printer}:${state.camera.url}:${attempt}`} camera={state.camera} onError={reconnect} onRatio={setRatio} onFrame={frame} /> : null;
  return { state, stream, ratio, select, retry: () => retry((n) => n + 1), active };
}

export function OverviewCamera({ printer, offline }: { printer: string; offline: boolean }) {
  const camera = useCamera(printer, !offline);
  return <div className="overview-camera" aria-label="Camera">
    {offline ? <span>Offline</span> : <>{camera.stream}
      {camera.state.kind === 'off' && <span>Camera off</span>}
      {camera.active && camera.state.kind === 'reconnecting' && <small className="camera-reconnecting">reconnecting</small>}</>}
  </div>;
}

function PrinterStill({ cover }: { cover?: string }) {
  const [failed, fail] = useState<string>();
  return cover && failed !== cover ? <img className="device-camera-still" src={cover} alt="" onError={() => fail(cover)} /> : null;
}

export function DeviceCamera({ printer, cover, light, lightLabel, cameraLabel, busy, onLight, offline = false }: { printer: string; cover?: string; light?: boolean; lightLabel: string; cameraLabel?: string; busy: boolean; onLight(): void; offline?: boolean }) {
  const frame = useRef<HTMLElement>(null);
  const [watching, watch] = useState(true), [fullscreenError, setFullscreenError] = useState('');
  const camera = useCamera(printer, watching && !offline);
  const fullscreen = () => {
    const action = document.fullscreenElement ? document.exitFullscreen() : frame.current?.requestFullscreen();
    void action?.catch(() => setFullscreenError('Fullscreen isn\'t available right now'));
  };
  if (offline || camera.state.kind === 'off') return <section className="device-camera device-camera-off" aria-label="Camera"><PrinterStill cover={cover} /><span className="device-camera-empty">{offline ? 'Offline' : 'Camera off'}</span></section>;
  return <section ref={frame} className={`device-camera ${watching ? '' : 'camera-hidden'}`} aria-label="Camera">
    {!watching ? <div className="device-camera-hidden"><span>Camera hidden</span><button type="button" className="btn slant" onClick={() => watch(true)}>Show camera</button></div>
      : <><div className="device-camera-frame" style={{ aspectRatio: camera.ratio }}>{camera.stream}
        {camera.active && camera.state.kind === 'reconnecting' && <small className="camera-reconnecting">reconnecting</small>}
      </div><div className="device-camera-toolbar">
        <span className="device-camera-label">{cameraLabel ?? (camera.state.kind === 'ready' ? camera.state.camera.name : 'Camera')}</span>
        {camera.state.kind === 'ready' && camera.state.cameras.length > 1 && <select aria-label="Camera" value={camera.state.camera.id} onChange={(e) => camera.select(e.target.value)}>{camera.state.cameras.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select>}
        {light != null && <button type="button" aria-label={lightLabel} aria-pressed={light} disabled={busy} onClick={onLight}><Icon name={light ? 'monitor_lamp_on' : 'monitor_lamp_off'} size={18} /></button>}
        <button type="button" aria-label="Fullscreen camera" onClick={fullscreen}><Glyph name="fullscreen" size={18} /></button>
        <button type="button" aria-label="Hide camera" onClick={() => watch(false)}><Glyph name="minus" size={18} /></button>
      </div>{fullscreenError && <span className="device-camera-note" role="status">{fullscreenError}</span>}</>}
  </section>;
}
