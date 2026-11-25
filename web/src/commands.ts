import { z } from 'zod'
import { admission, failure, outcome, position } from './wire'

const commandSchema = z.object({
  round: z.uuid(),
  id: z.uuid(),
  credits: z.string().nullable(),
  result: z.string().optional(),
  position: position.optional(),
  player: z.string().optional(),
})
export type Command = z.infer<typeof commandSchema>
const key = 'orvex.command'

export function restoreCommand(): Command | null {
  try {
    const saved =
      commandSchema.safeParse(JSON.parse(sessionStorage.getItem(key) ?? 'null'))
        .data ?? null
    if (
      saved?.result === 'Refused · unauthenticated' ||
      (saved?.position && saved.result?.startsWith('Refused · '))
    ) {
      return { ...saved, result: undefined }
    }
    return saved
  } catch {
    return null
  }
}

export function saveCommand(command: Command | null) {
  if (command) sessionStorage.setItem(key, JSON.stringify(command))
  else sessionStorage.removeItem(key)
}

export async function recover(
  command: Command,
  token: string,
  signal: AbortSignal,
  report: (text: string) => void,
) {
  function inconsistent() {
    report('Inconsistent gateway receipt · outcome unavailable')
    return 'paused' as const
  }
  let observed = command.position
  function finish(result: string, player?: string) {
    saveCommand({
      ...command,
      result,
      player: player ?? command.player,
      position: observed,
    })
    report(result)
  }
  const headers = {
    Authorization: 'Bearer ' + token,
    'Content-Type': 'application/json',
    ...(command.player ? { 'Orvex-Expected-Player': command.player } : {}),
  }
  const payload =
    command.credits === null
      ? { cashOut: {} }
      : { placeBet: { betCredits: command.credits } }
  const body = JSON.stringify({ clientCommandId: command.id, ...payload })
  // Old pending receipts have no authenticated owner. Look them up without issuing a new POST.
  let lookup = Boolean(observed) || !command.player
  while (!signal.aborted) {
    try {
      const response = lookup
        ? await fetch(import.meta.env.BASE_URL + 'v1/commands/' + command.id, {
            headers,
            signal,
          })
        : await fetch(
            import.meta.env.BASE_URL +
              'v1/rounds/' +
              command.round +
              '/commands',
            {
              method: 'POST',
              headers,
              body,
              signal,
            },
          )
      const data: unknown = await response.json().catch(() => null)
      if (signal.aborted) return
      if (response.ok) {
        const parsed = position.safeParse(data)
        if (!parsed.success) return inconsistent()
        const next = parsed.data
        if (
          next.roundId !== command.round ||
          next.clientCommandId !== command.id ||
          (observed &&
            (next.sequence !== observed.sequence ||
              next.effectiveTimeUnixMicro !== observed.effectiveTimeUnixMicro))
        )
          return inconsistent()
        observed = next
      }
      if (response.ok && !lookup) {
        const parsed = admission.safeParse(data)
        if (!parsed.success || response.status !== 202) return inconsistent()
        const receipt = parsed.data
        saveCommand({ ...command, position: observed })
        lookup = true
        report('Admitted · sequence ' + receipt.sequence)
        continue
      }
      if (response.ok) {
        const parsed = outcome.safeParse(data)
        if (!parsed.success || response.status !== 200) return inconsistent()
        const status = parsed.data
        saveCommand({ ...command, position: observed })
        if (status.status === 'accepted') {
          const [event] = status.events
          if (
            status.events.length !== 1 ||
            !event ||
            (command.player && event.playerId !== command.player) ||
            (command.credits === null
              ? event.type !== 'BetCashedOut'
              : event.type !== 'BetPlaced' ||
                event.betCredits !== command.credits)
          )
            return inconsistent()
          return finish(
            event.type === 'BetCashedOut'
              ? 'Paid ' + event.payoutCredits + ' credits'
              : 'Bet accepted',
            event.playerId,
          )
        }
        if (status.status === 'rejected')
          return finish('Rejected · ' + status.reason)
      } else if (response.status < 500) {
        const error = failure.safeParse(data)
        if (
          !lookup &&
          error.success &&
          ((response.status === 404 && error.data.code === 'round_not_found') ||
            (response.status === 409 &&
              [
                'round_closed',
                'round_sealed',
                'insufficient_credits',
                'command_id_reused',
              ].includes(error.data.code)))
        ) {
          return finish('Refused · ' + error.data.code)
        }
        if (!command.player && response.status === 404) {
          report(
            'Legacy command outcome unknown. Retry lookup with the original player token, or leave this session to start again.',
          )
          return 'paused'
        }
        const detail = error.success
          ? error.data.code
          : 'HTTP ' + response.status
        report(
          (observed ? 'Admitted · outcome unavailable' : 'Admission unknown') +
            ' · ' +
            detail,
        )
        return 'paused'
      } else {
        report(
          observed
            ? 'Admitted · waiting for command outcome…'
            : 'Recovering command…',
        )
      }
    } catch {
      if (signal.aborted) return
      report(
        observed
          ? 'Admitted · waiting for command outcome…'
          : 'Recovering command…',
      )
    }
    await new Promise((resolve) => setTimeout(resolve, 1000))
  }
}
