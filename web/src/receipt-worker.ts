import './generated/wasm_exec.js'
import { z } from 'zod'
import type { Command } from './commands'

declare class Go {
  importObject: WebAssembly.Imports
  run(instance: WebAssembly.Instance): Promise<void>
}
declare function verifySettled(data: Uint8Array, expectedHead: string): string

const recordSchema = z.object({
  kind: z.literal('command'),
  roundId: z.string(),
  sequence: z.string(),
  effectiveTimeUnixMicro: z.string(),
  commandId: z.string(),
  command: z.object({
    authority: z.literal('player'),
    type: z.enum(['PlaceBet', 'CashOut']),
    player: z.string(),
    betCredits: z.string().optional(),
  }),
  events: z.array(
    z.object({
      event: z.object({
        type: z.string(),
        payoutCredits: z.string().optional(),
      }),
    }),
  ),
  result: z.object({
    status: z.enum(['accepted', 'rejected']),
    reason: z.string().optional(),
  }),
})

// Gateway IDs hash the raw 16 UUID bytes after the domain and player prefixes.
async function commandId(player: string, client: string) {
  const prefix = new TextEncoder().encode(
    'orvex/command-id/v1\0' + player + '\0',
  )
  const source = new Uint8Array(prefix.length + 16)
  source.set(prefix)
  const digits = client.replaceAll('-', '')
  for (let i = 0; i < 16; i++)
    source[prefix.length + i] = parseInt(digits.slice(2 * i, 2 * i + 2), 16)
  const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', source))
  hash[6] = (hash[6] & 15) | 128
  hash[8] = (hash[8] & 63) | 128
  return Array.from(hash.slice(0, 16), (b) =>
    b.toString(16).padStart(2, '0'),
  ).join('')
}

async function check(file: File, head: string, player: string, claim: Command) {
  const position = claim.position
  if (
    position &&
    (position.roundId !== claim.round || position.clientCommandId !== claim.id)
  )
    throw Error('Receipt identity is missing or inconsistent.')
  const data = new Uint8Array(await file.arrayBuffer())
  const go = new Go()
  const module = await WebAssembly.instantiateStreaming(
    fetch(import.meta.env.BASE_URL + 'audit/verify.wasm'),
    go.importObject,
  ).catch(() => {
    throw Error('Verifier could not load.')
  })
  void go.run(module.instance)
  const failure = verifySettled(data, head)
  if (failure) throw Error(failure)

  // In v2 the header is followed by genesis and all command records in sequence order.
  const decoder = new TextDecoder('utf-8', { fatal: true })
  const id = await commandId(player, claim.id)
  const target = position ? Number(position.sequence) + 1 : undefined
  let parsed
  for (let offset = 0, line = 0; offset < data.length; line++) {
    const end = data.indexOf(10, offset)
    if (end < 0) break
    if (line === target || (target === undefined && line >= 2)) {
      const record = recordSchema.safeParse(
        JSON.parse(decoder.decode(data.subarray(offset, end))),
      )
      if (
        line === target ||
        (record.success && record.data.commandId.replaceAll('-', '') === id)
      ) {
        parsed = record
        break
      }
    }
    offset = end + 1
  }
  if (!parsed || !parsed.success)
    throw Error('Player command missing at the receipt sequence.')
  const r = parsed.data
  const matches =
    r.roundId === claim.round &&
    (!position ||
      (r.sequence === position.sequence &&
        r.effectiveTimeUnixMicro === position.effectiveTimeUnixMicro)) &&
    (!claim.player || claim.player === player) &&
    r.command.player === player &&
    r.commandId.replaceAll('-', '') === id &&
    (claim.credits === null
      ? r.command.type === 'CashOut'
      : r.command.type === 'PlaceBet' && r.command.betCredits === claim.credits)
  if (!matches) throw Error('Receipt does not match the audited command.')
  const paid = r.events.find((e) => e.event.type === 'BetCashedOut')
  const result =
    r.result.status === 'rejected'
      ? 'Rejected · ' + r.result.reason
      : paid
        ? 'Paid ' + paid.event.payoutCredits + ' credits'
        : 'Bet accepted'
  if (result !== claim.result)
    throw Error('Receipt outcome differs from the audited result.')
  return (
    'Verified for ' +
    player +
    ' at sequence ' +
    r.sequence +
    ' · ' +
    result +
    (position
      ? ''
      : ' · Legacy receipt: original sequence and time unavailable')
  )
}

self.onmessage = async ({
  data,
}: MessageEvent<{
  file: File
  head: string
  player: string
  claim: Command
}>) => {
  try {
    self.postMessage(await check(data.file, data.head, data.player, data.claim))
  } catch (error) {
    self.postMessage(
      error instanceof Error ? error.message : 'Verification failed.',
    )
  }
}
