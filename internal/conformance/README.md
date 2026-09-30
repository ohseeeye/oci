# OCI distribution conformance

Each backend owns an integration-tagged `TestOCIConformance` in its package.
This directory provides the shared runner for the official OCI distribution-spec
suite, which tests that backend through `ociserver`.

Docker must be installed and running. Run every backend with:

```console
task conformance
```

Individual backends can be selected with `task conformance:ocimem`,
`task conformance:ocilayout`, or `task conformance:ocisqlite`.

The harness builds a pinned version of the upstream conformance runner, starts
each registry on a temporary local port, and fails when the upstream runner
reports a conformance failure. HTML, YAML, and JUnit reports are written under
`results/<backend>/run-*/` at the repository root when using the tasks above.
Previous reports are retained.

The pull-request workflow also runs all backend conformance tests as a
separate `OCI conformance` check, using the same shared results location.

## Testing another backend or module

The reusable harness lives in `internal/conformance`. It embeds the pinned
Dockerfile and configuration, so callers do not need to locate the root checkout
or copy runner assets. Its Go dependencies are the standard library and this
module's core registry/server packages, not individual storage backends.

The `github.com/ohseeeye/oci/ocisqlite` module uses this runner from its
integration test. Another module can use the same pattern:

```go
//go:build integration

package ocisqlite_test

import (
	"testing"

	"github.com/ohseeeye/oci"
	"github.com/ohseeeye/oci/internal/conformance"
)

// Call this with a registry created by the backend's test setup.
func checkConformance(t *testing.T, registry oci.Registry) {
	t.Helper()
	conformance.Run(t, "ocisqlite", registry)
}
```

Import paths underneath `github.com/ohseeeye/oci` can use this internal package,
even when they have their own `go.mod`. The nested module should require the root
module; during local development, a `replace github.com/ohseeeye/oci => ..`
directive (for a module directly under the repository root) can select the local
checkout. SQLite dependencies remain in that nested module.

Run `go test -tags=integration -count=1 -v ./...` from the backend module.
Reports are written to `results/<backend>/run-*/` under the calling test package's
working directory by default, and their absolute path is logged. Set
`OCI_CONFORMANCE_RESULTS` to an absolute directory to collect reports from
multiple packages or modules in one location. The Taskfile targets set this to
the repository's `results/` directory. For example, from the repository root:

```sh
OCI_CONFORMANCE_RESULTS="$PWD/results" go test -tags=integration ./ocimem ./ocilayout -run TestOCIConformance -count=1 -v
```

Docker must be able to mount the results directory and temporary runner assets.

`task conformance` runs the root-module backends and the nested SQLite module.
For each additional nested backend, add a task that runs its module's integration
tests and include it in the aggregate target; Go's `./...` does not cross module
boundaries.
