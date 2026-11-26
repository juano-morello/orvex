import { useEffect, useRef, useState } from 'react'
import { restoreCommand } from './commands'

export function VerifyReceipt() {
  const [message, setMessage] = useState('')
  const worker = useRef<Worker | null>(null)
  function reset() {
    worker.current?.terminate()
    worker.current = null
    setMessage('')
  }
  useEffect(() => () => worker.current?.terminate(), [])
  const claim = restoreCommand()
  if (
    !claim?.result ||
    !(
      claim.result === 'Bet accepted' ||
      claim.result.startsWith('Paid ') ||
      claim.result.startsWith('Rejected · ')
    )
  )
    return null
  return (
    <section>
      <h2>Verify settled receipt</h2>
      <p>
        Select the operator’s settled v2 export and enter the expected head from
        a trusted separate record. Verification runs on this device.
      </p>
      <form
        onChange={reset}
        onSubmit={(event) => {
          event.preventDefault()
          reset()
          const form = new FormData(event.currentTarget)
          const file = form.get('history')
          if (!(file instanceof File)) return
          if (file.size > 32 * 1024 * 1024) {
            setMessage('Browser limit is 32 MiB.')
            return
          }
          const audit = new Worker(
            new URL('./receipt-worker.ts', import.meta.url),
            { type: 'module' },
          )
          worker.current = audit
          setMessage('Checking history and receipt…')
          audit.onmessage = ({ data }) => {
            if (worker.current !== audit) return
            worker.current = null
            setMessage(data)
            audit.terminate()
          }
          audit.onerror = () => {
            if (worker.current !== audit) return
            worker.current = null
            setMessage('Verifier could not run.')
            audit.terminate()
          }
          audit.postMessage({
            claim,
            file,
            head: form.get('head'),
            player: form.get('player'),
          })
        }}
      >
        <label>
          Player ID
          <input
            name="player"
            defaultValue={claim.player}
            required
            pattern="[a-z0-9_\-]{1,64}"
          />
        </label>
        <label>
          Expected chain head
          <input
            name="head"
            required
            pattern="[0-9a-f]{64}"
            spellCheck={false}
          />
        </label>
        <label>
          Settled history
          <input name="history" type="file" required />
        </label>
        <button>Verify receipt</button>
        {worker.current && (
          <button
            type="button"
            onClick={() => {
              reset()
              setMessage('Verification cancelled.')
            }}
          >
            Cancel verification
          </button>
        )}
      </form>
      <p aria-live="polite">{message}</p>
    </section>
  )
}
