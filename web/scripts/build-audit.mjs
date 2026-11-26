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
const generated = 'src/generated'
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
mkdirSync(generated, { recursive: true })
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
  renameSync(join(staging, 'wasm_exec.js'), join(generated, 'wasm_exec.js'))
  renameSync(join(staging, 'verify.wasm'), join(dir, 'verify.wasm'))
  rmSync(join(dir, 'wasm_exec.js'), { force: true })
} finally {
  rmSync(staging, { recursive: true, force: true })
}
