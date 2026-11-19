export interface Camera { id: string; name: string; format: 'mjpeg' | 'mp4'; url: string }

export function readCameras(value: unknown): Camera[] {
  if (!value || typeof value !== 'object' || !('cameras' in value) || !Array.isArray(value.cameras)) throw new Error('Invalid camera settings');
  return value.cameras.map((camera: unknown) => {
    if (!camera || typeof camera !== 'object' || !('id' in camera) || typeof camera.id !== 'string'
      || !('name' in camera) || typeof camera.name !== 'string' || !('url' in camera) || typeof camera.url !== 'string'
      || !('format' in camera) || (camera.format !== 'mjpeg' && camera.format !== 'mp4')) throw new Error('Invalid camera settings');
    const url = new URL(camera.url, location.origin);
    if (url.origin !== location.origin || !url.pathname.startsWith('/api/printer-connections/')) throw new Error('Invalid camera URL');
    return { id: camera.id, name: camera.name, format: camera.format, url: camera.url };
  });
}

export class JPEGFrames {
  private pending = new Uint8Array(0);
  private length: number | undefined;

  push(chunk: Uint8Array): Uint8Array<ArrayBuffer>[] {
    const bytes = new Uint8Array(this.pending.length + chunk.length);
    bytes.set(this.pending); bytes.set(chunk, this.pending.length); this.pending = bytes;
    const frames: Uint8Array<ArrayBuffer>[] = [];
    while (this.pending.length) {
      if (this.length === undefined) {
        let end = -1;
        for (let i = 0; i + 3 < this.pending.length; i++) {
          if (this.pending[i] === 13 && this.pending[i + 1] === 10 && this.pending[i + 2] === 13 && this.pending[i + 3] === 10) { end = i; break; }
        }
        if (end < 0) {
          if (this.pending.length > 65536) throw new Error('Camera header is too large');
          break;
        }
        const header = new TextDecoder().decode(this.pending.subarray(0, end));
        const length = /(?:^|\r\n)Content-Length:\s*(\d+)\s*(?:\r\n|$)/i.exec(header);
        if (!length) throw new Error('Camera frame length is missing');
        this.length = Number(length[1]);
        if (this.length < 1 || this.length > 16 << 20) throw new Error('Camera frame is too large');
        this.pending = this.pending.slice(end + 4);
      }
      if (this.pending.length < this.length) break;
      frames.push(this.pending.slice(0, this.length));
      this.pending = this.pending.slice(this.length); this.length = undefined;
    }
    return frames;
  }
}

export function followCameraVisibility(source: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>, change: (visible: boolean) => void): () => void {
  const update = () => change(source.visibilityState === 'visible');
  source.addEventListener('visibilitychange', update);
  update();
  return () => source.removeEventListener('visibilitychange', update);
}
