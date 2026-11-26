import { defineConfig } from 'vite'

const base = (process.env.ORVEX_WEB_BASE || '/').replace(/\/$/, '') + '/'
export default defineConfig({
  base,
  preview: {
    headers: {
      'X-Content-Type-Options': 'nosniff',
      'Referrer-Policy': 'no-referrer',
      'Content-Security-Policy':
        "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
    },
  },
  server: {
    host: '127.0.0.1',
    proxy: {
      [base + 'v1/']: {
        target:
          'http://' + (process.env.ORVEX_GATEWAY_ADDR || '127.0.0.1:8080'),
        rewrite: (path) => '/' + path.slice(base.length),
      },
    },
  },
})
