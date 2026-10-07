# Security

## Credentials

| Credential | Held by | Never in |
|---|---|---|
| Node daemon token (`token_id.token`) | Gateway (Secret in the system namespace) | agents, game pods, install Jobs, operator |
| Per-agent Wings token (`gs-<uuid>-agent`) | agent pod env; read by gateway and operator | game pod, install Jobs, CR |
| Shim token (`gs-<uuid>-shim`) | agent pod and shim env (the shim is non-dumpable) | game process, install Jobs, CR |
| Gateway SSH host key (`pelican-gateway-sftp-hostkey`) | gateway | pods |
| Egg variables (`gs-<uuid>-env`) | gateway (served to the agent), install Job `envFrom`, game process env | CR spec, ConfigMaps |
| Panel S3 credentials | Panel | agents (presigned URLs only) |

The node token signs every browser JWT for every server on the node. The
gateway verifies those JWTs and re-signs the same claims with the agent token
of the target server, so a compromised agent can only mint tokens for its own
server, and Wings' own scope, expiry, one-time-use and denylist checks run in
the agent.

## Pod security

Every server has an agent pod, which runs Wings and holds the server's tokens,
and, while it is on, a game pod, which runs the egg's code. The two share only
the server's volume; the game pod mounts just the server directory of it.

Agent pods and game pods satisfy the `restricted` Pod Security Standard (in
`HostPort` mode the game pod additionally uses host ports, which needs a
`privileged` namespace; everything else stays as below): non-root pinned UID,
`fsGroup`, all capabilities dropped, `RuntimeDefault` seccomp, read-only root
filesystem, no host namespaces, no service account token. Because install
Jobs need root in the same namespace, the namespace is labelled `baseline` and
the restricted shape of game and agent pods is enforced by a `ValidatingAdmissionPolicy`
bound to their ServiceAccounts; a second policy limits install pods to PVC,
ConfigMap and emptyDir volumes and forbids host ports, added capabilities and
privilege escalation.

On OpenShift the same split maps to SCCs (`restricted-v2` for game and agent
pods with the namespace UID range, `anyuid` for the installer).

## Network

- The servers namespace has a default-deny policy for ingress and egress.
- Each game pod gets a policy allowing its allocation ports from anywhere,
  DNS, the shim port of its own agent pod, egress to `0.0.0.0/0` except
  link-local and the ranges in `network.blockedEgressCIDRs` (by default the
  private and shared ranges `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` and
  `100.64.0.0/10`, which cover the usual pod, service, node and LAN ranges),
  and the in-cluster allowances of the class.
- Each agent pod gets a policy allowing its HTTP and SFTP ports from the
  gateway and operator only, its shim port from its own game pod only, DNS,
  the gateway's remote API port, the same internet egress and the class's
  additional in-cluster allowances.
- Install Jobs get DNS and the same internet egress, without the in-cluster
  allowances.
- With `tls.enabled: true` every hop between the components is TLS 1.3 from an
  internal CA the operator keeps: gateway and operator to the agent's HTTP API
  (with client certificates, on top of the agent token), the agent to the
  gateway's remote API, and the shim to the agent (around the shim-token
  handshake). SFTP between the gateway and the agent is SSH either way.
  Certificates are renewed by the operator and reloaded without restarts.
  `tls.ca.rotation.enabled` (default `false`) also replaces the CA before it
  expires, trusting the new one everywhere before it signs, so nothing
  restarts. `tls.certManager.enabled` (default `false`) has cert-manager issue
  every certificate instead, from `tls.certManager.issuerRef` (a ClusterIssuer)
  or a self-signed CA the chart creates; use an issuer dedicated to
  pelican-k8s, since agents accept any client certificate it signs for the
  names `pelican-gateway` and `pelican-operator`. cert-manager renewing that
  CA with a new key breaks trust until every leaf renews, so give it a long
  lifetime or distribute its bundle separately (trust-manager).
  Turning the value on or off recreates every agent pod and, through
  `RecreatePending`, running game pods. Details, names and rotation:
  ARCHITECTURE.md §12.6.
- With `tls.enabled: false` (the default) that traffic is plain HTTP and plain
  TCP inside the cluster, protected by NetworkPolicy, bearer tokens and the
  mutual shim handshake. NetworkPolicy limits who can connect, not who can
  read: someone who can capture pod traffic can read an agent token, which is
  scoped to one server, the server configuration with its egg variables, and
  console data. Turn TLS on, or use a CNI with transparent encryption, if the
  cluster network is not trusted. A CNI that encrypts between nodes does not
  protect against an observer on the node itself.

## Blast radius

A compromised game process can read and write its own files and use the game
pod's allowed egress: the internet, the shim port of its own agent, and what
the class allows inside the cluster. With `inClusterEgress.gameServers` (on by
default) that includes every port of other servers' game pods, and
`inClusterEgress.additional` adds the configured destinations; turn the first
off when servers must not reach each other. Every connection to the shim port
must answer a challenge keyed
with the shim token. The shim reads that token from its environment after
making itself non-dumpable and removes it from the game's environment, so the
game process cannot pose as the shim: process state, stats and exit codes come
from the shim. It cannot reach the gateway, the operator, its agent's Wings API
or SFTP server, other servers' agents, the Panel, the Kubernetes API, the agent
token or
the node token, and the agent's part of the volume (activity, logs, install
state) is not mounted in its pod.

A compromised agent holds its own Wings token and the shim token: it controls
its own game process and can act as its own server
towards the Panel through the gateway's allow-list (state, activity, install
result, its pending backups) and mint browser tokens for its own server. It
cannot reach other servers' agents, the Panel directly, the Kubernetes API or
the node token.
