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
    if (saved?.position && saved.result?.startsWith('Refused · ')) {
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
  let observed = command.position
  function finish(result: string, player?: string) {
    saveCommand({ ...command, result, player, position: observed })
    report(result)
  }
  const headers = {
    Authorization: 'Bearer ' + token,
    'Content-Type': 'application/json',
  }
  const payload =
    command.credits === null
      ? { cashOut: {} }
      : { placeBet: { betCredits: command.credits } }
  const body = JSON.stringify({ clientCommandId: command.id, ...payload })
  let admitted = Boolean(observed)
  while (!signal.aborted) {
    try {
      const response = admitted
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
        const next = position.parse(data)
        if (
          next.roundId !== command.round ||
          next.clientCommandId !== command.id ||
          (observed &&
            (next.sequence !== observed.sequence ||
              next.effectiveTimeUnixMicro !== observed.effectiveTimeUnixMicro))
        ) {
          report('Inconsistent gateway receipt · outcome unavailable')
          return 'paused'
        }
        observed = next
      }
      if (response.ok && !admitted) {
        const receipt = admission.parse(data)
        saveCommand({ ...command, position: observed })
        admitted = true
        report('Admitted · sequence ' + receipt.sequence)
        continue
      }
      if (response.ok) {
        const status = outcome.parse(data)
        if (status.status === 'accepted') {
          const [event] = status.events
          if (
            status.events.length !== 1 ||
            !event ||
            (command.credits === null
              ? event.type !== 'BetCashedOut'
              : event.type !== 'BetPlaced' ||
                event.betCredits !== command.credits)
          ) {
            report('Inconsistent gateway receipt · outcome unavailable')
            return 'paused'
          }
          return finish(
            event.type === 'BetCashedOut'
              ? 'Paid ' + event.payoutCredits + ' credits'
              : 'Bet accepted',
            event.playerId,
          )
        }
        if (status.status === 'rejected')
          return finish('Rejected · ' + status.reason)
      } else if (
        response.status < 500 &&
        !(admitted && response.status === 404)
      ) {
        const error = failure.safeParse(data)
        if (
          !admitted &&
          error.success &&
          [
            'invalid_request',
            'unauthenticated',
            'round_not_found',
            'round_closed',
            'round_sealed',
            'insufficient_credits',
            'command_id_reused',
            'request_too_large',
          ].includes(error.data.code)
        ) {
          return finish('Refused · ' + error.data.code)
        }
        const detail = error.success
          ? error.data.code
          : 'HTTP ' + response.status
        report(
          (admitted ? 'Admitted · outcome unavailable' : 'Admission unknown') +
            ' · ' +
            detail,
        )
        return 'paused'
      } else if (admitted) {
        report('Admitted · waiting for command outcome…')
      }
    } catch {
      if (signal.aborted) return
      report(
        admitted
          ? 'Admitted · waiting for command outcome…'
          : 'Recovering command…',
      )
    }
    await new Promise((resolve) => setTimeout(resolve, 1000))
  }
}
