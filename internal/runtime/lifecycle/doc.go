// Package lifecycle is the engine's crash/v1 round scheduler: it creates
// rounds, starts, closes and settles them without a human, and adopts the
// rounds an earlier engine process left unsettled.
//
//   - Discovery: every scheduler lists the crash rounds that have no
//     round_secret_reveals row, which are the unsettled ones, and drives
//     each to settlement before it creates a round. A round whose creation
//     committed while its creator died is adopted here.
//   - Adoption: execution.ClaimLease raises the round's fencing token for
//     this process's owner ID, so an earlier engine still running is fenced
//     at its next Transaction B and stops (ErrFenced).
//   - Creation: a transaction advisory lock serializes the check for an
//     unsettled round and the genesis write. One plan, made of a random UUIDv4 round ID, a crypto/rand
//     seed and created_at from the database clock, is drawn once and sent
//     unchanged until CreateRound answers definitely. A retry after an
//     unknown outcome is therefore Duplicate, never a second seed.
//   - Driving: lifecycle admissions lock and validate the live lease in
//     the same transaction as the admission. A table maps each phase to its one system command, with a
//     command ID derived from the round and the step, so a restarted
//     engine's admission of a command the earlier one admitted is a
//     Duplicate. Start is due at created_at + BettingWindow, close at the
//     crash deadline T0 + D read from the round's state, settle at once;
//     the database clock decides. Each tick executes at most one batch of
//     admitted commands, then checks the clock, so a due step is admitted
//     even when admissions never stop, behind those admitted before it.
//     The driver then waits for the step's admission sequence to execute.
//     The final record ends the drive: the executor has dropped the
//     round's state, and nothing reads it again.
//   - Halts: a round whose execution is corrupt or fails closed, or whose
//     lifecycle command is rejected, halts. It stays unsettled, blocks new
//     rounds and is logged once per scheduler. Nothing voids it.
//
// Logs carry round IDs, phases, fencing tokens and error classes
// (Describe), never a seed, the crash deadline, its offset or text from the
// database driver, which can name the database host, user and name. The
// crash outcome is still readable at rest before RoundCrashed (residual
// risk R-1, LBT-26); nothing here changes that.
package lifecycle
