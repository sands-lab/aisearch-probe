package main

import "testing"

// orEnv backfills a secret flag from the environment AFTER the env file is loaded,
// because a flag's os.Getenv default is captured before loadEnvFile runs. The rule:
// an explicit flag wins; otherwise use the env; otherwise empty.
func TestOrEnv(t *testing.T) {
	const key = "PROBE_TEST_ORENV"

	// A value passed on the flag wins, even when the env is also set.
	t.Setenv(key, "from-env")
	if got := orEnv("from-flag", key); got != "from-flag" {
		t.Errorf("explicit flag must win, got %q", got)
	}
	// An empty flag falls back to the env — this is the env-file-only case the fix
	// is for.
	if got := orEnv("", key); got != "from-env" {
		t.Errorf("empty flag must fall back to env, got %q", got)
	}
	// Both empty stays empty (so the caller still errors clearly).
	t.Setenv(key, "")
	if got := orEnv("", key); got != "" {
		t.Errorf("both empty must be empty, got %q", got)
	}
}
