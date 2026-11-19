import { savePreferences, usePreference } from '../saved-preferences';
import { AFTER_SLICE_PREFERENCE, afterSlicePreference, type AfterSliceAction } from '../after-slice';
import { preloadable } from '../preloadable';
import { Check } from './Check';
import { Suspense, useState, useSyncExternalStore } from 'react';
import { markStale, setState, useStore } from '../state/store';
import { setThemePref, subscribeTheme, themePref, type ThemePref } from '../theme';
import { cameraPrefsSnapshot, setCameraPrefs, subscribeCamera, type CameraPrefs } from '../camera';
import { InfoTip } from './InfoTip';
import { Modal, ModalBody, ModalFooter, ModalTitle } from './Modal';
import { notificationSettingsSnapshot, setNotificationsEnabled, subscribeNotificationSettings } from '../notifications/browser';

const About = preloadable(() => import('./About'));
const PlateSettings = preloadable(() => import('./PlateSettings'));
const CloneDialog = preloadable(() => import('./CloneDialog'));
const SimplifyDialog = preloadable(() => import('./SimplifyDialog'));
const FilamentsDialog = preloadable(() => import('./FilamentsDialog'));
const ObjectTable = preloadable(() => import('./ObjectTable'));
const CreatePrinter = preloadable(() => import('./CreatePrinter'));
const SendDialog = preloadable(() => import('./SendDialog'));
const WhatYouCanDo = preloadable(() => import('./WhatYouCanDo'));
const MixedFilaments = preloadable(() => import('./MixedFilaments'));
const SetupWizard = preloadable(() => import('./FirstRun').then((m) => ({ default: m.SetupWizard })));

const close = () => setState({ appDialog: null });
const mod = /Mac|iPhone|iPad/.test(navigator.platform) ? 'Cmd' : 'Ctrl';

const THEMES: [ThemePref, string][] = [['system', 'System'], ['light', 'Light'], ['dark', 'Dark']];
const AFTER_SLICE_ACTIONS: [AfterSliceAction, string][] = [['export', 'Export'], ['send', 'Send to printer']];

function setDeveloperMode(developerMode: boolean) {
  savePreferences({'orca.developer_mode':developerMode ? '1' : '0'});
  setState({ developerMode });
}

function setMixedTemperatures(on: boolean) {
  savePreferences({'orca.enable_high_low_temp_mixed_printing':on ? '1' : '0'});
  setState((s) => ({ mixedTemperatures: on, job: markStale(s.job) }));
}

