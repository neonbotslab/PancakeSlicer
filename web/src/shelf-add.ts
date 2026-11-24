import { responseError } from './api/access';
import { printerBed } from './actions';
import { leaseSettled, leaseWritable } from './flow/project-lease';
import { project } from './flow/project';
import { instanceBox, loadMesh, nextFreeSpot, parsePoints } from './plater/geometry';
import { documentGeneration, undoable } from './state/history';
import { getState, markStale, newId, printerPreset, setState, type PlateObject } from './state/store';

function editable() {
  if (!leaseWritable()) throw new Error('Take control of the project before adding a model.');
  if (getState().projectPlates.find((p) => p.index === getState().plateIndex)?.locked) throw new Error('Unlock the plate before adding a model.');
  if (['opening', 'conflict', 'failed'].includes(project.get().value)) throw new Error('Resolve the project save before adding a model.');
}

export const captureShelfDestination = () => ({ generation: documentGeneration(), plate: getState().plateIndex });

export async function addShelfCopy(id: string, revision: number, name: string, destination: ReturnType<typeof captureShelfDestination>): Promise<void> {
  const { generation, plate } = destination;
  const checkDestination = () => {
    if (generation !== documentGeneration() || plate !== getState().plateIndex) throw new Error('The active plate changed. Add the model again.');
    editable();
  };
  await leaseSettled();
  checkDestination();
  const response = await fetch(`/api/shelf/models/${encodeURIComponent(id)}/geometry.stl?revision=${revision}`);
  if (!response.ok) throw await responseError(response);
  const source = new File([await response.blob()], `${name.replace(/\.(stl|3mf|step|stp|obj)$/i, '')}.stl`, { type: 'model/stl' });
  const geometry = await loadMesh(source, undefined, false);
  let inserted = false;
  try {
    const area = await printerBed();
    checkDestination();
    const triangles = (geometry.index?.count ?? geometry.attributes.position.count) / 3;
    const object: PlateObject = {
      id: newId(), name: source.name, source, geometry, rotX: 0, rotY: 0, scale: [1, 1, 1], extruder: 1,
      settings: {}, instances: [{ id: newId(), x: 0, y: 0, rotZ: 0 }],
      simplification: { trianglesBefore: triangles, trianglesAfter: triangles, maxErrorMm: 0, kept: 'original' },
    };
    const config = printerPreset(getState())?.config;
    const excluded = parsePoints(config?.bed_exclude_area);
    const at = nextFreeSpot(getState().objects, object, area, excluded.length >= 3 ? [excluded] : []);
    const height = instanceBox(object, object.instances[0]).max.z;
    if (!at || height > Number(config?.printable_height ?? Infinity)) throw new Error('There is no free space on this plate. Make room before adding the model.');
    object.instances[0] = { ...object.instances[0], ...at };
    undoable('Add model from Shelf', () => setState((s) => ({
      objects: [...s.objects, object], selected: { object: object.id, instance: object.instances[0].id }, job: markStale(s.job),
    })));
    inserted = true;
  } finally {
    if (!inserted) geometry.dispose();
  }
}
