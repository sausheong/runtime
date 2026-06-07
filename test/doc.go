// Package test holds cross-component integration tests.
//
// The integration tests require a running Postgres and are gated behind the
// `integration` build tag, so the default `go test ./...` run stays hermetic.
// Run them with: go test -tags integration ./test/
package test
