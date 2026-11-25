export function requireAuditSuccess(verdict: unknown, exited: boolean): void {
  if (verdict === true && !exited) return
  if (!exited && typeof verdict === 'string' && verdict) throw Error(verdict)
  throw Error('Verifier did not complete. Use crashctl to verify this export.')
}
