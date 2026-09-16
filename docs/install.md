# Installing pelican-k8s

This guide takes you from an empty cluster to a Panel that manages game
servers as Kubernetes resources.

## 1. Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes ≥ 1.33 | 1.35+ recommended. Uses native sidecars, `ValidatingAdmissionPolicy` and in-place pod resize (`pods/resize`) |
| StorageClass with `allowVolumeExpansion: true` | One RWO PVC per server; Panel disk changes expand it online |
| Ingress controller or OpenShift Router | For the gateway's HTTP API (Panel calls, websockets, signed uploads/downloads) |
| A way to expose TCP | SFTP (`NodePort`/`LoadBalancer`) and game ports (`LoadBalancer`, `NodePort` on single nodes, or `HostPort`) |
| A Pelican Panel | Any deployment; the `pelican-panel` chart in this repo is one option ([panel.md](panel.md)) |
| Optional: CSI snapshots | `VolumeSnapshotClass` for `SnapshotThenDelete` and scheduled snapshots |

Images are published to `ghcr.io/claiyc/pelican-k8s/{shim,agent,gateway,operator}`.

## 2. Create the node in the Panel

Admin → Nodes → Create:

| Field | Value |
|---|---|
| Name | anything |
| FQDN | `wings.example.com` (the gateway's Ingress/Route host) |
| Communicate over SSL | yes |
| Behind proxy | yes |
| Daemon port | `8080` |
| Daemon connect port | `443` |
| SFTP port | `30022` (NodePort default) or your LoadBalancer port |
| SFTP alias | address users should connect to (node IP or LB DNS name) |
| Memory / disk / CPU | the capacity you want the Panel to schedule against |

The `Configuration` tab shows `token_id` and `token`. The `remote` URL there is
the Panel URL the gateway must reach.

The Panel connects to the gateway through the same public address that
browsers use (`https://wings.example.com`), so it must be resolvable from the
Panel pod and its TLS certificate must be trusted there. For private CAs
(OpenShift's router CA, internal PKI) mount the CA into the Panel and pass it
to the gateway (see `gateway.extraCA` and the OpenShift section).

## 3. Install the chart

```bash
helm install pelican-k8s charts/pelican-k8s -n pelican-system --create-namespace \
  --set gateway.panelURL=https://panel.example.com \
  --set gateway.nodeTokenID=<token_id> \
  --set gateway.nodeToken=<token> \
  --set gateway.ingress.enabled=true \
  --set gateway.ingress.host=wings.example.com \
  --set gateway.ingress.className=nginx \
  --set defaultClass.spec.storage.storageClassName=standard
```

Prefer a values file for real installs; the chart README lists every value.
Put the node token in a Secret (`gateway.existingSecret`) if the values file is
committed anywhere.

What the chart creates:

- namespace `pelican-servers` (Pod Security `baseline`, default-deny NetworkPolicy)
- CRDs `gameservers.pelican-k8s.io` and `gameserverclasses.pelican-k8s.io`
- gateway and operator Deployments with scoped RBAC
- ConfigMap `pelican-agent-config` (the agent's Wings `config.yml`)
- `GameServerClass/default`
- `ValidatingAdmissionPolicy` objects enforcing the restricted shape of game pods and the volume allow-list of install Jobs

### Ingress requirements

The Panel and browsers hold long connections through the gateway:

- request/read timeout of at least **16 minutes** (Panel compress/decompress calls wait up to 15)
- websocket support with long idle timeouts (console sessions last hours)
- request bodies up to the upload limit (100 MiB by default)

ingress-nginx example annotations:

```yaml
gateway:
  ingress:
    annotations:
      nginx.ingress.kubernetes.io/proxy-read-timeout: "1200"
      nginx.ingress.kubernetes.io/proxy-send-timeout: "1200"
      nginx.ingress.kubernetes.io/proxy-body-size: 100m
```

OpenShift Routes: `gateway.route.enabled=true` sets `haproxy.router.openshift.io/timeout: 16m`.

### SFTP

SFTP is plain TCP and cannot go through an HTTP ingress. The chart creates a
second Service for it:

| `gateway.sftp.service.type` | Panel node fields |
|---|---|
| `NodePort` (default, port 30022) | SFTP port `30022`, alias = a node address |
| `LoadBalancer` | SFTP port `2022`, alias = the LB address |

### Game port exposure

Set in the class (`defaultClass.spec.exposure.mode`):

| Mode | Use when | Panel allocations |
|---|---|---|
| `LoadBalancer` (default) | any cluster with a LB implementation (cloud, MetalLB, kube-vip) | IP = LB pool IP, any port. `loadBalancer.ipAnnotation` pins the IP (e.g. `metallb.io/loadBalancerIPs`), `sharingAnnotation` lets servers share one IP |
| `NodePort` | single-node clusters | IP = node IP, ports **must be in the NodePort range** (30000–32767 by default) |
| `HostPort` | single node, ports outside the NodePort range | IP = node IP; needs `serversNamespace.podSecurityLevel=privileged` |

`/api/system/ips` (the Panel's allocation IP dropdown) returns
`gateway.externalIPs`, else the class `exposure.externalIPs`, else node addresses.

## 4. Verify

```bash
kubectl -n pelican-system get pods
kubectl -n pelican-system logs deploy/pelican-k8s-gateway | head
curl -s -H "Authorization: Bearer <token>" https://wings.example.com/api/system
```

In the Panel, the node page should show the system information (version 1.0.0,
os `linux`). Add allocations to the node, create a server and watch:

```bash
kubectl -n pelican-servers get gameservers -w
kubectl -n pelican-servers get pods,pvc,svc,jobs
kubectl -n pelican-servers describe gameserver gs-<uuid>
```

The install runs as a Job (`gs-<uuid>-install-<n>`); its output streams to the
Panel console and to `logs/install/<uuid>.log` on the server volume.

## 5. OpenShift

Set `openshift.enabled=true`. This binds `restricted-v2` to the game
ServiceAccounts and `anyuid` to the installer ServiceAccount, makes the operator
pick the namespace's UID range (`openshift.io/sa.scc.uid-range`) as the pinned
UID, and drops the seccomp profile from install pods (the `anyuid` SCC rejects
it). Use `gateway.route.enabled=true` for the API.

Router CA: OpenShift's default router certificate is signed by a cluster CA the
Panel and the gateway do not trust out of the box.

```bash
oc get secret router-ca -n openshift-ingress-operator -o jsonpath='{.data.tls\.crt}' | base64 -d > router-ca.crt
oc create configmap router-ca -n pelican-system --from-file=ca.crt=router-ca.crt
# gateway: --set gateway.extraCA.configMap=router-ca
# Panel: mount a merged bundle over /etc/ssl/certs/ca-certificates.crt (see panel.md)
```

OpenShift's CoreDNS answers on pod port 5353 behind the `:53` service; the
generated NetworkPolicies allow both.

## 6. Uninstall

`helm uninstall` removes the gateway, operator and policies but keeps the
servers namespace, the CRDs and every `GameServer` (and therefore the volumes).
Delete the servers in the Panel first if you want the data gone; the class
`storage.deletionPolicy` decides whether volumes are deleted, retained or
snapshotted.
