package buildinfo

import "testing"

func TestStringDefaultVersion(t *testing.T) {
	if got, want := String("gateway"), "gateway dev"; got != want {
		t.Fatalf("String(gateway) = %q, want %q", got, want)
	}
}

func TestStringInjectedVersion(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "v1.2.3"

	if got, want := String("engine"), "engine v1.2.3"; got != want {
		t.Fatalf("String(engine) = %q, want %q", got, want)
	}
}
