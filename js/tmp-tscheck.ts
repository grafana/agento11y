import type { BasePlugin } from '@google/adk';
import { Agento11yClient, defaultConfig } from './src/index.js';
import { withAgento11yGoogleAdkPlugins } from './src/frameworks/google-adk/index.js';

const c = new Agento11yClient(defaultConfig());
const arr: BasePlugin[] = [];
const cfg: {plugins: BasePlugin[]} = withAgento11yGoogleAdkPlugins({plugins: arr}, c);
console.log(cfg.plugins.length);
