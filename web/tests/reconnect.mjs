import assert from 'node:assert/strict'
import { chromium } from '@playwright/test'
import { preview } from 'vite'

const server = await preview({ preview: { host: '127.0.0.1', port: 0 } })
const browser = await chromium.launch()
const round = '5d1f0c2e-7a3b-4c4d-9e8f-0a1b2c3d4ea1'
const next = '5d1f0c2e-7a3b-4c4d-9e8f-0a1b2c3d4ea2'
const claim = { round, id: next, credits: '100', result: 'Bet accepted' }
const record = (seq, fields) =>
  'event: record\nid: ' +
  seq +
  '\ndata: ' +
  JSON.stringify({ sequence: String(seq), ...fields }) +
  '\n\n'
try {
  const page = await browser.newPage()
  page.setDefaultTimeout(10000)
  await page.addInitScript((claim) => {
    sessionStorage.setItem('orvex.token', 'test-player')
    sessionStorage.setItem('orvex.command', JSON.stringify(claim))
  }, claim)
  let available = false
  let current = round
  const outage = page.waitForResponse((res) => res.status() === 502, {
    timeout: 10000,
  })
  await page.route('**/rounds/current', (route) =>
    route.fulfill({ json: { roundId: current } }),
  )
  await page.route('**/stream*', (route) => {
    const url = new URL(route.request().url())
    if (url.pathname.includes(next)) return route.fulfill({ status: 404 })
    const sse = (body) =>
      route.fulfill({ contentType: 'text/event-stream', body })
    if (!url.search)
      return sse(
        record(0, { genesis: {} }) +
          record(1, {
            events: [
              {
                type: 'RoundStarted',
                startedAtUnixMicro: String(Date.now() * 1000),
              },
            ],
          }),
      )
    assert.equal(url.searchParams.get('after'), '1')
    if (!available) {
      return route.fulfill({ status: 502, body: 'Service unavailable' })
    }
    return sse(
      record(2, {
        events: [
          { type: 'RoundCrashed', crashPointScaled: '19999' },
          { type: 'RoundSettled' },
        ],
      }),
    )
  })
  await page.goto(server.resolvedUrls.local[0], {
    waitUntil: 'domcontentloaded',
  })
  await page.waitForFunction(
    () => document.querySelector('.board strong')?.textContent === '—',
  )
  await page.getByText(/Round feed unavailable/).waitFor()
  // Keep the proxy unavailable through at least one reconnect.
  await outage
  assert.equal(
    await page.getByRole('button', { name: 'Cash out' }).isDisabled(),
    true,
  )
  available = true
  await page.getByText('Settled', { exact: true }).waitFor()
  await page.getByText('1.9999×', { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Join current round' }).click()
  await page.getByText('Waiting for a new round…').waitFor()
  assert.deepEqual(
    await page.evaluate(() =>
      JSON.parse(sessionStorage.getItem('orvex.command')),
    ),
    claim,
  )
  current = next
  await page.getByText('Round ' + next, { exact: true }).waitFor()
  assert.equal(
    await page.evaluate(() => sessionStorage.getItem('orvex.command')),
    null,
  )
  await page.getByText(/Round feed unavailable/).waitFor()
  current = round
  await page.getByRole('button', { name: 'Join current round' }).click()
  await page.getByText('Round ' + round, { exact: true }).waitFor()
} finally {
  await browser.close()
  await server.close()
}
