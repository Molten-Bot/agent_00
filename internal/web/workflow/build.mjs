import { build } from 'esbuild';
import { readFile, writeFile, readdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
const root = fileURLToPath(new URL('.', import.meta.url));
const result = await build({
  metafile: true,
  absWorkingDir: root,
  entryPoints: ['src/editor.jsx'],
  outfile: '../static/workflow-editor.js',
  bundle: true,
  minify: true,
  format: 'iife',
  target: ['es2022'],
  define: { 'process.env.NODE_ENV': '"production"' },
  legalComments: 'external',
});
const packages = [...new Set(Object.keys(result.metafile.inputs).filter(path => path.startsWith('node_modules/')).map(path => {
  const parts = path.slice('node_modules/'.length).split('/');
  return parts[0].startsWith('@') ? parts.slice(0, 2).join('/') : parts[0];
}))].sort();
const licenses = await Promise.all(packages.map(async name => {
  const folder = `${root}node_modules/${name}`;
  const files = (await readdir(folder)).filter(file => /^licen[sc]e(?:\.|$)/i.test(file));
  // The compiler runtime ships without a license file; it shares React's repository license.
  if (!files.length && name === 'react-compiler-runtime') return `${name}\n${await readFile(`${root}node_modules/react/LICENSE`, 'utf8')}`;
  if (!files.length) throw new Error(`Missing license for ${name}`);
  return `${name}\n${(await Promise.all(files.map(file => readFile(`${folder}/${file}`, 'utf8')))).join('\n')}`;
}));
await writeFile(`${root}../static/workflow-editor-LICENSE.txt`, licenses.join('\n\n---\n\n'));
