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

| Key | Default | Description |
|---|---|---|
| `image.registry` | `ghcr.io/claiyc/pelican-k8s` | Registry holding the `shim`, `agent`, `gateway` and `operator` images |
| `image.tag` | chart `appVersion` | Image tag for all four images |
| `image.pullPolicy` / `image.pullSecrets` | `IfNotPresent` / `[]` | Pull settings for the pelican-k8s images |
| `serversNamespace.name` | `pelican-servers` | Namespace holding `GameServer` objects and game pods |
| `serversNamespace.create` | `true` | Create the namespace (kept on uninstall) |
| `serversNamespace.podSecurityLevel` | `baseline` | Pod Security Admission level (`privileged` for HostPort exposure) |
| `gateway.panelURL` | required | Panel base URL, also the accepted websocket `Origin` |
| `gateway.nodeTokenID` / `gateway.nodeToken` | required unless `existingSecret` | Node credentials from the Panel (`token_id` / `token`) |
| `gateway.existingSecret` | `""` | Secret with keys `token_id` and `token` instead of the values above |
| `gateway.advertisedVersion` | `1.0.0` | Wings version reported in `User-Agent` and `/api/system` |
| `gateway.allowedOrigins` | `[]` | Extra websocket origins |
| `gateway.externalIPs` | `[]` | Addresses returned by `/api/system/ips` (falls back to the class `exposure.externalIPs`, then node addresses) |
| `gateway.remoteURL` | `http://<release>-gateway.<ns>.svc:8081` | How agents reach the gateway |
| `gateway.resyncInterval` | `15m` | Panel/cluster drift check |
| `gateway.uploadLimitMiB` | `100` | Browser upload limit (match the agent and ingress body limits) |
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
| `agent.*` | see values | Rendered into the agent's Wings `config.yml` (crash detection, SFTP read-only, log count, upload limit, timezone); `agent.extra` is merged verbatim |
| `defaultClass.create` / `.name` / `.spec` | `true` / `default` | The default `GameServerClass`; every `spec` field is documented in `docs/classes.md` |
| `admissionPolicies.enabled` | `true` | `ValidatingAdmissionPolicy` for game pods and install Jobs |
| `networkPolicies.enabled` | `true` | Default deny in the servers namespace plus gateway policies |
| `openshift.enabled` | `false` | SCC bindings, namespace UID ranges and seccomp handling for OpenShift |
| `openshift.gameSCC` / `openshift.installerSCC` | `restricted-v2` / `anyuid` | SCCs bound to the game and installer ServiceAccounts |

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
