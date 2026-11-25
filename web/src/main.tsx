import { VerifyReceipt } from './VerifyReceipt'
import { z } from 'zod'
import { memo, useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { recover, restoreCommand, saveCommand } from './commands'
import { currentRound, frameSchema } from './wire'
import './style.css'

const sessionSchema = z.object({
  player: z.string().regex(/^[a-z0-9_-]{1,64}$/),
})
const retryDelay = (attempt: number) =>
  (0.5 + Math.random() * 0.5) * Math.min(8000, 1000 * 2 ** Math.min(attempt, 3))

type ActivityBatch = {
  entries: string[]
  total: number
  previous: ActivityBatch | null
}

const Activity = memo(function Activity({
  history,
}: {
  history: ActivityBatch | null
}) {
  const [pageEnd, setPageEnd] = useState<number | null>(null)
  const total = history?.total ?? 0
  const end = Math.min(pageEnd ?? total, total)
  const start = Math.max(0, end - 100)
  const rows: string[] = []
  for (
    let batch = history;
    batch && batch.total > start;
    batch = batch.previous
  ) {
    const offset = batch.total - batch.entries.length
    for (
      let i = Math.min(end, batch.total) - 1;
      i >= Math.max(offset, start);
      i--
    ) {
      rows.push(batch.entries[i - offset])
    }
  }
  return (
    <>
      <ul>
        {rows.reverse().map((line, i) => (
          <li key={start + i}>{line}</li>
        ))}
      </ul>
      {start > 100 && (
        <button onClick={() => setPageEnd(100)}>First activity</button>
      )}
      {start > 0 && (
        <button onClick={() => setPageEnd(start)}>Earlier activity</button>
      )}
      {end + 100 < total && (
        <button onClick={() => setPageEnd(null)}>Latest activity</button>
      )}
      {end < total && (
        <button
          onClick={() => setPageEnd(end + 100 >= total ? null : end + 100)}
        >
          Later activity
        </button>
      )}
    </>
  )
})

function Console({
  token,
  updateToken,
}: {
  token: string
  updateToken: (value: string) => void
}) {
  const [command, setCommand] = useState(restoreCommand)
  const [round, setRound] = useState(
    command?.round ??
      z.uuid().safeParse(sessionStorage.getItem('orvex.round')).data ??
      '',
  )
  const [phase, setPhase] = useState('Connecting')
  const [events, setEvents] = useState<ActivityBatch | null>(null)
  const [start, setStart] = useState(0)
  const [multiplier, setMultiplier] = useState('1.00')
  const [credits, setCredits] = useState('100')
  const [receipt, setReceipt] = useState(command?.result ?? '')
  const [busy, setBusy] = useState(Boolean(command && !command.result))

  const [seeking, setSeeking] = useState(false)
  const [connected, setConnected] = useState(false)
  const [paused, setPaused] = useState(false)
  const [attempt, setAttempt] = useState(0)

  const [identity, setIdentity] = useState<{
    token: string
    player: string
  } | null>(null)
  const [authStatus, setAuthStatus] = useState<'waiting' | 'invalid' | 'ready'>(
    'waiting',
  )
  const player = identity?.token === token ? identity.player : undefined
  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    let failures = 0
    async function identify() {
      try {
        const res = await fetch(import.meta.env.BASE_URL + 'v1/session', {
          headers: { Authorization: 'Bearer ' + token },
          signal: controller.signal,
          cache: 'no-store',
        })
        if (controller.signal.aborted) return
        if (res.status === 401) {
          setAuthStatus('invalid')
          return
        }
        if (!res.ok) throw Error()
        const session = sessionSchema.parse(await res.json())
        if (!controller.signal.aborted) {
          setIdentity({ token, player: session.player })
          setAuthStatus('ready')
        }
      } catch {
        if (controller.signal.aborted) return
        setAuthStatus('waiting')
        timer = setTimeout(identify, retryDelay(failures++))
      }
    }
    void identify()
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [token])
  useEffect(() => {
    if (round && !seeking) return
    const ctrl = new AbortController()
    let failures = 0
    async function discover() {
      while (!ctrl.signal.aborted) {
        try {
          const res = await fetch(
            import.meta.env.BASE_URL + 'v1/rounds/current',
            { signal: ctrl.signal },
          )
          if (res.ok) {
            const { roundId } = currentRound.parse(await res.json())
            if (ctrl.signal.aborted) return
            if (roundId === round) {
              failures = 0
              await new Promise((resolve) =>
                setTimeout(resolve, retryDelay(failures++)),
              )
              continue
            }
            saveCommand(null)
            setCommand(null)
            setReceipt('')
            setSeeking(false)
            setConnected(false)
            setPhase('Connecting')
            sessionStorage.setItem('orvex.round', roundId)
            setRound(roundId)
            return
          }
        } catch {
          if (ctrl.signal.aborted) return
        }
        await new Promise((resolve) =>
          setTimeout(resolve, retryDelay(failures++)),
        )
      }
    }
    void discover()
    return () => ctrl.abort()
  }, [round, seeking])

  useEffect(() => {
    if (!round) return
    let stream: EventSource
    let retry: ReturnType<typeof setTimeout>
    let disposed = false
    let failures = 0
    let cursor = -1n
    setEvents(null)
    setPhase('Connecting')
    setMultiplier('1.00')
    setStart(0)
    function subscribe() {
      const after = cursor < 0n ? '' : '?after=' + cursor
      const source = new EventSource(
        import.meta.env.BASE_URL + 'v1/rounds/' + round + '/stream' + after,
      )
      stream = source
      let closed = false
      const reconnect = () => {
        if (disposed || closed) return
        closed = true
        source.close()
        setConnected(false)
        retry = setTimeout(subscribe, retryDelay(failures++))
      }
      source.onopen = () => {
        if (!disposed && !closed) setConnected(true)
      }
      source.onerror = reconnect
      source.addEventListener('record', (frame) => {
        if (disposed || closed) return
        let record
        try {
          record = frameSchema.parse(JSON.parse(frame.data))
        } catch {
          reconnect()
          return
        }
        const seq = BigInt(record.sequence)
        if (seq <= cursor) return
        if (
          seq !== cursor + 1n ||
          Boolean(record.genesis) !== (seq === 0n) ||
          Boolean(record.events) === Boolean(record.genesis)
        ) {
          reconnect()
          return
        }
        cursor = seq
        failures = 0
        setConnected(true)
        const activity: string[] = []
        if (record.genesis) setPhase('Betting open')
        for (const event of record.events ?? []) {
          switch (event.type) {
            case 'RoundStarted':
              setStart(Number(event.startedAtUnixMicro) / 1000)
              setPhase('Running')
              break
            case 'RoundCrashed':
              setStart(0)
              setMultiplier((Number(event.crashPointScaled) / 10000).toFixed(4))
              setPhase('Crashed')
              break
            case 'RoundSettled':
              setPhase('Settled')
              closed = true
              source.close()
              break
            case 'BetPlaced':
              activity.push(event.playerId + ' bet ' + event.betCredits)
              break
            case 'BetCashedOut':
              activity.push(event.playerId + ' received ' + event.payoutCredits)
              break
            case 'BetLost':
              activity.push(event.playerId + ' lost ' + event.betCredits)
              break
          }
        }
        if (activity.length)
          setEvents((previous) => ({
            entries: activity,
            total: (previous?.total ?? 0) + activity.length,
            previous,
          }))
      })
      source.addEventListener('end', reconnect)
    }
    subscribe()
    return () => {
      disposed = true
      clearTimeout(retry)
      stream.close()
    }
  }, [round])

  useEffect(() => {
    if (!start || !connected) return
    const timer = setInterval(() => {
      const tick = Math.max(0, Math.floor((Date.now() - start) / 10))
      setMultiplier(Math.min(10000, Math.exp(0.0006 * tick)).toFixed(2))
    }, 50)
    return () => clearInterval(timer)
  }, [start, connected])

  useEffect(() => {
    if (!command || command.result) return
    const ctrl = new AbortController()
    setBusy(true)
    setPaused(false)
    void recover(
      restoreCommand() ?? command,
      token,
      ctrl.signal,
      setReceipt,
    ).then((result) => {
      if (ctrl.signal.aborted) return
      setPaused(result === 'paused')
      setBusy(result === 'paused')
    })
    return () => ctrl.abort()
  }, [command, token, attempt])

  useEffect(() => {
    if (!busy) return
    const warn = (event: BeforeUnloadEvent) => event.preventDefault()
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [busy])

  function submit(amount: string | null) {
    if (!player || busy) return
    const next = { round, player, id: crypto.randomUUID(), credits: amount }
    saveCommand(next)
    setBusy(true)
    setReceipt('Submitting…')
    setCommand(next)
  }

  return (
    <>
      <section className="board">
        <span aria-live="polite">
          {connected || !round
            ? phase
            : 'Round feed unavailable · reconnecting…'}
        </span>
        <strong>{connected ? multiplier + '×' : '—'}</strong>
        <small>
          {phase === 'Running'
            ? 'Estimated live multiplier'
            : 'Crash multiplier'}
        </small>
        <code>Round {round || '…'}</code>
      </section>
      <div className="columns">
        <section>
          <h2>Your play</h2>
          <p>
            {authStatus === 'invalid'
              ? 'Enter a valid player token to continue.'
              : authStatus === 'waiting'
                ? 'Waiting for player authentication…'
                : busy
                  ? 'Finish command recovery before playing again.'
                  : 'Place a bet while betting is open. Cash out while the round is running.'}
          </p>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              submit(credits)
            }}
          >
            <label>
              Simulated credits
              <input
                value={credits}
                onChange={(e) => setCredits(e.target.value)}
                required
                pattern="[1-9][0-9]{0,9}"
                inputMode="numeric"
              />
            </label>
            <button
              disabled={
                busy ||
                !player ||
                seeking ||
                receipt === 'Bet accepted' ||
                !connected ||
                phase !== 'Betting open' ||
                Number(credits) > 1e9
              }
            >
              Place bet
            </button>
            <button
              type="button"
              disabled={
                busy ||
                !player ||
                seeking ||
                !connected ||
                receipt.startsWith('Paid ') ||
                phase !== 'Running'
              }
              onClick={() => submit(null)}
            >
              Cash out
            </button>
          </form>
          <p role="status">{receipt}</p>
          {phase === 'Settled' && !busy && <VerifyReceipt />}
          {seeking && (
            <button onClick={() => setSeeking(false)}>
              Stay on this round
            </button>
          )}
          {(paused || authStatus === 'invalid') && (
            <form
              onSubmit={(event) => {
                event.preventDefault()
                const next = new FormData(event.currentTarget).get(
                  'replacement',
                )
                if (typeof next === 'string' && next.trim())
                  updateToken(next.trim())
                setAttempt((n) => n + 1)
              }}
            >
              <label>
                {paused
                  ? 'Updated token for this player (optional)'
                  : 'Replacement player token'}
                <input
                  type="password"
                  name="replacement"
                  autoComplete="off"
                  required={!paused}
                />
              </label>
              <button>
                {paused ? 'Retry command recovery' : 'Update player token'}
              </button>
            </form>
          )}
          {(seeking || phase === 'Settled' || !connected) && round && (
            <button disabled={busy || seeking} onClick={() => setSeeking(true)}>
              {seeking ? 'Waiting for a new round…' : 'Join current round'}
            </button>
          )}
        </section>
        <section>
          <h2>Round activity</h2>
          <Activity key={round} history={events} />
        </section>
      </div>
    </>
  )
}

