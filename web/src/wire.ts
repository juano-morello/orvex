import { z } from 'zod'

const decimal = z.string().regex(/^\d+$/)
const placed = z.object({
  type: z.literal('BetPlaced'),
  playerId: z.string(),
  betCredits: decimal,
})
const cashed = z.object({
  type: z.literal('BetCashedOut'),
  playerId: z.string(),
  payoutCredits: decimal,
})
const events = z.discriminatedUnion('type', [
  placed,
  cashed,
  z.object({ type: z.literal('RoundStarted'), startedAtUnixMicro: decimal }),
  z.object({ type: z.literal('RoundCrashed'), crashPointScaled: decimal }),
  z.object({
    type: z.literal('BetLost'),
    playerId: z.string(),
    betCredits: decimal,
  }),
  z.object({ type: z.literal('RoundSettled') }),
])
export const currentRound = z.object({ roundId: z.uuid() })
export const frameSchema = z.object({
  sequence: decimal,
  genesis: z.object({}).optional(),
  events: events.array().optional(),
})
export const admission = z.object({
  status: z.literal('admitted'),
  sequence: decimal,
})
export const outcome = z.discriminatedUnion('status', [
  z.object({ status: z.literal('pending') }),
  z.object({
    status: z.literal('accepted'),
    events: z.union([placed, cashed]).array(),
  }),
  z.object({ status: z.literal('rejected'), reason: z.string() }),
])
export const failure = z.object({ code: z.string() })

export const position = z.object({
  roundId: z.uuid(),
  clientCommandId: z.uuid(),
  sequence: decimal,
  effectiveTimeUnixMicro: decimal,
})
