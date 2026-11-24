//go:build integration && acceptance && race

package firstround

// raceEnabled builds the services with -race when the test binary has it,
// so a data race in a service fails its exit status check.
const raceEnabled = true
