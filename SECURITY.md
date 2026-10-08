# Security policy

## Reporting a vulnerability

Please do not open public issues for security problems. Use GitHub's private
vulnerability reporting on this repository ("Report a vulnerability" under the
Security tab). You will get an acknowledgement within a few days.

## Scope

The components in this repository: shim, agent, gateway, operator and the Helm
charts. Issues in Pelican Panel or Wings themselves belong to
[pelican/panel](https://github.com/pelican/panel) and
[pelican/wings](https://github.com/pelican/wings). The agent embeds Wings from
the `pelican-k8s-hooks` branch of [Claiyc/wings](https://github.com/Claiyc/wings),
which adds opt-in hooks to upstream Wings; fixes there flow in when that branch
is brought up to date and the pinned version is bumped.

## Design notes

The trust boundaries (node token, per-agent tokens, egg variables, what a
compromised game process or agent can reach) are described in
[docs/security.md](docs/security.md) and ARCHITECTURE.md section 12.
