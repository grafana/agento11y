// Vite serves any import with a ?raw query as the file's text. The vendored
// UMD builds are read that way rather than through node:fs, so the browser
// tree keeps its `types: []` tsconfig and no test needs @types/node.
declare module '*?raw' {
  const content: string;
  export default content;
}

declare module 'virtual:theme-css-source' {
  const content: string;
  export default content;
}
