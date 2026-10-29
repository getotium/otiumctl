# otiumctl

The command-line client for [Otium](https://getotium.ai): batch AI inference that waits for the
cheapest compute inside your deadline. Sign in, manage API keys, publish and test workflows, and
follow runs from a terminal, a script or CI.

`otiumctl` is a client of the documented Otium API and nothing else. Every command calls the API
with your credential; nothing here talks to anything else.

## Install

Download the archive for your platform from the
[latest release](https://github.com/getotium/otiumctl/releases/latest), check it against
`checksums.txt`, and put `otiumctl` on your `PATH`.

Or, with Go installed:

```sh
go install github.com/getotium/otiumctl@latest
```

Check it works:

```sh
otiumctl version
```

## Sign in

```sh
otiumctl auth login --hostname api.getotium.ai
```

This shows a one-time code and opens the Otium portal, where you approve the sign-in. The token
is kept in your system keychain (macOS Keychain, Windows Credential Manager, or the Secret Service
on Linux) and renews itself.

For CI and other unattended use, set an API key instead:

```sh
export OTIUM_TOKEN=sk-otium-...
```

## Common commands

```sh
otiumctl api-keys create --name ci          # prints the secret once
otiumctl workflow validate enrich.yaml      # compile as a publish would; stores nothing
otiumctl workflow test .                    # run *.test.yaml suites on the real engine
otiumctl workflow apply -f ./workflows/     # publish what changed
otiumctl run-set get <run-set-id>           # counts, outcome, what is held and why
```

`otiumctl` with no arguments lists every command. The
[workflow docs](https://getotium.ai/docs/workflows) explain the documents these commands work
with.

`workflow validate` and `workflow test` exit non-zero when a document would be rejected or a test
fails, so they drop into a pre-commit hook or a CI step without parsing output.

## Build and test

```sh
go test ./...
task check     # gofmt, vet, golangci-lint and the race-enabled tests, as CI runs them
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
