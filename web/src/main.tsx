import { VerifyReceipt } from './VerifyReceipt'
import { useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { recover, restoreCommand, saveCommand } from './commands'
import { currentRound, frameSchema } from './wire'
import './style.css'

function Console({
  token,
  updateToken,
}: {
  token: string
  updateToken: (value: string) => void
}) {
  const [command, setCommand] = useState(restoreCommand)
  const [round, setRound] = useState(
    command?.round ?? sessionStorage.getItem('orvex.round') ?? '',
  )
  const [phase, setPhase] = useState('Connecting')
  const [events, setEvents] = useState<string[]>([])
  const [start, setStart] = useState(0)
  const [multiplier, setMultiplier] = useState('1.00')
  const [credits, setCredits] = useState('100')
  const [receipt, setReceipt] = useState(command?.result ?? '')
  const [busy, setBusy] = useState(Boolean(command && !command.result))

  const [seeking, setSeeking] = useState(false)
  const [connected, setConnected] = useState(false)
  const [paused, setPaused] = useState(false)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    if (round && !seeking) return
    const ctrl = new AbortController()
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
              await new Promise((resolve) => setTimeout(resolve, 1000))
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
        await new Promise((resolve) => setTimeout(resolve, 1000))
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
    let cursor = -1n
    setEvents([])
    setPhase('Connecting')
    setMultiplier('1.00')
    setStart(0)
    function subscribe() {
      const after = cursor < 0n ? '' : '?after=' + cursor
      stream = new EventSource(
        import.meta.env.BASE_URL + 'v1/rounds/' + round + '/stream' + after,
      )
      stream.onopen = () => {
        if (!disposed) setConnected(true)
      }
      stream.onerror = () => {
        stream.close()
        if (disposed) return
        setConnected(false)
        retry = setTimeout(subscribe, 1000)
      }
      stream.addEventListener('record', (frame) => {
        if (disposed) return
        const record = frameSchema.parse(JSON.parse(frame.data))
        const seq = BigInt(record.sequence)
        if (seq <= cursor) return
        cursor = seq
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
              stream.close()
              break
            case 'BetPlaced':
              setEvents((rows) => [
                ...rows,
                event.playerId + ' bet ' + event.betCredits,
              ])
              break
            case 'BetCashedOut':
              setEvents((rows) => [
                ...rows,
                event.playerId + ' received ' + event.payoutCredits,
              ])
              break
            case 'BetLost':
              setEvents((rows) => [
                ...rows,
                event.playerId + ' lost ' + event.betCredits,
              ])
              break
          }
        }
      })
      stream.addEventListener('end', () => stream.close())
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

  function submit(amount: string | null) {
    const next = { round, id: crypto.randomUUID(), credits: amount }
    saveCommand(next)
    setBusy(true)
    setReceipt('Submitting…')
    setCommand(next)
  }

  return (
    <>
      <section className="board">
        <span>
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
          {paused && (
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
                Updated token for this player (optional)
                <input type="password" name="replacement" autoComplete="off" />
              </label>
              <button>Retry command recovery</button>
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
          <ul>
            {events.map((line, index) => (
              <li key={index}>{line}</li>
            ))}
          </ul>
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
        <b>ORVEX</b>
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
