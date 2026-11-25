import { writeFile } from 'node:fs/promises'
import { join as pathJoin } from 'node:path'
import assert from 'node:assert/strict'
import { chromium } from '@playwright/test'
import { preview } from 'vite'

const server = await preview({ preview: { host: '127.0.0.1', port: 0 } })
let browser
try {
  browser = await chromium.launch()
  const [alice, bob] = await Promise.all([browser.newPage(), browser.newPage()])
  const charlie = await browser.newPage()
  async function join(page, token) {
    page.setDefaultTimeout(60_000)
    page.on('dialog', (dialog) =>
      dialog.type() === 'beforeunload' ? dialog.accept() : dialog.dismiss(),
    )
    const response = await page.goto(server.resolvedUrls.local[0], {
      waitUntil: 'domcontentloaded',
    })
    const headers = response.headers()
    assert.match(headers['content-security-policy'], /frame-ancestors 'none'/)
    assert.match(headers['content-security-policy'], /object-src 'none'/)
    assert.equal(headers['x-content-type-options'], 'nosniff')
    assert.equal(headers['referrer-policy'], 'no-referrer')
    await page.getByLabel('Player token').fill(token)
    await page.getByRole('button', { name: 'Join round' }).click()
    if (page === charlie) return
    if (page === bob) {
      const attempts = []
      const malformed = async (route) => {
        attempts.push(route.request().postData())
        if (attempts.length === 1) {
          return route.fulfill({
            status: 503,
            json: { code: 'outcome_unknown' },
          })
        }
        await route.fulfill({
          status: 400,
          contentType: 'text/html',
          body: 'Bad request',
        })
      }
      await page.route('**/rounds/*/commands', malformed)
      await page.getByLabel('Simulated credits').fill('1001')
      await page.getByRole('button', { name: 'Place bet' }).click()
      await page.getByText('Admission unknown · HTTP 400').waitFor()
      await page.unroute('**/rounds/*/commands', malformed)
      const retry = page.waitForRequest((r) => r.method() === 'POST')
      await page.getByRole('button', { name: 'Retry command recovery' }).click()
      assert.deepEqual(attempts, Array(2).fill((await retry).postData()))
      await page.getByText('Refused · insufficient_credits').waitFor()
      await page.reload({ waitUntil: 'domcontentloaded' })
      await page.getByText('Refused · insufficient_credits').waitFor()
    }
    let submissions = 0
    if (page === bob) {
      page.on('request', (request) => {
        if (request.method() === 'POST') submissions++
      })
      await page.route('**/v1/commands/*', (route) =>
        route.fulfill({ status: 401, json: { code: 'unauthenticated' } }),
      )
    }
    await page.getByRole('button', { name: 'Place bet' }).click()
    if (page === bob) {
      await page
        .getByText('Admitted · outcome unavailable · unauthenticated')
        .waitFor()
      await page.reload({ waitUntil: 'domcontentloaded' })
      await page
        .getByText('Admitted · outcome unavailable · unauthenticated')
        .waitFor()
      await page.unroute('**/v1/commands/*')
      for (const [status, json, message] of [
        [
          404,
          { code: 'command_not_found' },
          'Admitted · outcome unavailable · command_not_found',
        ],
        [200, {}, 'Inconsistent gateway receipt · outcome unavailable'],
      ]) {
        await page.route('**/v1/commands/*', (route) =>
          route.fulfill({ status, json }),
        )
        await page
          .getByRole('button', { name: 'Retry command recovery' })
          .click()
        await page.getByText(message).waitFor()
        await page.unroute('**/v1/commands/*')
      }
      await page.getByRole('button', { name: 'Retry command recovery' }).click()
    }
    await page.getByText('Bet accepted', { exact: true }).waitFor()
    if (page === bob) assert.equal(submissions, 1)
  }
  await Promise.all([
    join(alice, process.env.ALICE_TOKEN),
    join(bob, process.env.BOB_TOKEN),
    join(charlie, process.env.CHARLIE_TOKEN),
  ])
  let first
  let rotated = true
  let duplicate
  await alice.route('**/rounds/*/commands', async (route) => {
    if (first && rotated) {
      rotated = false
      return route.fulfill({ status: 401, json: { code: 'unauthenticated' } })
    }
    const response = await route.fetch()
    if (response.status() === 403) {
      assert.equal((await response.json()).code, 'player_mismatch')
      return route.fulfill({ response })
    }
    assert.equal(response.status(), 202)
    const receipt = await response.json()
    if (!first) {
      first = receipt
      await route.abort()
    } else {
      duplicate = receipt
      await route.fulfill({ response })
    }
  })
  await Promise.all(
    [alice, charlie].map((page) =>
      page.getByRole('button', { name: 'Cash out' }).click(),
    ),
  )
  await alice.getByText('Recovering command…').waitFor()
  await alice.reload({ waitUntil: 'domcontentloaded' })
  await alice.getByText('Admission unknown · unauthenticated').waitFor()
  await alice
    .getByLabel('Updated token for this player (optional)')
    .fill(process.env.BOB_TOKEN)
  await alice.getByRole('button', { name: 'Retry command recovery' }).click()
  await alice.getByText('Admission unknown · player_mismatch').waitFor()
  await alice
    .getByLabel('Updated token for this player (optional)')
    .fill(process.env.ALICE_TOKEN)
  await alice.getByRole('button', { name: 'Retry command recovery' }).click()
  await alice
    .getByRole('status')
    .filter({ hasText: /^Paid \d+ credits$/ })
    .waitFor()
  assert.equal(duplicate.duplicate, true)
  for (const field of [
    'clientCommandId',
    'roundId',
    'sequence',
    'effectiveTimeUnixMicro',
  ]) {
    assert.equal(duplicate[field], first[field])
  }
  const paid = await alice.getByRole('status').textContent()
  await alice.reload({ waitUntil: 'domcontentloaded' })
  assert.equal(await alice.getByRole('status').textContent(), paid)
  await bob.getByText('bob lost 100', { exact: true }).waitFor()
  assert.equal(await bob.getByText(/^alice received \d+$/).count(), 1)
  await charlie.getByText('Rejected · no_bet', { exact: true }).waitFor()
  for (const [name, page] of [
    ['alice', alice],
    ['charlie', charlie],
  ]) {
    const claim = await page.evaluate(() =>
      sessionStorage.getItem('orvex.command'),
    )
    await writeFile(pathJoin(process.env.EVIDENCE_DIR, name + '.json'), claim, {
      mode: 0o600,
    })
  }
} finally {
  await browser?.close()
  await server.close()
}
