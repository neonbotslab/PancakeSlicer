// iOS and iPadOS (every browser there runs WebKit) turn `accept` into system file types and grey out
// files whose extension has no registered type, such as .stl, .3mf or .gcode. There the picker lists
// every file and the caller's own name check rejects the wrong ones. iPadOS reports itself as a Mac.
const IOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || /Mac/.test(navigator.platform) && navigator.maxTouchPoints > 1;

export function fileAccept(accept: string): string | undefined {
  return IOS ? undefined : accept;
}

// The input stays in the document until the picker closes: Safari can drop a detached input, and its change event with it.
export function pickFiles(accept: string, onFiles: (files: FileList) => unknown, multiple = false): void {
  const input = document.createElement('input');
  input.type = 'file';
  input.multiple = multiple;
  input.hidden = true;
  const type = fileAccept(accept);
  if (type) input.accept = type;
  input.onchange = () => { input.remove(); if (input.files?.length) void onFiles(input.files); };
  input.oncancel = () => input.remove();
  document.body.append(input);
  input.click();
}
