# Security

## Credentials

| Credential | Held by | Never in |
|---|---|---|
| Node daemon token (`token_id.token`) | Gateway (Secret in the system namespace) | agents, game pods, install Jobs, operator |
| Per-agent Wings token (`gs-<uuid>-agent`) | agent container env; read by gateway and operator | game container, install Jobs, CR |
| Gateway SSH host key (`pelican-gateway-sftp-hostkey`) | gateway | pods |
| Egg variables (`gs-<uuid>-env`) | gateway (served to the agent), install Job `envFrom`, game process env | CR spec, ConfigMaps |
| Panel S3 credentials | Panel | agents (presigned URLs only) |

The node token signs every browser JWT for every server on the node. The
gateway verifies those JWTs and re-signs the same claims with the agent token
of the target server, so a compromised agent can only mint tokens for its own
server, and Wings' own scope, expiry, one-time-use and denylist checks run in
the agent.

## Pod security

Game pods satisfy the `restricted` Pod Security Standard: non-root pinned UID,
`fsGroup`, all capabilities dropped, `RuntimeDefault` seccomp, read-only root
filesystem, no host namespaces, no service account token. Because install
Jobs need root in the same namespace, the namespace is labelled `baseline` and
the restricted shape of game pods is enforced by a `ValidatingAdmissionPolicy`
bound to their ServiceAccount; a second policy limits install pods to the
server volume, the script ConfigMap and emptyDirs.

On OpenShift the same split maps to SCCs (`restricted-v2` for game pods with
the namespace UID range, `anyuid` for the installer).

## Network

- The servers namespace has a default-deny policy for ingress and egress.
- Each server gets a policy allowing its allocation ports from anywhere, the
  agent ports from the gateway and operator only, DNS, internet egress minus the
  cluster, node and LAN ranges you configure in `network.blockedEgressCIDRs`,
  and the in-cluster allowances of the class.
- Install Jobs get DNS and internet egress only.
- Agent ↔ gateway and operator ↔ agent traffic is plain HTTP inside the cluster,
  protected by NetworkPolicy and bearer tokens.

## Blast radius

A compromised game process can read and write its own files, use the pod's
allowed egress and talk to the shim socket that controls only itself. It cannot
reach the Panel, other agents, the Kubernetes API or the node token.

A compromised agent holds its own Wings token: it can act as its own server
towards the Panel through the gateway's allow-list (state, activity, install
result, its pending backups) and mint browser tokens for its own server. It
cannot reach other servers' agents, the Panel directly, the Kubernetes API or
the node token.
