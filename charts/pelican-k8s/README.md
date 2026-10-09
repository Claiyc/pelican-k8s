# pelican-k8s Helm chart

Installs the Kubernetes-native backend for [Pelican Panel](https://pelican.dev):
the **gateway** (what the Panel sees as a Wings node), the **operator**
(reconciles `GameServer` resources into pods, volumes, services and install
Jobs), the CRDs, RBAC, admission policies, network policies, the agent
configuration and a default `GameServerClass`.

See [`docs/install.md`](../../docs/install.md) for the full walkthrough and
[`docs/classes.md`](../../docs/classes.md) for the class reference.

## Quick start

```bash
helm install pelican-k8s charts/pelican-k8s -n pelican-system --create-namespace \
  --set gateway.panelURL=https://panel.example.com \
  --set gateway.nodeTokenID=<token_id from the Panel node> \
  --set gateway.nodeToken=<token from the Panel node> \
  --set gateway.ingress.enabled=true --set gateway.ingress.host=wings.example.com \
  --set defaultClass.spec.storage.storageClassName=<storage class with expansion>
```

CRDs live in `crds/` and are installed on first install only. On upgrades apply
them yourself: `kubectl apply -f charts/pelican-k8s/crds`.

## Values

`values.schema.json` describes every value with its type, allowed values and
default. Helm checks the merged values against it on `install`, `upgrade`,
`template` and `lint`, so a misspelt key, a wrong type or an unsupported value
fails before anything renders. Editors with YAML language support (VS Code
with the Red Hat YAML extension, JetBrains IDEs) complete and check a values
file against it when the file starts with
`# yaml-language-server: $schema=<path or URL of values.schema.json>`.

| Key | Default | Description |
|---|---|---|
| `image.registry` | `ghcr.io/claiyc/pelican-k8s` | Registry holding the `shim`, `agent`, `gateway` and `operator` images |
| `image.tag` | chart `appVersion` | Image tag for all four images |
| `image.pullPolicy` / `image.pullSecrets` | `IfNotPresent` / `[]` | Pull settings for the pelican-k8s images |
| `serversNamespace.name` | `pelican-servers` | Namespace holding `GameServer` objects, agent pods and game pods |
| `serversNamespace.create` | `true` | Create the namespace (kept on uninstall) |
| `serversNamespace.podSecurityLevel` | `baseline` | Pod Security Admission level (`privileged` for HostPort exposure) |
| `gateway.replicas` / `operator.replicas` | `2` / `2` | Replica counts. Gateway replicas are active/active; operator replicas elect one leader through a Lease, the other is a standby |
| `gateway.podAntiAffinity` / `operator.podAntiAffinity` | `soft` / `soft` | Spread replicas across nodes: `soft` prefers different nodes and still schedules on a single node, `hard` requires them (replicas beyond the node count stay Pending), `none` adds no rule |
| `gateway.affinity` / `operator.affinity` | `{}` | Pod affinity; when set it replaces the `podAntiAffinity` rule |
| `gateway.nodeSelector` / `.tolerations`, `operator.nodeSelector` / `.tolerations` | `{}` / `[]` | Node placement |
| `gateway.podDisruptionBudget.enabled` / `.maxUnavailable` (same for `operator`) | `true` / `1` | PodDisruptionBudget limiting voluntary disruptions; a drain evicts one replica at a time and waits for its replacement to run elsewhere; with one replica it is evicted at once. Set `enabled: false` on single-node clusters |
| `gateway.panelURL` | required | Panel base URL, also the accepted websocket `Origin` |
| `gateway.nodeTokenID` / `gateway.nodeToken` | required unless `existingSecret` | Node credentials from the Panel (`token_id` / `token`) |
| `gateway.existingSecret` | `""` | Secret with keys `token_id` and `token` instead of the values above |
| `gateway.advertisedVersion` | `1.0.0` | Wings version reported in `User-Agent` and `/api/system` |
| `gateway.allowedOrigins` | `[]` | Extra websocket origins |
| `gateway.externalIPs` | `[]` | Addresses returned by `/api/system/ips` (falls back to the class `exposure.externalIPs`, then the MetalLB pools with `gateway.metallb.discoverPools`, then node addresses) |
| `gateway.metallb.discoverPools` / `.poolNames` / `.maxAddresses` | `false` / `[]` / `256` | Offer MetalLB `IPAddressPool` addresses from `/api/system/ips` (empty `poolNames` = every pool) |
| `gateway.remoteURL` | `http://<release>-gateway.<ns>.svc:8081` (`https://` with `tls.enabled`) | How agents reach the gateway; with `tls.enabled` an `https` URL whose host goes on the gateway's certificate |
| `gateway.resyncInterval` | `15m` | Panel/cluster drift check |
| `gateway.agentWait` | `120s` | How long open consoles are held while a server's agent pod is replaced |
| `gateway.extraCA.configMap` / `.key` | `""` / `ca.crt` | ConfigMap with a PEM CA to trust for the Panel's TLS (private CAs, OpenShift router CA) |
| `gateway.sftp.service.type` / `.port` / `.nodePort` | `NodePort` / `2022` / `30022` | How users reach SFTP |
| `gateway.sftp.keyOnly` | `false` | Disable SFTP password logins |
| `gateway.sftp.hostKeySecret` | `pelican-gateway-sftp-hostkey` | Secret holding the generated SSH host key |
| `gateway.ingress.*` | disabled | Ingress for the Wings API (needs long timeouts, see docs) |
| `gateway.route.*` | disabled | OpenShift Route alternative (edge TLS, 16m timeout annotation) |
| `gateway.resources` / `operator.resources` | small | Pod resources |
| `gateway.logLevel` / `operator.logLevel` | `info` | `debug` for verbose logs |
| `operator.concurrency` | `4` | Max concurrent reconciles |
| `operator.leaderElect` | `true` | Leader election |
| `tls.enabled` | `false` | TLS between gateway, operator, agents and shims from an internal CA the operator keeps (Secret `<release>-ca`); switching it recreates every agent pod and running game pods |
| `tls.ca.lifetime` | `87600h` | Lifetime of the internal CA the operator creates |
| `tls.ca.rotation.enabled` / `.overlap` | `false` / `1h` | Replace the internal CA automatically in its last third of life; each step waits `overlap` for Secrets to reach every pod |
| `tls.certManager.enabled` | `false` | Issue every certificate through cert-manager instead of the internal CA (needs cert-manager) |
| `tls.certManager.issuerRef` | `{}` | The issuer, e.g. `{name: corp-ca, kind: ClusterIssuer}`; empty creates a self-signed CA and ClusterIssuer `<release>-ca` |
| `tls.certManager.caNamespace` / `.caDuration` | `cert-manager` / `87600h` | Namespace and lifetime of the chart-created CA (cert-manager's cluster resource namespace) |
| `agent.*` | see values | Rendered into the agent's Wings `config.yml` (crash detection, SFTP read-only, log count, upload limit, timezone); `agent.extra` is merged verbatim |
| `defaultClass.create` / `.name` / `.spec` | `true` / `default` | The default `GameServerClass`; every `spec` field is documented in `docs/classes.md` |
| `agentPriorityClass.create` / `.name` / `.value` | `true` / `pelican-agent` / `1000` | PriorityClass of agent pods (class `agentPriorityClassName`); must rank above game pods |
| `agentPriorityClass.preemptionPolicy` | `PreemptLowerPriority` | `Never` stops agent pods from evicting other pods on a full node; the agent waits for room instead |
| `admissionPolicies.enabled` | `true` | `ValidatingAdmissionPolicy` for game pods, agent pods and install Jobs |
| `networkPolicies.enabled` | `true` | Default deny and an install-Job egress policy in the servers namespace, plus the gateway's ingress policy |
| `openshift.enabled` | `false` | SCC bindings, namespace UID ranges and seccomp handling for OpenShift |
| `openshift.gameSCC` / `openshift.installerSCC` | `restricted-v2` / `anyuid` | SCCs bound to the game and agent ServiceAccounts, and to the installer ServiceAccount |

## Panel node settings

After the install, create a node in the Panel (Admin > Nodes) with:

| Field | Value |
|---|---|
| FQDN | the Ingress/Route host of the gateway |
| Scheme | `https` |
| Behind proxy | yes |
| Daemon port (listen) | `8080` |
| Daemon port (connect) | `443` |
| SFTP port | the SFTP NodePort/LoadBalancer port (`30022` by default) |
| SFTP alias | the node/LB address users connect to |

Copy the node's `token_id` and `token` into `gateway.nodeTokenID` / `gateway.nodeToken`
(or a Secret referenced by `gateway.existingSecret`).
