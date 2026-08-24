module github.com/kaitsehuang780911/fleet-telemetry

go 1.26

// One module for the whole repo. `api/` and `sim/` share the telemetry wire
// format, which lives in `internal/wire/`. Go scopes an `internal/` package to
// the subtree rooted at its parent directory, so a root-level internal/ is
// importable from both api/ and sim/ — whereas api/internal/ would be private
// to api/. Two modules would have needed a go.work file or a replace directive
// to share the same types.
