# Contributing

Issues and pull requests are welcome. For a security issue, follow [SECURITY.md](SECURITY.md) instead of opening an issue.

## Module layout

The repository holds five Go modules, each with its own `go.mod`:

| Directory | Module | What it is |
| --- | --- | --- |
| `.` | `github.com/aswin-kevin/orchestrix` | The core: the agent loop, providers, middleware, backends, permissions, the `orx` CLI (`cmd/orx`) and the examples. |
| `orxotel` | `.../orchestrix/orxotel` | OpenTelemetry tracing. |
| `orxpg` | `.../orchestrix/orxpg` | PostgreSQL checkpointer and key/value store (`pgx/v5`). |
| `orxdocker` | `.../orchestrix/orxdocker` | Hardened Docker containers as a sandbox executor. |
| `evals` | `.../orchestrix/evals` | Repeated-trial evaluations against real models, and the `orxeval` command. |

The core module depends on exactly two modules outside the standard library: `gopkg.in/yaml.v3` and `golang.org/x/net`. Keep it that way: a feature that needs another dependency belongs in a satellite module. The satellites build against the core in the same checkout through a `replace github.com/aswin-kevin/orchestrix => ../` directive.

## Checks

Every change must pass these in each module it touches (run them from the module's directory):

```sh
gofmt -l .                      # prints nothing
go vet ./...
GOOS=windows go vet ./...
go test -race -count=1 ./...
go mod tidy -diff               # prints nothing
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

CI runs the same checks for every module, the core tests on Linux and Windows with the minimum supported Go (1.26) and the latest stable release, a 60-second differential fuzz of the shell parser against `sh` and `bash`, and `govulncheck`.

Tests that run real shells live in `_test.go` files with a `//go:build unix` constraint and skip when the shell is missing. Exported struct fields need a doc comment or a line comment; `internal/doccheck` fails otherwise.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary`, for example `fix(permission): refuse --output for every git subcommand`. Common types are `feat`, `fix`, `docs`, `test`, `refactor` and `ci`; the scope is the package or module. The body explains why the change is needed. Mark a breaking change with `!` after the type or scope, and add a line to `CHANGELOG.md` under `## Unreleased` for it (and for any other change users will notice).

## Docker and PostgreSQL tests

The `orxdocker` and `orxpg` tests that need a real daemon or database skip unless configured:

```sh
cd orxdocker && ORX_TEST_DOCKER=1 go test -race -timeout 15m ./...
cd orxpg && ORX_TEST_PG_DSN='postgres://user:pass@localhost:5432/db?sslmode=disable' go test -race ./...
```

With `ORX_TEST_DOCKER=1` the Docker tests fail rather than skip when the daemon cannot be reached; `ORX_TEST_DOCKER_IMAGE` overrides the image. The PostgreSQL tests need a role that can create schemas: each test migrates its tables into a schema of its own and drops it afterwards.

## Live tests and evals

The live tests (`TestLive*` in the root package) and the evals call a real model and never run as part of `go test ./...`. Both read `ORX_TEST_MODEL` (a spec such as `anthropic:<model>` or `openai:<model>`), `ORX_TEST_BASE_URL` and `ORX_TEST_API_KEY`:

```sh
ORX_TEST_MODEL=openai:<model> ORX_TEST_BASE_URL=<url> ORX_TEST_API_KEY=<key> go test -run Live -count=1 -v .
cd evals && ORX_TEST_MODEL=... ORX_TEST_BASE_URL=... ORX_TEST_API_KEY=... go run ./cmd/orxeval -n 5 -out results.json
```

See "Live tests and evals" in the [README](README.md#live-tests-and-evals) for baselines and the nightly `Live` workflow.

## Releases

The core is tagged first, then the satellites. Each satellite's `go.mod` requires the core release it was tested against, so after tagging the core (`vX.Y.Z`), bump the core requirement in `orxotel`, `orxpg`, `orxdocker` and `evals` to it, run the checks, commit, and tag the satellites with their module prefix (`orxotel/vX.Y.Z`, `orxpg/vX.Y.Z`, `orxdocker/vX.Y.Z`). `internal/version` holds the core release number (sent in the MCP client info and the web User-Agent); bump it before tagging the core. Tag a release only when the latest `Live` run is green for every configured model.
