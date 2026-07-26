// Package pgtest resolves the PostgreSQL DSN the integration tests run
// against.
//
// It exists because the tests are destructive: they DROP and recreate the
// runtime tables. Several packages previously hardcoded
// postgres://runtime:runtime@localhost:5432/runtime as an untouchable const, so
// pointing the suite at a scratch database was impossible — the documented
// RUNTIME_PG_DSN override silently did nothing and the tests dropped tables in
// whatever lived at that address. One package honoured RUNTIME_TEST_PG_DSN,
// which nothing sets. Resolving the DSN in one place, from either variable,
// makes the override real.
package pgtest

import "os"

// DefaultDSN is the local development database the Makefile provisions with
// `make pg-up`. It stays the fallback so a plain `go test -tags integration`
// still works on a developer machine.
const DefaultDSN = "postgres://runtime:runtime@localhost:5432/runtime?sslmode=disable"

// DSN returns the database the integration tests may DROP tables in.
// RUNTIME_TEST_PG_DSN wins over RUNTIME_PG_DSN so a developer can aim the
// destructive suite somewhere other than the database the rest of their tooling
// is pointed at; both are honoured because the Makefile exports the latter.
func DSN() string {
	if v := os.Getenv("RUNTIME_TEST_PG_DSN"); v != "" {
		return v
	}
	if v := os.Getenv("RUNTIME_PG_DSN"); v != "" {
		return v
	}
	return DefaultDSN
}
