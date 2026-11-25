import { defineConfig } from 'vite'

const base = (process.env.ORVEX_WEB_BASE || '/').replace(/\/$/, '') + '/'
export default defineConfig({
  base,
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
