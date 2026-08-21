import { createRoot } from 'react-dom/client';
import { App } from './app';

// The mount is the one side effect in the tree, and it lives on its own so
// that importing any module in a test does not try to render the whole app
// into a document that has no #root.
const root = document.getElementById('root');
if (!root) throw new Error('the viewer shell is missing its #root element');
createRoot(root).render(<App />);
