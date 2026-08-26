module github.com/KaiTseHuang780911/fleet-telemetry

go 1.26

// One module for the whole repo. `api/` and `sim/` share the telemetry wire
// format, which lives in `internal/wire/`. Go scopes an `internal/` package to
// the subtree rooted at its parent directory, so a root-level internal/ is
// importable from both api/ and sim/ — whereas api/internal/ would be private
// to api/. Two modules would have needed a go.work file or a replace directive
// to share the same types.

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/pressly/goose/v3 v3.27.3
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)
