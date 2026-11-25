import assert from 'node:assert/strict'
import { test } from 'node:test'
import { requireAuditSuccess } from '../src/audit-verdict.ts'

test('receipt matching requires a completed audit and a running verifier', () => {
  requireAuditSuccess(true, false)
  for (const value of [undefined, null, false, 0, '', {}, 'failed']) {
    assert.throws(() => requireAuditSuccess(value, false))
  }
  assert.throws(() => requireAuditSuccess(true, true))
  assert.throws(() => requireAuditSuccess(undefined, true))
})
