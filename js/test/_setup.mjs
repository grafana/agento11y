// Strip ambient AGENTO11Y_* / SIGIL_* / OTEL_* env vars; Agento11yClient reads them automatically.
for (const key of Object.keys(process.env)) {
  if (key.startsWith('AGENTO11Y_') || key.startsWith('SIGIL_') || key.startsWith('OTEL_')) {
    delete process.env[key];
  }
}
