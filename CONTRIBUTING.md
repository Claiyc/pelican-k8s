# Contributing

Thanks for helping. This project has a small surface but a lot of moving parts
(a Kubernetes operator, a Panel-facing gateway, Wings embedded as a library
and a PID 1 shim), so a few conventions keep it manageable.

## Before you start

- Read [ARCHITECTURE.md](ARCHITECTURE.md). Most design questions are answered
  there, including what is deliberately out of scope.
- For anything touching the Panel or Wings protocol, check
  [docs/wings-panel-contract.md](docs/wings-panel-contract.md) first; the
  gateway must keep behaving like Wings.
- Open an issue for larger changes so the approach can be discussed before
  you invest time.

## Development

See [docs/development.md](docs/development.md) for the full workflow. In short:

```bash
make build           # binaries into ./bin
make test            # unit tests (fast, no cluster)
make generate        # after changing api/v1alpha1: deepcopy + CRDs (commit the result)
go test -tags spike ./test/spike     # agent + shim run Paper in Docker (needs Docker, internet)
go test -tags e2e   ./test/e2e       # against a deployed gateway (see docs/development.md)
```

Tests are mandatory for logic changes: rendering, reconcile rules, gateway
handlers and the shim all have unit tests to extend.

## Pull requests

- One topic per PR, with a description of what changed and why.
- `go vet`, `golangci-lint`, `helm lint` and the unit tests run in CI; keep them green.
- Do not modify vendored upstream behaviour: Wings changes go to the fork
  branch as opt-in hooks (see docs/development.md) and are proposed upstream.
- Update the docs when behaviour or values change.

## Commit messages

Imperative subject line, a body that explains the reasoning when it is not
obvious. No trailer lines are required.
