import { chromium } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { createServer, preview } from 'vite'

const dir = process.env.EVIDENCE_DIR
const server =
  process.env.ORVEX_TEST_DEV === '1'
    ? await (
        await createServer({ server: { host: '127.0.0.1', port: 0 } })
      ).listen()
    : await preview({ preview: { host: '127.0.0.1', port: 0 } })
const browser = await chromium.launch()
try {
  async function verify(
    player,
    change,
    head,
    expected,
    file = join(dir, 'round.jsonl'),
  ) {
    const claim = JSON.parse(
      await readFile(join(dir, player + '.json'), 'utf8'),
    )
    const page = await browser.newPage()
    try {
      await page.addInitScript(
        ({ claim, token }) => {
          sessionStorage.setItem('orvex.command', JSON.stringify(claim))
          sessionStorage.setItem('orvex.token', token)
        },
        {
          claim: { ...claim, ...change },
          token: process.env[player.toUpperCase() + '_TOKEN'],
        },
      )
      await page.goto(server.resolvedUrls.local[0], {
        waitUntil: 'domcontentloaded',
      })
      await page.getByLabel('Player ID').fill(player)
      await page.getByLabel('Expected chain head').fill(head)
      await page.getByLabel('Settled history').setInputFiles(file)
      await page.getByRole('button', { name: 'Verify receipt' }).click()
      await page.getByText(expected).waitFor({ timeout: 15000 })
    } finally {
      await page.close()
    }
  }
  const head = process.env.EXPECTED_HEAD
  await verify(
    'alice',
    {},
    head,
    /^Verified for alice at sequence \d+ · Paid \d+ credits$/,
  )
  await verify(
    'charlie',
    {},
    head,
    /^Verified for charlie at sequence \d+ · Rejected · no_bet$/,
  )
  await verify(
    'alice',
    { result: 'Paid 999999 credits' },
    head,
    'Receipt outcome differs from the audited result.',
  )
  await verify(
    'charlie',
    { result: 'Bet accepted' },
    head,
    'Receipt outcome differs from the audited result.',
  )
  await verify(
    'alice',
    { position: undefined, player: undefined },
    head,
    /Legacy receipt: original sequence and time unavailable$/,
  )
  await verify(
    'alice',
    { player: 'bob' },
    head,
    'Receipt does not match the audited command.',
  )
  await verify('alice', {}, '0'.repeat(64), /^Audit failed: chain at /)
  const alice = JSON.parse(await readFile(join(dir, 'alice.json'), 'utf8'))
  await verify(
    'alice',
    { position: { ...alice.position, effectiveTimeUnixMicro: '1' } },
    head,
    'Receipt does not match the audited command.',
  )
  const unsupported =
    '../internal/audit/verification/testdata/golden/tamper/rules-version-2.jsonl'
  const header = JSON.parse(
    (await readFile(unsupported, 'utf8')).split('\n')[0],
  )
  await verify(
    'alice',
    {},
    header.head,
    'Audit unsupported: rules at sequence 0.',
    unsupported,
  )
  for (const fixture of [
    'golden/worked-example',
    'golden-v2/in-progress',
    'golden-v2/tamper/posting-amount-edited',
  ]) {
    const file = '../internal/audit/verification/testdata/' + fixture + '.jsonl'
    const header = JSON.parse((await readFile(file, 'utf8')).split('\n')[0])
    await verify(
      'alice',
      {},
      header.head,
      fixture.includes('tamper')
        ? /^Audit failed: ledger at /
        : 'A settled Crash v1 history with v2 ledger evidence is required.',
      file,
    )
  }
} finally {
  await browser.close()
  await server.close()
}
