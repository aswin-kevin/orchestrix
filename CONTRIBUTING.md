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

Tests that run real shells live in `_test.go` files with a `//go:build unix` constraint and skip when the shell is missing. `internal/doccheck` fails when an exported field, type, function or method lacks a doc comment, or when a comment runs past 79 columns (see Code style).

## Code style

`gofmt`, `go vet` and staticcheck catch most problems. The rules below cover what they do not. Each names a file that follows it.

1. **Package comment in `doc.go`.** Every package (commands included, example programs excepted) keeps its `// Package x …` comment in `doc.go`, next to the package clause and nothing else. See `orxpg/doc.go`.
2. **File names.** snake_case, grouped by subject with a shared prefix; OS-specific files end in `_unix`, `_windows` or `_other`. Tests are named after what they test, never after a milestone or review; helper-only test files are `helpers_test.go`. See `mcp/http.go`, `mcp/http_server.go`, `mcp/http_session.go`.
3. **Constructors.** `New` builds a package's main type, `NewX` a secondary type, and a verb (`Dial`, `Start`, `Connect`) a value whose construction does I/O. See `tool/registry.go`.
4. **Configuration.** A `Config` struct passed by value. Its zero value is usable and every field documents its default ("0 means DefaultX"). Defaults are exported `DefaultX` constants. `New` validates and returns `(T, error)`: zero means the default, and the only accepted negative is an exported `= -1` constant named in the field doc (`agent.Unlimited`, `llm.NoRetries`, `llm.NoIdleTimeout`, `llm.NoCooldown`, `agent.NoOutputRetries`, `orxdocker.Unlimited`); any other negative makes `New` fail with "<pkg>: <Field> is negative". Per-call option structs and constructors without an error return document "0 or negative means the default" instead (`mcp.HandlerConfig`, `coordinator.ParallelOptions`; `backend.ExecOptions` says an implementation treats a negative value as 0). A required value may sit in `Config` when it mirrors a JSON config (`mcp.StdioConfig.Command`, `mcp.HTTPConfig.URL`). See `middleware/guard/guard.go`.
5. **Functional options** only on the `orchestrix` facade and in builders, named `WithX`. A flag that turns something on takes no argument. The `state.KeyOption` constructors (`Private`, `RunLocal`, `MergeWith`) are the accepted exception: key declarations read as adjectives. See `options.go`.
6. **Per-call parameter structs** are named `XOptions`. See `backend/backend.go`.
7. **Constructors return concrete exported types.** Callers that want an interface assign the result to one. The exceptions are constructors whose result depends on their input: `llm.New` returns the `Provider` chosen by the configured format, and `composite.New` returns a `backend.Backend` because the result's optional capabilities depend on its default backend. See `middleware/async/async.go`.
8. **Errors.** An error that crosses a package boundary starts with `"pkg: "`. A package name used twice in the module takes its last two path elements (`backend/memory: `). Errors the model reads have no prefix. Sentinels `ErrX` sit at the top of their file or in `errors.go`; error types are `XError` with documented methods. Wrap with `%w` and test with `errors.Is` / `errors.As`, never `==`. See `backend/errors.go`.
9. **Panics** only for programmer errors at declaration or registration, with the message `"pkg: Func …"`. Configuration errors are returned. See `llm/registry.go`.
10. **Typed enums.** `type X string` constants prefixed with the type's noun (`KindFinal`, `ReasonMaxTurns`). The value sets of event kinds, terminal reasons and statuses may grow in minor releases. Verb constants of a decision type (`permission.Allow`, `agent.Approve`) are the accepted exception. See `agent/event.go`.
11. **Bool fields** read so false is the default: `DisableX`, or `AllowX` for an opt-in risk. See `middleware/web/client.go`.
12. **Prompts.** A `Prompt *string` field is a prompt segment (nil means the default, a pointer to "" means none). A `Prompt string` field is a whole prompt ("" means the default). See `middleware/compress/compress.go`.
13. **Names of tools and middleware.** Tool names are `ToolX` constants with verb_noun values; every middleware package exports `const Name`. See `middleware/planning/planning.go`.
14. **Identifiers.** Initialisms all caps (`ThreadID`, `BaseURL`), no `Get` prefix, the same 1–2 letter receiver on every method of a type, and `-er` names for single-method interfaces (agent hook interfaces are named after their hook). Interfaces never gain methods after v1; new capabilities arrive as optional interfaces found by type assertion, and wrappers expose `Unwrap`. See `agent/agent.go`.
15. **Doc comments.** Full sentences starting with the identifier, wrapped at 79 columns (tabs count as 4), trailing comments included. Every exported field, type, function and method is documented. `internal/doccheck` enforces this. See `agent/config.go`.
16. **Context.** `ctx` is the first parameter of anything that may block. Library code uses `context.Background()` only for detached cleanup with a timeout, and `context.WithoutCancel` for work after a run. See `agent/lease.go`.
17. **Closing.** Anything that may block or do I/O has `Close(ctx) error`, idempotent and safe for concurrent use. `Close() error` is for purely local handles. See `mcp/middleware.go`.
18. **Logging** goes only through the injected `*slog.Logger`, with messages `"pkg: what happened"` and the error under `"err"`. Never use `log` or the global `slog`. See `agent/log.go`.
19. **Tests.** Standard library only. Failure messages read `Func(x) = %v, want %v`. Helpers call `t.Helper()`; use `t.TempDir()` and `t.Context()` (`context.Background()` only in cleanups and examples). Table tests use `tests` and `tt`; goroutines use `wg.Go`. Conformance kits for implementers live in `<pkg>test` packages (`backendtest`, `llmtest`), and the main entry points have `Example` tests. See `backend/backendtest/conformance.go`.
20. **Shared helpers instead of copies.** Before writing a small helper, look in `internal/` (`msgutil` for message predicates, `limitio` for bounded reads) and use `cmp.Or` for defaults. See `internal/limitio/limitio.go`.

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