function App() {
  const [token, setToken] = useState(
    sessionStorage.getItem('orvex.token') ?? '',
  )
  return (
    <main>
      <header>
        {token ? <h1>Orvex Crash</h1> : <b>ORVEX</b>}
        <span>Crash / Simulated credits</span>
      </header>
      {token ? (
        <Console
          token={token}
          updateToken={(value) => {
            sessionStorage.setItem('orvex.token', value)
            setToken(value)
          }}
        />
      ) : (
        <section>
          <h1>Join a round</h1>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              const value = new FormData(e.currentTarget).get('token')
              if (typeof value === 'string' && value.trim()) {
                sessionStorage.setItem('orvex.token', value.trim())
                setToken(value.trim())
              }
            }}
          >
            <label>
              Player token
              <input name="token" type="password" required autoComplete="off" />
            </label>
            <button>Join round</button>
          </form>
        </section>
      )}
      {token && (
        <button
          className="leave"
          onClick={() => {
            if (
              !window.confirm(
                'Leave this player session and clear its saved command?',
              )
            )
              return
            saveCommand(null)
            sessionStorage.removeItem('orvex.round')
            sessionStorage.removeItem('orvex.token')
            setToken('')
          }}
        >
          Leave session
        </button>
      )}
    </main>
  )
}

const root = document.getElementById('root')
if (root) createRoot(root).render(<App />)
