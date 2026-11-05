# AGENTS.md

## Repository Overview
Go 1.26+ service. Module path: `github.com/your-org/your-service`.
Production code under `internal/`. Public packages under `pkg/`.
`cmd/` holds only `main.go` entrypoints with minimal logic.

## Build Commands
```bash
go build ./...
go fix ./...        # Apply Go 1.26 modernizers before any PR
```text

## Test Commands

```bash
# Unit tests (always run with race detector)
go test -race ./...

# With coverage
go test -race -coverprofile=coverage.out -covermode=atomic ./...

# Short mode (skip integration tests)
go test -short ./...

# Integration tests (require //go:build integration tag)
go test -race -tags=integration ./...
```text

## Lint Commands

```bash
go vet ./...
golangci-lint run ./...
gosec ./...
govulncheck ./...
```markdown

## Verification Sequence

Before any commit, run in order:

1. `go mod tidy`
2. `go fmt ./...`
3. `go fix ./...`
4. `go vet ./...`
5. `golangci-lint run ./...`
6. `go test -race ./...`

## Code Style

- Error messages: lowercase, no trailing punctuation
- Wrap with context: `fmt.Errorf("connecting to database: %w", err)`
- Use `%w` inside error chains; `%v` only at top-level boundaries
- Never discard errors with `_`
- Use `errors.Is()` / `errors.As()` for inspection
- Use `errors.AsType[T]()` (Go 1.26) for type-safe unwrapping in new code
- Log with `log/slog` using structured attributes, never `fmt.Println`

## Testing Standards

- Table-driven tests with `t.Run` and named `name` fields
- Mark independent tests `t.Parallel()`
- Tag integration tests: `//go:build integration` at the top of the file
- Use `require` (testify) for fatal assertions; `assert` for non-fatal
- Use `goleak.VerifyTestMain` in any package that spawns goroutines

## Concurrency Rules

- Every goroutine must have a defined exit strategy before it is spawned
- Only the sender closes a channel
- Always include `ctx.Done()` in select statements
- Never use `time.After` in loops — use `time.NewTimer` with `Reset`
- Use channels for ownership transfer; `sync.Mutex` for shared struct state

## Module Rules

- Run `go mod tidy` and commit both `go.mod` and `go.sum` after any import change
- Manage tool dependencies with `go get -tool` (Go 1.24+)
- Use `go.work` for local multi-module development; do not commit unless this is a monorepo

## Go 1.26 Specifics

- Use `new(expr)` for pointer initialisation when the value fits a single expression
- Use `errors.AsType[T]()` for type-safe error unwrapping in new code
- Run `go fix ./...` as part of the pre-PR workflow
- `go tool doc` is removed — use `go doc` directly

```text

For a high-trust subdirectory (e.g., a payments service), layer a subdirectory `AGENTS.md` override:

```markdown
# services/payments/AGENTS.md

## Security Requirements (override root)
All changes must pass `gosec -severity high ./services/payments/...`.
Treat medium-severity findings as blocking for new code.
Run `govulncheck ./services/payments/...` and fail on any known vulnerability.