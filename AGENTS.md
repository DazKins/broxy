# Repository Guidelines

## Project Structure & Module Organization
`cmd/broxy` contains the CLI entrypoint. Core application code lives under `internal/`: `app` wires commands, `httpapi` serves the OpenAI-compatible API, `awsbedrock` handles upstream calls, `db` manages SQLite storage, `config` resolves paths and config, `service` manages user services, and `ui` embeds the admin UI. Static UI assets are committed under `internal/ui/dist/`; there is currently no frontend source tree checked in under `web/`. CI and release automation live in `.github/workflows/`, and the installer script is `scripts/install.sh`.

## Build, Test, and Development Commands
Use the standard Go toolchain from `go.mod`.

- `go build ./...` builds all packages.
- `go build -o ./broxy ./cmd/broxy` builds the local executable used in README examples.
- `go test ./...` runs the full test suite.
- `GOOS=linux GOARCH=amd64 go build ./cmd/broxy` smoke-tests a release target; CI also builds `linux`/`darwin` for `amd64` and `arm64`.
- `goreleaser check` validates release config.
- `goreleaser release --snapshot --clean` creates local release artifacts in `dist/`.

## Coding Style & Naming Conventions
Follow standard Go conventions: tabs for indentation, `gofmt` formatting, short package names, and exported identifiers only when a symbol must cross package boundaries. Keep packages focused on one concern and prefer small helpers over large multipurpose files. Name tests `TestXxx`, keep black-box API tests near the package they cover, and use descriptive fixture names such as `fakeProvider` or `recordingProvider`.

## Testing Guidelines
Add `_test.go` files beside the code they exercise. The existing suite uses Go’s built-in `testing` package with temp directories and in-memory HTTP handlers rather than external services. Cover new CLI branches, HTTP handlers, and persistence behavior with deterministic tests. There is no enforced coverage threshold, but `go test ./...` must pass before opening a PR.

## Commit & Pull Request Guidelines
Recent history favors short, imperative subjects such as `fix install script`, `add responses api compatibility`, and occasional scoped prefixes like `[Claw] Add per-API-key monthly usage limits and tracking`. Keep commit messages specific to one change. PRs should explain the behavior change, list validation performed, link related issues, and include screenshots when `internal/ui/dist/` changes affect the admin UI.

## Release Process
The repository default branch is `main` (`origin/HEAD -> origin/main`); treat requests to commit or release from `master` as targeting the default branch unless a real `master` branch is created later. Releases are tag driven. Pushing a `v*` tag triggers `.github/workflows/release.yml`, which runs GoReleaser and publishes the GitHub release.

Before releasing, make sure the tree is clean and current:

- `git status -sb`
- `git fetch origin --tags --prune`
- `git pull --ff-only origin main`

Validate the exact commit that will be tagged:

- `go test ./...`
- `goreleaser check`

To publish a release, commit the intended changes to `main`, then create and push an annotated semver tag:

- `git tag --list v0.4.0` to confirm the tag does not already exist.
- `git tag -a v0.4.0 -m "v0.4.0"` to create the release tag.
- `git push origin main` to publish the commit.
- `git push origin v0.4.0` to start the release workflow.

Do not reuse or move an existing release tag unless explicitly asked to repair a failed release. The binary version is injected by GoReleaser through `.goreleaser.yml` using `-X github.com/personal/broxy/internal/app.Version={{ .Version }}`, so normal releases do not require editing a version constant. After pushing the tag, verify the release appears at `https://github.com/DazKins/broxy/releases/tag/vX.Y.Z` with assets for darwin/linux amd64/arm64 and `checksums.txt`.

## Security & Configuration Tips
Do not commit real AWS credentials, generated config files, SQLite databases, or local logs. Use `broxy config path` for local testing; Broxy-owned config, pricing, database, and log files are expected under `~/.broxy/`. For network-dependent tooling such as GitHub release downloads or AWS calls, respect the existing proxy environment when working behind restricted networks.
