# otiumctl — AI assistant context

Otium's open-source command-line client (Apache-2.0). One `package main` at the repo root.

- **It is a client of the documented Otium API and nothing else.** Every command calls the API
  with the user's credential. It embeds no compiler or engine: `workflow validate` and
  `workflow test` call `POST /v1/workflows/validate` and `POST /v1/workflows/test`. Its types
  mirror the API's JSON and hold only what the CLI reads; a field it does not know is passed
  through on `--json` output where the command prints a whole response.
- **Credentials:** `auth login` runs the OAuth device grant and keeps the token in the system
  keychain (`hosts.go`), renewing it before it lapses and once after a 401 (`client.go`). CI uses
  an API key in `OTIUM_TOKEN`. Never log a token or print one except where a command exists to
  (`auth token`, `api-keys create`).
- **Tests run against fakes of the API** (`newFakeAPI` in `assets_test.go`, the device-grant fake
  in `auth_test.go`), with canned answers in the API's own shapes. No network, no keychain.
- **Exit status is the contract for CI:** a rejected document or a failing test exits non-zero.
- Release: tag `vX.Y.Z` on main; `.github/workflows/release.yaml` runs GoReleaser
  (`.goreleaser.yaml`), which sets `main.version`.

Commands: `task check` (the CI gate), `task test`, `task build`, `task release:snapshot`.
