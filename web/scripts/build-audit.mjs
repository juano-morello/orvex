import { execFileSync } from 'node:child_process'
import {
  mkdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
  mkdtempSync,
} from 'node:fs'
import { join } from 'node:path'

const dir = 'public/audit'
const go = process.env.GO || 'go'
const env = {
  ...process.env,
  GOTOOLCHAIN: 'go1.26.11',
  GOOS: 'js',
  GOARCH: 'wasm',
  CGO_ENABLED: '0',
}
const goroot = execFileSync(go, ['env', 'GOROOT'], {
  env,
  encoding: 'utf8',
}).trim()
mkdirSync(dir, { recursive: true })
const staging = mkdtempSync(join(dir, '.build-'))
try {
  writeFileSync(
    join(staging, 'wasm_exec.js'),
    readFileSync(join(goroot, 'lib/wasm/wasm_exec.js')),
  )
  execFileSync(
    go,
    [
      'build',
      '-trimpath',
      '-ldflags=-s -w',
      '-o',
      join(staging, 'verify.wasm'),
      '../cmd/receiptwasm',
    ],
    { env, stdio: 'inherit' },
  )
  for (const name of ['wasm_exec.js', 'verify.wasm'])
    renameSync(join(staging, name), join(dir, name))
} finally {
  rmSync(staging, { recursive: true, force: true })
}
