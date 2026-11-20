import { createMachine } from './fsm';
export type ProjectRoute = { kind: 'blank' } | { kind: 'projects' } | { kind: 'project'; id: string }
  | { kind: 'device'; printer: string | null } | { kind: 'missing' };
const hashed = () => location.protocol === 'file:' || import.meta.env.MODE === 'demo';
export const projectHref = (id: string) => `${hashed() ? '#' : ''}/p/${encodeURIComponent(id)}`;
export const projectsHref = () => `${hashed() ? '#' : ''}/projects`;
export const devicePath = (printer: string | null = null) => printer === null ? '/device' : `/device/${encodeURIComponent(printer)}`;
export const deviceHref = (printer: string | null = null) => `${hashed() ? '#' : ''}${devicePath(printer)}`;
function parseRoute(): ProjectRoute {
  const path = hashed() ? location.hash.slice(1) || '/' : location.pathname;
  if (path === '/') return { kind: 'blank' };
  if (path === '/projects' || path === '/projects/') return { kind: 'projects' };
  if (path === '/device' || path === '/device/') return { kind: 'device', printer: null };
  const match = /^\/(p|device)\/([^/]+)\/?$/.exec(path);
  if (!match) return { kind: 'missing' };
  try {
    const key = decodeURIComponent(match[2]);
    return match[1] === 'device' ? { kind: 'device', printer: key } : { kind: 'project', id: key };
  } catch { return { kind: 'missing' }; }
}
export const urlRoute = (): ProjectRoute => parseRoute();
export const urlProject = (): string | null => { const r = parseRoute(); return r.kind === 'project' ? r.id : null; };
export function writeRoute(path: string, replace = false) {
  const href = `${hashed() ? '#' : ''}${path}`;
  const current = hashed() ? location.hash : location.pathname;
  if (href === current) return;
  if (replace) history.replaceState(null, '', href);
  else history.pushState(null, '', href);
  currentRoute();
}

type RouteEvent = { type: 'NAVIGATE'; route: ProjectRoute };
const routeKey = (route: ProjectRoute) => route.kind === 'project' ? route.id : route.kind === 'device' ? route.printer : null;
const transitions = [
  { target: 'project', guard: (_: { id: string | null }, e: RouteEvent) => e.route.kind === 'project', action: (_: { id: string | null }, e: RouteEvent) => ({ id: routeKey(e.route) }) },
  { target: 'device', guard: (_: { id: string | null }, e: RouteEvent) => e.route.kind === 'device', action: (_: { id: string | null }, e: RouteEvent) => ({ id: routeKey(e.route) }) },
  { target: 'projects', guard: (_: { id: string | null }, e: RouteEvent) => e.route.kind === 'projects', action: () => ({ id: null }) },
  { target: 'blank', guard: (_: { id: string | null }, e: RouteEvent) => e.route.kind === 'blank', action: () => ({ id: null }) },
  { target: 'missing', action: () => ({ id: null }) },
] as const;
const routeOn = { NAVIGATE: [...transitions] };
export const routing = createMachine<ProjectRoute['kind'], RouteEvent, { id: string | null }>({
  id: 'route', initial: 'blank', context: { id: null },
  states: { blank: { on: routeOn }, projects: { on: routeOn }, project: { on: routeOn }, device: { on: routeOn }, missing: { on: routeOn } },
});
export function currentRoute(): ProjectRoute {
  const route = parseRoute(), current = routing.get();
  if (route.kind !== current.value || routeKey(route) !== current.context.id)
    routing.send({ type: 'NAVIGATE', route });
  return route;
}
window.addEventListener('popstate', currentRoute);
window.addEventListener('hashchange', currentRoute);
