// Package version provides this module's build-time version string for
// every binary in cmd/ — injected via -ldflags at build time (see the
// Makefile's VERSION/LDFLAGS variables) from `git describe --tags
// --always --dirty`. Falls back to "dev" for a bare `go build` outside
// the Makefile (e.g. `go test`, an IDE build) — never what actually
// runs in production, since every real build goes through the Makefile.
package version

var Version = "dev"
