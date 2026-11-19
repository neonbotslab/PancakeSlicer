import { useEffect, useRef } from 'react';
import { responseError } from '../api/access';
import { JPEGFrames, type Camera } from './camera-stream';

function MP4Camera({ url, onError, onRatio, onFrame }: { url: string; onError(message: string): void; onRatio(ratio: number): void; onFrame(): void }) {
  const video = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    if (typeof MediaSource === 'undefined') { onError('Live video is unavailable in this browser'); return; }
    const abort = new AbortController();
    const media = new MediaSource(), objectURL = URL.createObjectURL(media);
    const element = video.current!;
    element.src = objectURL;
    let frameCallback = 0;
    const presented = () => { onFrame(); frameCallback = element.requestVideoFrameCallback(presented); };
    if (typeof element.requestVideoFrameCallback === 'function') frameCallback = element.requestVideoFrameCallback(presented);
    const run = async () => {
      try {
        const response = await fetch(url, { signal: abort.signal });
        if (!response.ok) throw await responseError(response);
        if (!response.body) throw new Error('Camera stream is empty');
        const reader = response.body.getReader();
        let init = new Uint8Array(0), codec = '';
        while (!codec) {
          const chunk = await reader.read();
          if (chunk.done) throw new Error('The camera stream disconnected before video could start.');
          const joined = new Uint8Array(init.length + chunk.value.length); joined.set(init); joined.set(chunk.value, init.length); init = joined;
          if (init.length > 2 << 20) throw new Error('The camera sent an unexpectedly large video header.');
          for (let i = 4; i + 8 < init.length; i++) {
            if (init[i] === 97 && init[i + 1] === 118 && init[i + 2] === 99 && init[i + 3] === 67) {
              codec = `avc1.${[...init.slice(i + 5, i + 8)].map((v) => v.toString(16).padStart(2, '0')).join('')}`; break;
            }
          }
        }
        const mime = `video/mp4; codecs="${codec}"`;
        if (!MediaSource.isTypeSupported(mime)) throw new Error('Your browser or device doesn\'t support decoding this camera\'s H.264 video stream.');
        const buffer = media.addSourceBuffer(mime);
        const update = (work: () => void) => new Promise<void>((resolve, reject) => {
          const done = () => { cleanup(); resolve(); }, failed = () => { cleanup(); reject(new Error('Oops! Couldn\'t decode the live video stream from the camera.')); };
          const cleanup = () => { buffer.removeEventListener('updateend', done); buffer.removeEventListener('error', failed); abort.signal.removeEventListener('abort', failed); };
          buffer.addEventListener('updateend', done, { once: true }); buffer.addEventListener('error', failed, { once: true }); abort.signal.addEventListener('abort', failed, { once: true });
          try { abort.signal.throwIfAborted(); work(); } catch (error) { cleanup(); reject(error); }
        });
        await update(() => buffer.appendBuffer(init));
        while (!abort.signal.aborted) {
          const { done, value } = await reader.read();
          if (done) throw new Error('Camera stream disconnected');
          await update(() => buffer.appendBuffer(value));
          if (buffer.buffered.length) {
            const end = buffer.buffered.end(buffer.buffered.length - 1);
            if (end - element.currentTime > 3) element.currentTime = Math.max(buffer.buffered.start(0), end - 0.5);
            if (end > 20 && buffer.buffered.start(0) < end - 20) {
              await update(() => buffer.remove(0, end - 15));
            }
          }
        }
      } catch (error) { if (!abort.signal.aborted) onError(error instanceof Error ? error.message : 'Camera unavailable'); }
    };
    media.addEventListener('sourceopen', () => void run(), { once: true });
    return () => { abort.abort(); if (frameCallback) element.cancelVideoFrameCallback(frameCallback); element.removeAttribute('src'); element.load(); URL.revokeObjectURL(objectURL); };
  }, [url, onError, onFrame]);
  return <video ref={video} autoPlay muted playsInline style={{ visibility: 'hidden' }} onLoadedData={(e) => { e.currentTarget.style.visibility = 'visible'; }} onTimeUpdate={(e) => { if (typeof e.currentTarget.requestVideoFrameCallback !== 'function') onFrame(); }} onError={() => onError('Camera video could not be decoded')} aria-label="Live printer camera" onResize={(e) => { const v = e.currentTarget; if (v.videoWidth && v.videoHeight) onRatio(v.videoWidth / v.videoHeight); }} />;
}


function MJPEGCamera({ url, onError, onRatio, onFrame }: StreamProps) {
  const image = useRef<HTMLImageElement>(null);
  useEffect(() => {
    const abort = new AbortController();
    const element = image.current!;
    let displayed = '', next = '';
    const run = async () => {
      try {
        const response = await fetch(url, { signal: abort.signal });
        if (!response.ok) throw await responseError(response);
        if (!response.body) throw new Error('Camera stream is empty');
        const reader = response.body.getReader(), frames = new JPEGFrames();
        while (!abort.signal.aborted) {
          const { done, value } = await reader.read();
          if (done) throw new Error('Camera stream disconnected');
          const frame = frames.push(value).at(-1);
          if (!frame || next) continue;
          next = URL.createObjectURL(new Blob([frame], { type: 'image/jpeg' }));
          element.src = next;
        }
      } catch (error) { if (!abort.signal.aborted) onError(error instanceof Error ? error.message : 'Camera unavailable'); }
    };
    element.onload = () => {
      if (displayed) URL.revokeObjectURL(displayed);
      displayed = next; next = ''; element.style.visibility = 'visible';
      if (element.naturalWidth && element.naturalHeight) onRatio(element.naturalWidth / element.naturalHeight);
      onFrame();
    };
    element.onerror = () => onError('Camera frame could not be decoded');
    void run();
    return () => {
      abort.abort(); element.onload = null; element.onerror = null; element.removeAttribute('src');
      if (displayed) URL.revokeObjectURL(displayed);
      if (next) URL.revokeObjectURL(next);
    };
  }, [url, onError, onRatio, onFrame]);
  return <img ref={image} style={{ visibility: 'hidden' }} alt="Live printer camera" />;
}

type StreamProps = { url: string; onError(message: string): void; onRatio(ratio: number): void; onFrame(): void };

export function LiveCamera({ camera, ...events }: Omit<StreamProps, 'url'> & { camera: Camera }) {
  return camera.format === 'mp4' ? <MP4Camera url={camera.url} {...events} /> : <MJPEGCamera url={camera.url} {...events} />;
}
