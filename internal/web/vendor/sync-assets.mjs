import { readFile, writeFile } from 'node:fs/promises';
const files = {
  'lucide/dist/umd/lucide.min.js': 'lucide.min.js',
  'lucide/LICENSE': 'lucide-LICENSE.txt',
  'marked/lib/marked.umd.js': 'marked.umd.js',
  'marked/LICENSE': 'marked-LICENSE.txt',
  '@xterm/xterm/lib/xterm.js': 'xterm.js',
  '@xterm/xterm/css/xterm.css': 'xterm.css',
  '@xterm/xterm/LICENSE': 'xterm-LICENSE.txt',
  'emoji-mart/dist/browser.js': 'emoji-mart-browser.js',
  'emoji-mart/LICENSE': 'emoji-mart-LICENSE.txt',
  '@emoji-mart/data/sets/15/native.json': 'emoji-mart-data.json',
  '@emoji-mart/data/LICENSE': 'emoji-mart-data-LICENSE.txt',
};
for (const [source, destination] of Object.entries(files)) {
  const input = new URL(`node_modules/${source}`, import.meta.url);
  const output = new URL(`../static/${destination}`, import.meta.url);
  let content = await readFile(input, 'utf8');
  // Source maps are not shipped with these self-contained browser assets.
  if (destination.endsWith('.js')) content = content.replace(/^\/\/# sourceMappingURL=.*(?:\r?\n|$)/gm, '');
  await writeFile(output, content);
}
