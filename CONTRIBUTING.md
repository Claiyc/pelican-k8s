# Contributing

Thanks for helping. This project has a small surface but a lot of moving parts
(a Kubernetes operator, a Panel-facing gateway, Wings embedded as a library
and a PID 1 shim), so a few conventions keep it manageable.

## Before you start

- Read [ARCHITECTURE.md](ARCHITECTURE.md). It describes how the system works
  and what it does not do.
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
  `make lint` runs the same two lint stages as CI: the whole tree on the
  baseline linters, your changes on all of them.
- Codecov, SonarQube Cloud and CodeRabbit comment on the pull request. They
  are advisory: read them, fix what is right, say so when it is not.
- Do not modify vendored upstream behaviour: Wings changes go to the fork
  branch as opt-in hooks (see docs/development.md) and are proposed upstream.
- Update the docs in the same PR when behaviour or values change. The docs
  describe the system as it is on master: no history, no "previously", no
  deviations from another document. ARCHITECTURE.md is changed to match the
  code, never annotated elsewhere. History belongs in commits and the
  changelog.

## Commit messages

Imperative subject line, a body that explains the reasoning when it is not
obvious. No trailer lines are required.
