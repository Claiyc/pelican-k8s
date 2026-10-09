# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and the project uses
[Semantic Versioning](https://semver.org/) once tagged.

## [Unreleased]

### Changed
- BREAKING: every server runs in two pods. The agent pod `gs-<uuid>-agent-0` (StatefulSet `gs-<uuid>-agent`) serves files, SFTP, the console and backups for as long as the server exists. The game pod `gs-<uuid>-0` (StatefulSet `gs-<uuid>`) runs only while the server is on, so a stopped server requests no game resources. `kubectl logs gs-<uuid>-0 -c agent` becomes `kubectl logs gs-<uuid>-agent-0`; `kubectl logs gs-<uuid>-0` still shows the console.
- BREAKING: the shim reaches the agent over TCP, on port 8082 of the headless Service `gs-<uuid>-agent`, and stops the process with the egg's stop configuration on SIGTERM. Each pod has its own NetworkPolicy: `gs-<uuid>` for the game pod and `gs-<uuid>-agent` for the agent pod.
- The scheduler places the game pod and the agent follows it. The game pod prefers the agent's node (class `scheduling.preferAgentNode`, default `true`) and requires it while the agent has in-flight work. An agent pod on another node is moved to the game pod's node once it is idle (`AgentRelocating`). Archives of the local backup adapter live on the agent pod's scratch volume and are lost when it moves.
- Agent pods run under their own ServiceAccount `pelican-agent` (class `agentServiceAccountName`) and PriorityClass `pelican-agent` (class `agentPriorityClassName`, chart `agentPriorityClass`), which ranks above game pods.
- A process that exits without Wings restarting it leaves the server stopped: after a minute offline the gateway sets `desired: Stopped` and the game pod goes, as the Panel shows it.
- The chart runs the gateway and the operator with two replicas each by default (`gateway.replicas`, `operator.replicas`), spread across nodes (`podAntiAffinity: soft`) and covered by a PodDisruptionBudget with `maxUnavailable: 1` (`gateway.podDisruptionBudget`, `operator.podDisruptionBudget`). Gateway replicas are active/active; operator replicas elect a leader. On a single-node cluster set `podDisruptionBudget.enabled: false` for both, or a node drain waits for the budget.
- BREAKING: both charts ship a `values.schema.json`, and Helm rejects values that do not match it, including misspelt or unknown keys in the objects the chart models.
- New conditions `GamePodReady` and `AgentRelocating`; `status.game` records the game pod and its node, `status.agent.node` the agent's.

### Fixed
- A restart from the Panel no longer records the server as stopped. The gateway took the restart's own `stopping` → `offline` for a stop typed into the console and set `desired: Stopped`, so after a node drain or pod recreation the server stayed off.
- A process that reached `running` moments after `starting` could stay at `starting` in `status.process`, because the agent's two state posts arrived in the wrong order. The gateway now records the state the agent reports on a fresh poll, and writes it only if the GameServer did not change since before the poll, so with several gateway replicas an older poll cannot overwrite a newer one. When that poll fails it records the posted state and polls the agent again until it answers.

### Added
- Open consoles survive an agent pod replacement: the gateway holds the browser's websocket for up to `gateway.agentWait` (default `120s`), moves it to the new agent and asks the Panel for a fresh token. File-manager and other HTTP calls wait up to 10 s for the new agent.
- `tls.enabled` (default `false`) encrypts the traffic between the gateway, the operator, the agents and the shims with TLS 1.3 from an internal CA the operator keeps in Secret `<release>-ca` (#76). The gateway and the operator present client certificates to agents in addition to the agent token, the agent's remote API calls verify the gateway, and the shim verifies its agent. The operator issues and renews the certificates (`gs-<uuid>-tls`, `<release>-gateway-tls`); the components reload them without a restart. `tls.ca.rotation.enabled` (default `false`) replaces the internal CA automatically before it expires, without restarts, and `tls.certManager.enabled` (default `false`) issues every certificate through cert-manager instead (a configured issuer or a chart-created self-signed CA). Agents accept only the gateway's and the operator's client certificates.
- The *Placement* workflow (`hack/e2e-placement.sh`, `test/placement`) runs the placement scenarios on a kind cluster with three workers.

## [1.1.0] - 2026-10-06

### What's Changed
* ci: apply the Trivy severity filter to the SARIF upload by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/60
* ci: report coverage and code quality by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/59
* ci: open the release PR as a draft by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/63
* readme: shorten the introduction and the quick start by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/61
* security: fix the SonarQube findings that are real by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/64
* sonar: leave the main packages out of the coverage figure by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/68
* sonar: raise the reliability rating by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/67
* gateway: split wsproxy session.run into pumps and handleAuth by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/66
* operator: split reconcilePod into phases by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/65
* gateway: unit-test the Panel API, Remote API, sync, proxies and startup by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/69
* operator, agent, shim: unit-test the controller, clients and Wings glue by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/70
* ci: leave gocognit out of the SonarQube report by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/72
* ci: run nothing on the release PR while it is a draft by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/71
* gateway: stop doubling the jwt: prefix on token errors by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/73
* ci: let a minor or major label on the release PR itself raise the version by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/74

## [1.0.2] - 2026-10-06

### Fixed
- The contract suite (`hack/e2e-kind.sh`) creates its Paper server again (#50). The Paper egg, imported from `pelican-eggs/minecraft` `main`, gained a required `USER_AGENT` variable for the PaperMC downloads API, so the Panel's variable validation rejected the server before it ever reached the gateway, and the nightly run reported it as Panel drift. The suite now imports the egg at a pinned commit (`EGG_URL` still overrides it) and passes `USER_AGENT` (`EGG_USER_AGENT`). A failing `tinker` call now prints the Panel's exception instead of exiting silently.

### What's Changed
* contract: pin the Paper egg and pass its new USER_AGENT variable by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/51
* docs: add a commits-since-latest-release badge to the README by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/52
* ci: release by merging a generated release PR by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/53
* ci: pick the release version from minor/major PR labels by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/55
* build(deps): bump sigs.k8s.io/controller-runtime from 0.25.1 to 0.25.2 in the kubernetes group by @dependabot[bot] in https://github.com/Claiyc/pelican-k8s/pull/56
* deps: bump Wings to upstream 9fb682f (GHSA-8m75-v66f-m43j) by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/57
* Describe the current system in the docs; fix the documented limitations by @Claiyc in https://github.com/Claiyc/pelican-k8s/pull/58

## [1.0.1] - 2026-10-02

Republishes 1.0.0 with a tree that matches its tag. The images and the OCI chart of 1.0.0 were correct; anything installed from a checkout or the `v1.0.0` git tag was not.

### Fixed
- A chart installed from a checkout or a git tag now runs the images of its own release. `v1.0.0` was tagged with `charts/pelican-k8s/Chart.yaml` still at `0.1.1`. The OCI chart is versioned from the tag by the release workflow and was right, but the Argo CD example (`path: charts/pelican-k8s` at the `v1.0.0` tag) and `helm install charts/pelican-k8s` take `image.tag` from the chart's `appVersion`, so they deployed the 0.1.1 images under a 1.0.0 label. The release workflow now verifies before pushing anything that the tag matches the chart's `version` and `appVersion` and that CHANGELOG.md has a section for it, and `test/docs` checks the same invariants, plus the chart versions stated in the chart READMEs and the Panel guide, on every pull request.
- The release notes carry the changelog again. The release workflow takes them from the CHANGELOG section of the tag, and 1.0.0 had none, so its notes read only "See CHANGELOG.md."; the 1.0.0 section below has been written up after the fact.
- The `pelican-panel` chart is `0.1.2`, unchanged in content. The release workflow pushes it on every tag at the version in its `Chart.yaml`, and 1.0.0 pushed `0.1.1` again over the published artifact.
- The install commands in README.md and docs/install.md, and the Argo CD example, pin this release; the `pelican-panel` README stated chart version `0.1.0`; ARCHITECTURE.md still credited Renovate with keeping Wings and the Panel current.

## [1.0.0] - 2026-10-01

### Changed
- A `LoadBalancer` address that never arrives is explained. `ExposureReady` said "waiting for the LoadBalancer address" indefinitely, which is honest during normal provisioning and useless on a cluster with no load balancer implementation at all (kind, bare kubeadm, single-node OpenShift), where nothing will ever assign one. After a two-minute grace period the condition names that likely cause and the ways out: install an implementation such as MetalLB, or move the class to `NodePort` (allocation ports in the NodePort range) or `HostPort`. docs/install.md now asks for the exposure mode before the first server is created, since nearly every game's default port is below the NodePort range, and docs/operations.md lists the condition.

### Fixed
- The operator no longer deletes and recreates the agent Service on every reconcile. The API server stores a Service rendered without a `type` as `ClusterIP`, and `applyService` read that default as a type change, which it handles by deleting the Service. The agent Service now renders its type explicitly, and the comparison applies the default on both sides so any Service rendered without a type is patched in place.
- The CodeQL steps are bumped together. Every step of the CodeQL Action reads the configuration the `init` step wrote and refuses one from another release (`Loaded a configuration file for version '4.38.0', but running version '4.38.1'`), but Dependabot treats `github/codeql-action/init`, `/analyze` and `/upload-sarif` as three dependencies and opens a pull request for each, so a release arrives as up to three changes that each fail on their own and only pass once all of them have landed. `init` and `analyze` are back in step, and a Dependabot group keeps all of the paths in one pull request from now on.
- The nightly contract run no longer files cluster plumbing as Panel drift. The suite asked the Panel for the node's system information about a second after `kubectl rollout status` returned, and a Deployment that has rolled out is not yet an endpoint kube-proxy has programmed, so the Panel's very first call could be refused at the TCP layer. The assertion that the payload carries a `version` then aborted the script without printing anything, the diagnostics showed a healthy gateway and no pods, Services or endpoints, and the workflow opened an `upstream` issue telling the reader to compare the contract document against a Panel version the run had never spoken a word of protocol to. The suite now waits for the gateway to answer `/healthz` from inside the Panel pod - the same DNS and Service path the Panel's own HTTP client takes - before it asks the Panel anything, distinguishes an exception from a missing `version`, dumps the pods, Services and endpoints of both namespaces on failure, and records the step it died in so the nightly issue names it instead of assuming upstream.
- A crash restart that races the game container's own restart no longer fails with `broken pipe`. When the old shim died, its exit event could put the server offline before the agent had noticed the dead connection, so Wings' crash handler called `Start` on it. `Start` retried once on a connection error, but the dead client was not marked done until its reader saw EOF, so the retry could be handed the same connection and fail again. `Start` now closes the dead client and waits for it to be done before it waits for the new shim.

## [0.1.1] - 2026-09-20

### Added
- MetalLB becomes a turnkey `LoadBalancer` option. `exposure.loadBalancer.provider: metallb` supplies the `metallb.io/loadBalancerIPs` and `metallb.io/allow-shared-ip` keys, so a class no longer has to spell them out — a mistyped key is silently ignored by MetalLB and only surfaces as a Service that never gets an address. `gateway.metallb.discoverPools` makes `/api/system/ips` return the addresses of MetalLB's `IPAddressPool` objects, so the Panel's allocation form only offers addresses MetalLB can announce. Explicit `gateway.externalIPs` and the class `exposure.externalIPs` still win, the pools are read with an uncached client because the CRD may be absent, and a missing CRD or missing RBAC falls back to the previous sources. The chart grants `metallb.io/ipaddresspools` reads only when discovery is enabled. Because each Service is pinned to its allocation IP, the address a player sees in the Panel is by construction the address MetalLB announces.
- `resources.memoryRequestPercentOfLimit`, the memory counterpart of `cpuRequestPercentOfLimit`. It defaults to 100, which reserves the Panel's `memory_limit` for every server exactly as before: memory is incompressible, so a server that grows into its limit on a full node is evicted rather than throttled, and reserving the cap is what lets the scheduler prevent that. Lowering it overcommits deliberately. Install Jobs now take their memory request from the game container too, for the same reason they already took their CPU request from it — a Job reserving the full limit would undo the class's overcommit for as long as it runs.
- Contract suite against a real Pelican Panel (`hack/e2e-kind.sh`, workflow `Contract`): a kind cluster runs the Panel from `charts/pelican-panel` and pelican-k8s built from the commit, then drives node registration, server creation and install, power, console websocket, files, SFTP, backup, suspension and deletion through the Panel's own services. It runs on every pull request against the pinned Panel version and nightly against `ghcr.io/pelican/panel:latest`; a nightly failure opens an `upstream` issue.
- Native Go fuzzing of the three parsers on a trust boundary: the shim's JSON-lines protocol, the output ring buffer and the gateway's JWT verification. The seed corpora run with the unit tests; a CI job fuzzes each target for 40s.
- README badges for CodeQL and the OpenSSF Scorecard. The Go Reference badge was dropped: every `pkg.go.dev` page for the module 404s while the badge image is served unconditionally, and 29 of the 37 packages are `internal/`, so there is no reference worth linking.
- `test/supplychain`: guards the pinning and least-privilege invariants above, so a future change cannot quietly reopen the Scorecard findings.
- `test/docs`: the versions in the documented `helm install` commands and the Argo CD example must match the chart versions the tree releases, so a chart bump cannot leave a quick start that fails on its first command.
- `test/supplychain` also asserts that `docker/docker/daemon` and `x/crypto/openpgp` stay out of the build graph; the six outstanding advisories have no fixed version, so not linking them is what keeps them harmless.

### Changed
- The Argo CD example in `docs/install.md` no longer sets `ServerSideApply=true`. On OpenShift an application controller that starts before the `route.openshift.io` API is available keeps a schema without `Route`, and every server-side-applied Application containing a Route then fails to compare until the controller is restarted.
- Pod readiness now carries the game state: the game container is ready only while Wings reports `running` (what the Panel shows as running), so a stopped or starting server shows `1/2` instead of `2/2`. Readiness restarts nothing and gates nothing (no liveness probe on the game container, Services publish not-ready addresses). The pod template changes, so existing pods are marked `RecreatePending` and pick the probe up at their next stop or start.
- Build with Go 1.27. `govulncheck` in CI fails only for reachable vulnerabilities that have a fix.
- Dependency updates come from Dependabot; the Renovate configuration was removed.

### Security
- Every GitHub Action is pinned to a commit SHA and every container base image to a digest, closing the Scorecard *Pinned-Dependencies* finding. Dependabot keeps both current.
- Workflow tokens are scoped to the jobs that need them: `security-events: write` (CodeQL), `contents: write` and `packages: write` (release) and `issues: write` (upstream drift) are no longer granted workflow-wide, closing the Scorecard *Token-Permissions* finding.

### Fixed
- A NodePort the API server refuses is a terminal `PortOutOfRange` condition instead of an endless retry. An allocation port outside `--service-node-port-range` produced a helpful condition and then attempted the Service anyway; the failure overwrote that condition with the raw API error and was returned, so the reconciler retried on a backoff that could never succeed and the explanation never reached the user. The API server stays the authority on the range — the rejection is classified rather than pre-empted, so a widened range still works — and the server is reported as `Error` with no pod, because one no player can reach must not look healthy in the Panel.
- Removing a CPU or memory limit recreates the pod. Setting a server to unlimited in the Panel drops the container limit, which the resize subresource rejects unconditionally, and resources are excluded from the pod template hash because they are normally resizable — so the reconciler retried a permanently doomed resize and the server kept both its old limit and its old request indefinitely. The removal is now detected before the request is made and drives the existing recreate path, which still waits for the process to go offline.
- The shim kills processes that escaped the child's process group. A stop killed the process group, but a process that calls `setsid` leaves it — Wine's `wineserver` does exactly that, so the game it hosts survived every stop, kept its ports bound and held the `WINEPREFIX` lock, and the next start stacked another server against the same prefix until none of them made progress. Everything left in the PID namespace is killed once the supervised process exits, guarded on being PID 1 so the sweep cannot reach beyond a container.
- The shim keeps reaping orphaned processes while no game runs. It forwarded every reaped pid into a channel that is only drained while a process is supervised, so after 16 orphans (for example from `kubectl exec` into a stopped server) the reaper blocked and every later orphan stayed a zombie; a stale entry could also be mistaken for the exit of a later process that reused the pid. Only the supervised process is forwarded now.
- The drift resync takes each server's configuration from the per-server endpoint. The Panel's server list eager-loads the startup variables, which returns the egg default for every variable, so the resync never saw a variable change and could overwrite correct values with defaults.
- A start or restart now syncs the server configuration from the Panel first. The Panel sends no sync for a startup variable change because Wings fetches the live configuration on every boot, but the agent's boot sync is answered from `spec.panel`, so such a change only applied after the next drift resync (up to 15 minutes).
- The image vulnerability scan could not resolve `aquasecurity/trivy-action@0.36.0` (the tags carry a `v` prefix from 0.29 on), so no Trivy results reached code scanning.
- The nightly *Upstream drift* workflow was failing on both jobs without any upstream having drifted. The Wings job pointed the `replace` at `github.com/pelican/wings@main`, which cannot resolve: package `boot` is one of the four hooks and exists only on the fork's `pelican-k8s-hooks` branch, so `go mod tidy` always failed, `|| true` hid it, and the drift tests then aborted on an inconsistent `go.mod` instead of running. The job now checks the tip of the hooks branch — the code the agent is actually built from — tidies without masking errors, warns when the pinned `replace` is behind that tip, and builds and tests against it instead of expecting the build to fail.
- The Panel job grepped the image for the literal string `servers/{uuid}/container/status`, which upstream no longer spells out anywhere since the daemon routes moved into `Route::prefix('/servers/{server:uuid}')->group(...)`; the endpoint itself is unchanged. The three greps are replaced by `hack/check-panel-contract.sh`, which resolves the prefix groups and diffs the whole remote route table, the `Http::daemon` bearer token and base URL, and the `Pelican Wings` response User-Agent check against docs/wings-panel-contract.md. It runs locally too (`PANEL_IMAGE=`, or `PANEL_SRC_DIR=` for a checkout without Docker).

## [0.1.0] - 2026-09-16

First release. Implements ARCHITECTURE.md end to end and was verified on an
OpenShift 4.22 / Kubernetes 1.35 cluster against Pelican Panel v1.0.0-beta38
with the Paper and Vanilla Minecraft eggs.

### Added
- Shim: PTY process supervisor as PID 1 of game containers with a JSON-lines socket protocol, output ring buffer, cgroup v2 stats, orphan reaping and OOM detection; `prepare`, `probe` and `install-run` helpers for init containers and install Jobs.
- Agent: Wings embedded as a library for one server (stock router, websocket, SFTP, filesystem, parser, crash detection, backups, activity) with a shim-backed process environment and a Job-backed installer.
- Operator: `GameServer` reconciler creating the PVC, Services, NetworkPolicy, StatefulSet and install Jobs, driving the agent (power, sync, install), in-place resize, deferred pod recreation, OOM relay, node-loss fencing, scheduled snapshots and Delete/Retain/SnapshotThenDelete finalizers.
- Gateway: Wings node API towards the Panel, agent-facing remote API, JWT re-signing for websockets and signed URLs, websocket proxy, SFTP relay, drift resync.
- Helm charts `pelican-k8s` and `pelican-panel`; CI (build, tests, lint, chart lint, multi-arch images), release and upstream-drift workflows; Renovate.
- Test suites: unit, spike (Paper egg through the agent and shim in Docker), e2e against a cluster, upstream compatibility diffs.

### Known limitations
- Server transfers, Panel mounts, `force_outgoing_ip`, swap, `io_weight`, CPU pinning and disabling the OOM killer are not supported (see docs/compatibility.md).
- Local (`wings` adapter) backups live on the pod's scratch volume and do not survive pod recreation; use the S3 adapter.
- Wings is pinned to a fork branch carrying four opt-in hooks until they are merged upstream.

[Unreleased]: https://github.com/Claiyc/pelican-k8s/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/Claiyc/pelican-k8s/compare/v1.0.2...v1.1.0
[1.0.2]: https://github.com/Claiyc/pelican-k8s/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/Claiyc/pelican-k8s/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/Claiyc/pelican-k8s/compare/v0.1.1...v1.0.0
[0.1.1]: https://github.com/Claiyc/pelican-k8s/releases/tag/v0.1.1
[0.1.0]: https://github.com/Claiyc/pelican-k8s/releases/tag/v0.1.0