function Preferences() {
  const pref = useSyncExternalStore(subscribeTheme, themePref);
  const camera = useSyncExternalStore(subscribeCamera, cameraPrefsSnapshot);
  const developerMode = useStore((s) => s.developerMode);
  const mixed = useStore((s) => s.mixedTemperatures);
  const canMix = useStore((s) => !!s.features.mixed_temperatures);
  const afterSlice = afterSlicePreference(usePreference(AFTER_SLICE_PREFERENCE));
  const hasConfiguredPrinter = useStore((s) => s.printerConnections.connections.length > 0);
  const afterSliceNote = afterSlice === 'send' && !hasConfiguredPrinter;
  const notifications = useSyncExternalStore(subscribeNotificationSettings, notificationSettingsSnapshot);
  const notificationNote = notifications.permission === 'denied'
    ? 'Blocked by your browser. Allow notifications for this site from the address bar.'
    : notifications.permission === 'unsupported' ? 'Only works on an https address.' : null;
  return (
    <Modal size="m" label="Preferences" onClose={close}>
      <ModalTitle title="Preferences" onClose={close} />
      <ModalBody className="modal-fields">
        <div className="form-row"><span>Theme</span><span className="seg" role="radiogroup" aria-label="Theme">{THEMES.map(([value, label]) =>
          <button key={value} type="button" role="radio" aria-checked={pref === value} data-autofocus={pref === value ? '' : undefined} onClick={() => setThemePref(value)}>{label}</button>)}</span></div>
        <h3 className="modal-group">Camera</h3>
        <div className="form-row"><span>Mouse controls<InfoTip label="Mouse controls">{'Default: Left-drag to rotate, right- or middle-drag to pan.\nTouchpad: Alt+drag to rotate, Shift+drag to pan.'}</InfoTip></span>
          <span className="seg" role="radiogroup" aria-label="Camera style">{(['default', 'touchpad'] as const).map((value) =>
            <button key={value} type="button" role="radio" aria-checked={camera.style === value} onClick={() => setCameraPrefs({ style: value as CameraPrefs['style'] })}>{value === 'default' ? 'Default' : 'Touchpad'}</button>)}</span></div>
        <label className="form-row"><span>Orbit speed</span>
          <input className="text num" type="number" min={0.05} max={2} step={0.05} value={camera.orbitMult}
            onChange={(e) => { const v = Number(e.target.value); if (v >= 0.05 && v <= 2) setCameraPrefs({ orbitMult: v }); }} /></label>
        <label className="modal-check"><Check checked={camera.zoomToMouse} onCheckedChange={(next) => setCameraPrefs({ zoomToMouse: next })} />Zoom to mouse position</label>
        <label className="modal-check"><Check checked={camera.reverseZoom} onCheckedChange={(next) => setCameraPrefs({ reverseZoom: next })} />Reverse zoom direction</label>
        <h3 className="modal-group">Defaults</h3>
        <div className="after-slice-setting">
          <div className="form-row"><span>After slicing</span>
            <span className="seg" role="radiogroup" aria-label="After slicing" aria-describedby={afterSliceNote ? 'after-slice-note' : undefined}>{AFTER_SLICE_ACTIONS.map(([value, label]) =>
              <button key={value} type="button" role="radio" aria-checked={afterSlice === value} onClick={() => savePreferences({ [AFTER_SLICE_PREFERENCE]: value })}>{label}</button>)}</span></div>
          {afterSliceNote && <p id="after-slice-note" className="notification-setting-note after-slice-note">No printer set up yet, so Export stays primary.</p>}
        </div>
        <h3 className="modal-group">Notifications</h3>
        <div className="notification-setting">
          <label className="modal-check">
            <Check checked={notifications.enabled && !notifications.disabled} disabled={notifications.disabled}
              aria-describedby={notificationNote ? 'notification-setting-note' : undefined} onCheckedChange={setNotificationsEnabled} />
            Notify me when PancakeSlicer is in the background
          </label>
          {notificationNote && <p id="notification-setting-note" className="notification-setting-note">{notificationNote}</p>}
        </div>
        <h3 className="modal-group">Advanced</h3>
        <label className="modal-check"><Check checked={developerMode} onCheckedChange={(next) => setDeveloperMode(next)} />Developer mode</label>
        {canMix && <label className="modal-check"><Check checked={mixed} onCheckedChange={(next) => setMixedTemperatures(next)} />Allow mixed filament temperatures</label>}
      </ModalBody>
      <ModalFooter><button type="button" className="btn-primary" onClick={close}>Close</button></ModalFooter>
    </Modal>
  );
}

const SHORTCUTS: [string, [string, string][]][] = [
  ['Global', [
    [`${mod}+N`, 'New project'], [`${mod}+O`, 'Open project'], [`${mod}+S`, 'Save'], [`${mod}+Shift+S`, 'Save as'],
    [`${mod}+I`, 'Import models'], [`${mod}+G`, 'Export'],
    [`${mod}+Z`, 'Undo'], [`${mod}+Y / ${mod}+Shift+Z`, 'Redo'], [`${mod}+F`, 'Search settings'], [`${mod}+P`, 'Preferences'],
    [`${mod}+\\`, 'Hide / show sidebar'], ['Tab', 'Prepare / Preview (in a 3D view)'], ['Shift+Tab / Esc', 'Leave the 3D view'], ['?', 'Shortcuts'],
    ['0 … 6', 'Views: default, top, bottom, front, rear, left, right'], ['I / O', 'Zoom in / out'],
  ]],
  ['Prepare', [
    [`${mod}+A`, 'Select all'], ['Esc', 'Deselect'], [`${mod}+C / ${mod}+X / ${mod}+V`, 'Copy / cut / paste'], [`${mod}+K`, 'Clone'],
    ['Del / Backspace', 'Delete'], [`${mod}+D / ${mod}+Del`, 'Delete all'], ['+ / -', 'Add / remove instance'],
    ['V', 'Printable on / off'], ['F2', 'Rename'], ['A / Shift+A', 'Arrange all / this plate'], ['Q / Shift+Q', 'Orient selected / all'],
    ['Arrows', 'Move 10 mm (Shift: 1 mm)'], ['PgUp / PgDn', 'Rotate 45°'],
    ['M / R / S', 'Move / rotate / scale'], ['F', 'Lay on face'], ['C', 'Cut'], ['U', 'Measure'],
    ['B', 'Boolean'], ['T', 'Text'], ['E', 'Brim ears'], ['L / P / H / N', 'Paint support / seam / fuzzy skin / color'],
    [`${mod}+E`, 'Labels'], [`${mod}+Shift+Enter`, 'Wireframe'],
    ['Left drag', 'Orbit'], ['Right drag', 'Pan'], ['Wheel', 'Zoom'],
  ]],
  ['Painting', [
    ['C / S', 'Circle / sphere'], ['F / G', 'Fill / gap fill'], ['T / H', 'Triangle / height range'], ['1 … 9', 'Filament'],
  ]],
  ['Preview', [
    ['Up / Down', 'Layer up / down'], ['Left / Right', 'Move back / forward'], ['Home / End', 'First / last move'],
    ['L', 'One layer'], ['Shift+G', 'Go to layer'], ['C', 'G-code window'],
  ]],
];

