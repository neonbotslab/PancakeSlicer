import * as THREE from 'three';
import { instanceBox } from '../plater/geometry';
import { filamentColor } from '../plater/filamentColor';
import { getState } from '../state/store';

const materials = new Map<string, THREE.MeshStandardMaterial>();
const plain = (color: string) => {
  let m = materials.get(color);
  if (!m) materials.set(color, (m = new THREE.MeshStandardMaterial({ color, roughness: 0.65, metalness: 0.05 })));
  return m;
};

const WIDTH = 320, HEIGHT = 240, SCALE = 2;
const SRGB = Uint8Array.from({ length: 256 }, (_, i) => { const c = i / 255; return Math.round(255 * (c <= 0.0031308 ? 12.92 * c : 1.055 * c ** (1 / 2.4) - 0.055)); });

export function projectThumbnail(): string | null {
  const p = (window as unknown as { __plater?: { renderer: THREE.WebGLRenderer; scene: THREE.Scene; camera: THREE.Camera } }).__plater;
  const c = p?.renderer.domElement, s = getState();
  if (!p || !c?.clientWidth || !c.clientHeight || s.tab !== 'prepare' || !s.objects.length) return null;
  const box = new THREE.Box3();
  for (const o of s.objects) for (const i of o.instances) box.union(instanceBox(o, i));
  const sphere = box.getBoundingSphere(new THREE.Sphere());
  const cam = new THREE.PerspectiveCamera(30, WIDTH / HEIGHT, 1, 10000);
  const dir = new THREE.Vector3(0.45, -1, 0.8).normalize();
  const dist = (1.15 * sphere.radius) / Math.sin(THREE.MathUtils.degToRad(15));
  cam.position.copy(sphere.center).addScaledVector(dir, dist);
  cam.up.set(0, 0, 1);
  cam.lookAt(sphere.center);
  cam.near = Math.max(0.1, dist - 4 * sphere.radius);
  cam.far = dist + 4 * sphere.radius;
  cam.updateProjectionMatrix();
  const byId = new Map(s.objects.map((o) => [o.id, o]));
  const hidden: THREE.Object3D[] = [], swapped: [THREE.Mesh, THREE.Material | THREE.Material[]][] = [];
  for (const child of p.scene.children) {
    if ((child as THREE.Light).isLight || !child.visible) continue;
    const { object, volume } = child.userData as { object?: number; volume?: number };
    const o = object === undefined ? undefined : byId.get(object), v = volume === undefined ? undefined : o?.volumes?.find((x) => x.id === volume);
    if (!o || !(child as THREE.Mesh).isMesh || (volume !== undefined && v?.type !== 'part')) { child.visible = false; hidden.push(child); continue; }
    const mesh = child as THREE.Mesh;
    swapped.push([mesh, mesh.material]);
    mesh.material = plain(filamentColor(s, v?.extruder || o.extruder));
  }
  // Render offscreen: drawing into the visible canvas (even for one task) shows up as a flicker in Firefox.
  const w = WIDTH * SCALE, h = HEIGHT * SCALE, target = new THREE.WebGLRenderTarget(w, h, { samples: 4 });
  const pixels = new Uint8Array(w * h * 4), previous = p.renderer.getRenderTarget();
  try {
    p.renderer.setRenderTarget(target);
    p.renderer.render(p.scene, cam);
    p.renderer.readRenderTargetPixels(target, 0, 0, w, h, pixels);
  } finally {
    p.renderer.setRenderTarget(previous);
    target.dispose();
    for (const child of hidden) child.visible = true;
    for (const [mesh, material] of swapped) mesh.material = material;
  }
  // Render targets hold linear colour and rows bottom-up; encode sRGB and flip for the 2D canvas.
  const image = new ImageData(w, h), row = w * 4;
  for (let y = 0; y < h; y++)
    for (let i = 0, src = (h - 1 - y) * row, dst = y * row; i < row; i += 4) {
      image.data[dst + i] = SRGB[pixels[src + i]]; image.data[dst + i + 1] = SRGB[pixels[src + i + 1]];
      image.data[dst + i + 2] = SRGB[pixels[src + i + 2]]; image.data[dst + i + 3] = 255;
    }
  const full = document.createElement('canvas'), t = document.createElement('canvas');
  full.width = w; full.height = h; full.getContext('2d')!.putImageData(image, 0, 0);
  t.width = WIDTH; t.height = HEIGHT;
  t.getContext('2d')!.drawImage(full, 0, 0, WIDTH, HEIGHT);
  return t.toDataURL('image/jpeg', 0.8);
}