function Keys({ keys }: { keys: string }) {
  return (
    <span className="keys">
      {keys.split(' / ').map((alt, i) => (
        <span key={i} className="keys-alt">
          {i > 0 && <span className="keys-or">/</span>}
          {(alt === '+' ? ['+'] : alt.split(/(?<=.)\+(?=.)/)).map((k) => <kbd key={k}>{k}</kbd>)}
        </span>
      ))}
    </span>
  );
}

function Shortcuts() {
  const [query, setQuery] = useState('');
  const q = query.trim().toLowerCase();
  const tab = useStore((s) => s.tab);
  const pages = [...SHORTCUTS].sort(([a], [b]) => Number(b.toLowerCase() === tab) - Number(a.toLowerCase() === tab))
    .map(([page, rows]) => [page, rows.filter(([k, what]) => !q || what.toLowerCase().includes(q) || k.toLowerCase().includes(q))] as const)
    .filter(([, rows]) => rows.length);
  return (
    <Modal size="xl" className="shortcuts-dialog" label="Keyboard shortcuts" onClose={close} backdropClose>
      <ModalTitle title="Keyboard shortcuts" onClose={close} />
      <div className="shortcuts-search">
        <input className="text" type="search" placeholder="Filter" aria-label="Filter shortcuts" value={query} data-autofocus onChange={(e) => setQuery(e.target.value)} />
      </div>
      <div className="shortcuts-body" tabIndex={0}>
        {pages.map(([page, rows]) => (
          <section key={page}>
            <h3>{page}</h3>
            <table className="shortcuts-table">
              <tbody>
                {rows.map(([key, what]) => (
                  <tr key={key + what}><th scope="row"><Keys keys={key} /></th><td>{what}</td></tr>
                ))}
              </tbody>
            </table>
          </section>
        ))}
        {!pages.length && <div className="shortcuts-none">No match</div>}
      </div>
    </Modal>
  );
}

export default function AppDialog({ which }: { which: 'about' | 'preferences' | 'shortcuts' | 'send' | 'setup' | 'what' | 'plate' | 'clone' | 'simplify' | 'filaments' | 'objectTable' | 'createPrinter' | 'mixed' }) {
  if (which === 'mixed') return <Suspense fallback={null}><MixedFilaments close={close} /></Suspense>;
  if (which === 'createPrinter') return <Suspense fallback={null}><CreatePrinter close={close} /></Suspense>;
  if (which === 'objectTable') return <Suspense fallback={null}><ObjectTable close={close} /></Suspense>;
  if (which === 'filaments') return <Suspense fallback={null}><FilamentsDialog close={close} /></Suspense>;
  if (which === 'simplify') return <Suspense fallback={null}><SimplifyDialog close={close} /></Suspense>;
  if (which === 'clone') return <Suspense fallback={null}><CloneDialog close={close} /></Suspense>;
  if (which === 'plate') return <Suspense fallback={null}><PlateSettings close={close} /></Suspense>;
  if (which === 'what') return <Suspense fallback={null}><WhatYouCanDo close={close} /></Suspense>;
  if (which === 'send') return <Suspense fallback={null}><SendDialog close={close} /></Suspense>;
  if (which === 'setup') return <Suspense fallback={null}><SetupWizard close={close} /></Suspense>;
  if (which === 'preferences') return <Preferences />;
  if (which === 'shortcuts') return <Shortcuts />;
  return <Suspense fallback={null}><About close={close} /></Suspense>;
}
