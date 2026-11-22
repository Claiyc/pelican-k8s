# Panel plugin plan

**Temporary document.** This file plans an optional Pelican Panel plugin for pelican-k8s. Nothing in
it is implemented yet. When a phase lands, its behaviour moves into [ARCHITECTURE.md](ARCHITECTURE.md)
and `docs/`, and the phase is marked done here.

## 1. Goal

pelican-k8s knows things the Panel cannot see: why a server is `Pending`, which node an allocation IP
pins a server to, whether a port is inside the NodePort range or already taken in the cluster, what
requests and limits a class turns Panel values into. Today an admin learns most of this from `kubectl`
after a server has already failed, and a user learns none of it.

The plugin shows this in the Panel and lets admins check allocations and classes before a server
depends on them.

Non-goals:

- changing the Panel (the plugin uses the Panel's plugin system and nothing else)
- making the Panel a Kubernetes console (no pod logs, no YAML, no arbitrary resource editing)
- editing cluster policy (`GameServerClass` objects stay owned by whoever applies them, see X-01)

## 2. Ground rules

- **R1 Optional.** Every pelican-k8s feature works without the plugin. The plugin never holds the only
  copy of something a server needs: state that affects servers lives in the cluster (the `GameServer`
  spec, a gateway ConfigMap) and is written through the gateway. Uninstalling the plugin removes UI,
  nothing else.
- **R2 No cluster credentials in the Panel.** The Panel is the internet-facing component. The plugin
  talks only to the gateway's extension API (§4.3), authenticated with the node's daemon token, the
  same token the Panel already uses for Wings calls. The Panel pod gets no ServiceAccount rights.
- **R3 The gateway decides, the plugin displays.** Allocation rules, placement and resource
  calculations run in Go next to the operator code that enforces them, so the plugin and the operator
  cannot disagree. The plugin maps reason codes to text and renders.
- **R4 Documented hooks only.** The plugin adds pages, widgets and actions through the plugin system's
  documented extension points. It does not change the Panel's own forms, tables or dialogs (X-03,
  X-05, X-06, X-07).
- **R5 Read-mostly.** The plugin writes only through the endpoints marked as writes in §4.3, and each
  write belongs to a UI record that says so.
- **R6 Every UI decision has a record.** Each element the plugin adds has a record in §5 with a use
  case and a UI design statement; each element considered and not built has one in §6. A UI change
  without a record, or one that contradicts its record, is not merged: the record changes in the
  same PR.

## 3. What the plugin system offers

From the Panel's plugin documentation, <https://pelican.dev/docs/panel/advanced/plugins>, for the
Panel version the chart deploys (`v1.0.0-rc.4`):

| What | How | Stability per the docs |
|---|---|---|
| Layout | `plugins/<id>/` with `plugin.json` (`id`, `name`, `author`, `category`, `namespace`, `class`, optional `panels`, `panel_version`, `api_version`, `composer_packages`, `update_url`) and `src/` | `plugin.json` schema is stable |
| Panels | `admin` (admin area), `app` (server list), `server` (client area, `Server` as tenant) | |
| New pages, resources, widgets | `discoverPages` / `discoverResources` / `discoverWidgets` under `src/Filament/<Panel>/` | discovery is stable |
| Widgets on existing pages | static hooks, e.g. `Console::registerCustomWidgets(ConsoleWidgetPosition::AboveConsole, …)`, `ListServers::registerCustomHeaderActions(…)`, `…Resource::registerCustomRelations(…)` (`CanModify`, `CanCustomize` traits) | best effort |
| Permissions | `Role::registerCustomPermissions`, `Subuser::registerCustomPermissions` | best effort |
| Settings page | `HasPluginSettings` (`getSettingsForm`, `getSettingsFormData`, `saveSettings`) | stable |
| Migrations, views, translations, providers, commands | auto-discovered | stable |
| Install | Admin → Plugins → Import (zip upload or a URL to a zip), Browse (Pelican Hub), or copy into `plugins/` and run `php artisan p:plugin:install`; installs, updates and uninstalls run on the queue worker (§4.6) | |
| Update | `update_url` points to a JSON feed keyed by Panel version (§4.6) | |

The docs call the plugin system "still in development". Internal services, models and Filament
internals may change in any Panel release, so the plugin pins `panel_version` and is tested against
the Panel version the chart deploys (§8, P0).

## 4. Architecture

### 4.1 Components

```
Browser ──► Panel (upstream image)
              └─ plugin pelican-k8s (PHP, Filament)
                    │  HTTPS, node daemon token (Bearer <token id>.<token>)
                    ▼
            gateway  /api/pelican-k8s/v1/*   (new, beside the Wings API)
                    │  informer caches, server-side dry runs
                    ▼
            Kubernetes API: GameServers, GameServerClasses, Nodes, Pods,
                            Services, PVCs, VolumeSnapshots, Events, MetalLB pools
```

### 4.2 Finding pelican-k8s nodes

The plugin calls `GET /api/pelican-k8s/v1/info` on every Panel node, with that node's daemon token, and
caches the answer for five minutes. A `200` with a known `apiVersion` marks a pelican-k8s node. A
`404` marks a node without the extension API (a Docker Wings node, or an older gateway); the plugin
shows nothing for it and never calls it again within the cache period. All plugin UI is scoped to
pelican-k8s nodes, so a Panel that manages Docker Wings nodes as well is unaffected (X-03).

### 4.3 Extension API

Routes on the gateway's Panel-facing listener, behind the same node-token check as the Wings routes.
Responses never contain egg variables, the env Secret, tokens or certificates.

| Method and path | Purpose | Write | Phase |
|---|---|---|---|
| `GET /info` | versions (gateway, operator, extension API), capabilities, Panel version compatibility, configuration checks | | P0 |
| `GET /servers` | per server: phase, class, node, process state, reason codes of conditions that are not healthy | | P1 |
| `GET /servers/{uuid}` | full status: conditions, placement and why, effective resources, ignored Panel settings, endpoints, last exit | | P1 |
| `GET /servers/{uuid}/events` | recent Kubernetes events of the server's objects, last hour, at most 50 | | P1 |
| `POST /allocations/check` | `{class, allocations: [{ip, port, server?}]}` → result per allocation (§7) | | P2 |
| `GET /capacity` | nodes (allocatable, requested by game pods, agent pods, other), shared-address groups | | P3 |
| `GET /classes` | classes with a derived summary and the number of servers using each | | P4 |
| `GET /class-rules`, `PUT /class-rules` | rules choosing the class of a new server by egg (§4.5) | `PUT` | P4 |
| `POST /servers/{uuid}/class?dryRun=true\|false` | preflight, or change `spec.className` | `dryRun=false` | P4 |
| `GET /servers/{uuid}/snapshots` | `VolumeSnapshot`s of the server's volume | | P5 |
| `GET /volumes/orphaned`, `DELETE /volumes/orphaned/{name}` | PVCs labelled `pelican-k8s.io/orphaned-at` | `DELETE` | P5 |

The API is versioned by path. `/info` lists the capabilities the gateway has, and the plugin hides
every element whose capability is missing, so a newer plugin works against an older gateway.

The gateway trusts the token holder, which is the Panel. Authorization by Panel user (owner, subuser,
admin role) happens in the plugin before it calls the gateway (UI-15).

### 4.4 Refresh and load

The gateway answers from informer caches and does not call the API server per request, except for
the dry runs in §7. The plugin caches answers for 10 seconds per node and endpoint. Widgets poll every
15 seconds while their page is open, using Filament polling. No background job runs in the Panel.

### 4.5 Class rules

A new server gets its class when the gateway creates its `GameServer`, which happens inside the
Panel's create call, before the plugin could act (X-05). So rules that choose a class have to be
applied by the gateway at creation:

- stored in a ConfigMap `pelican-k8s-class-rules` in the gateway's namespace: a list of
  `{eggUUID, className}` and an optional fallback; the chart's default class applies when nothing
  matches
- applied once, when the `GameServer` is created. The egg UUID is known at that point (it is already
  the label `pelican-k8s.io/egg-uuid`). Later rule changes do not touch existing servers
- editable with `kubectl` or GitOps; the plugin only offers editing when the ConfigMap has no Helm or
  Argo CD ownership labels (UI-11)

### 4.6 Packaging, distribution and updates

The install and update behaviour below was read from the Panel's source (`app/Models/Plugin.php`,
`app/Services/Helpers/PluginService.php`, `app/Filament/Admin/Resources/Plugins/`) on its `main`
branch. P0 checks it against the tag the chart deploys.

**Code location.** The plugin lives in this repository under `panel-plugin/pelican-k8s/`, because the
extension API and the plugin change together.

**What the Panel does on install, update and uninstall, and what follows for the plugin:**

| Panel behaviour | Consequence for the plugin |
|---|---|
| Install runs `composer require` in the Panel's base path for the `composer_packages` of every loaded plugin; uninstall runs `composer remove` | No `composer_packages`. They need `composer` and a writable app directory in the container, and change the Panel's own dependencies. The plugin uses only what the Panel ships (Laravel's HTTP client, Filament) |
| Install and update run the plugin's migrations and its seeder; uninstall resets the migrations | No migrations, no seeder. Nothing the plugin keeps needs a table: the toggles of UI-14 go to `.env`, which the chart keeps on `/pelican-data` |
| Install and uninstall run `yarn install` and `yarn build` for the whole Panel; for category `plugin` a failed build is only logged | No frontend assets: Filament components and Blade only (§5.1), so the plugin does not depend on that build |
| Status and load order are written into `plugins/<id>/plugin.json` under `meta`; the chart keeps `plugins/` on `/pelican-data` | Anything that replaces the plugin folder (an update, the chart, §9) has to keep `meta`, or the Panel treats the plugin as not installed |
| `panel_version` is checked at boot only: a mismatch marks the plugin `incompatible`; install and update check nothing | `panel_version` is the exact Panel version the chart deploys (no caret range across release candidates), and the update feed offers a plugin version only for Panel versions it was tested with |
| `api_version` above the Panel's supported version marks the plugin `incompatible` | `api_version: 1` |
| `panels` limits which Filament panels load the plugin | `panels: ["admin", "server"]` |
| A plugin that throws while booting is marked `crashed`; `PANEL_PLUGIN_SAFE_MODE=true` boots the Panel without any plugin | docs/operations.md names both for a Panel that fails after installing or updating the plugin |

**Package.** Each release attaches `pelican-k8s-panel-plugin-<version>.zip` and its SHA-256. The zip
holds the folder `pelican-k8s/` with `plugin.json` (without `meta`), `src/`, `lang/` and
`resources/views/`. The Panel accepts `plugin.json` at the zip root or one directory deep, takes the
id from `plugin.json` (`pelican-k8s` matches its id pattern `^[a-z0-9][a-z0-9._-]*$`) and moves the
folder to `plugins/pelican-k8s`. Category `plugin`. The plugin version is the pelican-k8s version.

**Channels.**

| Channel | How an admin uses it | Integrity |
|---|---|---|
| GitHub release (P0) | Admin → Plugins → Import, either from URL (the release asset URL; the Panel requires the URL to end in `.zip`) or by uploading the zip, then Install | Import checks no checksum; docs/install.md gives the SHA-256 and a command to verify it before uploading |
| Update feed (P0) | The Panel shows Update in its plugin list when the feed offers a newer version | The Panel checks `sha256` when the feed has it; the feed always has it |
| Pelican Hub (decided after P1, §9) | Admin → Plugins → Browse, install from the Hub | The Hub requires a checksum and HTTPS for in-panel installs |
| `charts/pelican-panel` (open, §9) | a chart value installs the plugin with the Panel | |

The official plugin repository (`pelican/plugins`) accepts no new plugins, so it is no channel.

**Update feed.** `plugin.json` sets `update_url` to
`https://github.com/Claiyc/pelican-k8s/releases/latest/download/panel-plugin-update.json`. The Panel
fetches it (5 s timeout, cached for 10 minutes), takes the entry whose key is its exact version, falls
back to the key `"*"`, and offers the update when `version_compare` finds the entry's `version`
newer than the installed one. The release workflow generates the feed from the compatibility matrix
in docs/compatibility.md, one key per tested Panel version and no `"*"`:

```json
{
  "v1.0.0-rc.4": {
    "version": "2.5.0",
    "download_url": "https://github.com/Claiyc/pelican-k8s/releases/download/v2.5.0/pelican-k8s-panel-plugin-2.5.0.zip",
    "sha256": "<64 hex digits>"
  }
}
```

A Panel version without a key is offered no update, which is correct: the Panel would install it and
mark it `incompatible` at the next boot. The key format (with or without the leading `v`, as the Panel
reports `config('app.version')`) and whether the Panel's HTTP client follows the `latest/download`
redirect are checked in P0.

**Plugin and gateway versions drift.** An admin updates the plugin in the Panel independently of the
cluster. A plugin newer than its gateway hides what the gateway's `/info` capabilities do not list
(§4.3); a gateway keeps every `/v1` route as long as it serves `/v1`, so an older plugin keeps working.

### 4.7 Gateway unreachable

When a pelican-k8s node does not answer, admin pages show one Filament warning per node ("The
pelican-k8s gateway for node X did not answer: <error>") in place of their content. User-facing
widgets show nothing: the Panel's own node-offline handling already tells users.

## 5. UI decisions

### 5.1 Principles all records follow

- **Quiet when healthy.** Elements that report problems show nothing when there is no problem. No
  green "all good" badges in the user's view.
- **Every message names who can act.** A message says what happens, whether it resolves by itself and,
  if not, who can fix it: "you" (only for things a user can change) or "an administrator".
- **Severity has one meaning everywhere.** Danger (red): the server cannot work until someone acts.
  Warning (yellow): it waits or may not start, and may resolve by itself. Info (grey): something
  worth knowing, nothing is wrong.
- **Users never see cluster internals.** No node, pod, namespace or class names in the server panel.
  Admin views show them.
- **Native Filament components only.** Sections, infolists, tables, badges, modals, notifications; no
  custom JavaScript, so the plugin follows the Panel's theme and dark mode.
- **Read-only unless the record says otherwise.** Every action that writes is named in its record,
  confirms before writing and states whether it can be undone.

### 5.2 Record format

Each record states:

- **Where**: panel, page and position
- **Who**: audience and permission (UI-15)
- **Use case**: the situation it serves
- **UI design statement**: what the element shows and does, and what it deliberately does not
- **States**: what it shows for each condition it can be in
- **Data**: the endpoint it reads or writes
- **Phase**

### 5.3 Server panel (users)

#### UI-01 Status banner

- **Where**: server panel, console page, `ConsoleWidgetPosition::AboveConsole`.
- **Who**: everyone who can open the server's console; no extra permission. Can be switched off in
  UI-14.
- **Use case**: a user presses Start and the Panel shows "starting" for minutes, because the game pod
  is `Pending`: no node has 8 GiB of memory free. Today the user sees nothing, retries, and opens a
  ticket. Other cases: the server was stopped because it ran out of memory (`lastExit.oomKilled`), the
  server will restart on another node once it is offline (`RecreatePending`), the disk cannot shrink
  (`DiskShrinkRefused`), the allocation port cannot be exposed (`PortOutOfRange`).
- **UI design statement**: a single Filament section with one sentence saying what is happening and an
  optional second sentence starting with "What you can do:" or "An administrator needs to". Colour by
  severity (§5.1). Text comes from a fixed translation catalog keyed by reason code; raw Kubernetes
  messages are never shown. No buttons, no links, no details toggle. Shown only while a reason in the
  catalog is active; a healthy server shows nothing. With several active reasons, the most severe one
  is shown.
- **States**: healthy → hidden; reason active → banner; reason not in the catalog → hidden (admins
  see it in UI-05); gateway unreachable or capability missing → hidden.
- **Data**: `GET /servers/{uuid}`.
- **Phase**: P1.

#### UI-02 Connect address

- **Where**: server panel, console page, `ConsoleWidgetPosition::AboveConsole`, below UI-01.
- **Who**: as UI-01. Can be switched off in UI-14.
- **Use case**: a class uses a cloud load balancer that assigns its own address or a hostname
  (`ipAnnotation` empty). The Panel shows the allocation IP the admin entered, which is not where
  players connect. `status.endpoints` holds the address that works.
- **UI design statement**: one info line "Players connect to `203.0.113.7:25565`" with Filament's copy
  action on the address. Shown only when an endpoint differs from the allocation IP and port the Panel
  shows; otherwise hidden. No editing; writing the address into the allocation's alias is an admin
  choice outside the plugin.
- **States**: endpoints equal the allocation → hidden; no endpoint yet → hidden; gateway unreachable →
  hidden.
- **Data**: `GET /servers/{uuid}`.
- **Phase**: P2.

### 5.4 Admin panel

All admin pages sit in a navigation group "Kubernetes" in the admin panel. With more than one
pelican-k8s node, every page has a node filter, defaulting to all.

#### UI-03 Overview

- **Where**: admin panel, page Kubernetes → Overview, first in the group.
- **Who**: admins with "View Kubernetes".
- **Use case**: an admin wants to know whether the pelican-k8s side is healthy and set up in a way that
  will work, before users find out: versions, whether the Panel version is supported, and
  configuration that will fail later. Examples: the Panel's client allocation port range lies outside
  the NodePort range of a NodePort class; LoadBalancer Services have had no address for more than two
  minutes; neither MetalLB pool discovery nor `externalIPs` is set, so allocation IPs cannot be checked.
- **UI design statement**: one section per pelican-k8s node with an infolist (Panel node name,
  gateway, operator and extension API versions, Panel version with "supported" or "not in the
  compatibility matrix", Kubernetes nodes ready / total, servers running / total). Below it a list of
  configuration checks, each a badge (OK, warning, danger) with one sentence and, for anything not OK,
  one sentence on how to fix it and a link to the matching section of the docs. No actions on this
  page.
- **States**: all checks OK → the list shows them collapsed into "All N checks passed"; gateway
  unreachable → §4.7.
- **Data**: `GET /info`.
- **Phase**: P0 (versions), P2 (allocation checks), later checks with their phases.

#### UI-04 Servers

- **Where**: admin panel, page Kubernetes → Servers.
- **Who**: admins with "View Kubernetes".
- **Use case**: an admin gets a ticket "my server won't start" or wants to see every server that has a
  problem right now, with the Kubernetes reason, without opening `kubectl`.
- **UI design statement**: a Filament table, one row per server on pelican-k8s nodes: Panel name (links
  to the Panel's own server edit page), owner, phase badge, process state, class, Kubernetes node,
  problem (the most severe active reason as a badge with its short text, empty when healthy). Default
  sort: problems first, then name. Filters: has problem, phase, class, node. Clicking a row opens UI-05.
  No row actions and no bulk actions; this page only finds servers.
- **States**: no servers → Filament empty state "No servers on pelican-k8s nodes"; gateway
  unreachable → §4.7.
- **Data**: `GET /servers`.
- **Phase**: P1.

#### UI-05 Server details

- **Where**: admin panel, page Kubernetes → Servers → a server.
- **Who**: admins with "View Kubernetes".
- **Use case**: the admin found a server in UI-04 or has its name from a ticket and needs to know why it
  is in its state and what to change: is it waiting for capacity, is it pinned to a node by its
  shared IP, its allocation IP or its node-local volume, what does it reserve on the node, which Panel
  settings have no effect on it.
- **UI design statement**: an infolist with sections in this order:
  1. *Status*: phase and a table of conditions (type, status, reason, message, since); unhealthy ones
     first and coloured by severity.
  2. *Placement*: game pod node, agent pod node, and each constraint that binds the server to a node
     as one sentence ("Shares 10.0.0.5 with 3 servers under `Local`: runs on node-b", "Volume is
     node-local on node-a", "Allocation IP 192.168.1.20 belongs to node-c").
  3. *Resources*: a table with the Panel value, the limit and the request for memory, CPU and disk
     (PVC size with overhead), and one sentence "Reserves X MiB memory and Y CPU on its node while
     running; Z while stopped".
  4. *Ignored Panel settings*: only those with a non-default value (`swap`, `io_weight`, `threads`,
     OOM killer disabled), each with "has no effect on Kubernetes"; hidden when none are set.
  5. *Endpoints*: the addresses players use.
  6. *Recent events*: the last events, newest first, collapsed by default.
  7. *Snapshots* (P5): name, created, size, ready; list only (X-08).
  Header actions: "Open in Panel" (the Panel's own server edit page) and "Change class" (UI-12).
- **States**: server unknown to the gateway → "This server has no GameServer object" with the Panel
  node it is on; gateway unreachable → §4.7.
- **Data**: `GET /servers/{uuid}`, `GET /servers/{uuid}/events`, `GET /servers/{uuid}/snapshots`.
- **Phase**: P1 (sections 1 to 6), P5 (section 7).

#### UI-06 Allocations

- **Where**: admin panel, page Kubernetes → Allocations.
- **Who**: admins with "View Kubernetes".
- **Use case**: the Panel accepts any IP and port as an allocation, but on pelican-k8s whether it works
  depends on the exposure mode of the class of the server that uses it, and on the cluster: a NodePort
  class needs ports in the NodePort range that no other Service holds, a HostPort class needs the port
  free on the node, a LoadBalancer class needs an address the load balancer can announce, and under
  `Local` the IP picks the node. Today a wrong allocation shows up as a server in `Error` or `Pending`.
  The admin wants to see every allocation that will not work, or will only work with conditions, and
  also to ask "could I use these free allocations for my NodePort class?".
- **UI design statement**: a Filament table of every allocation on pelican-k8s nodes: IP, port, alias,
  server (Panel name, or "free"), class it was checked against, result badge (OK, warning, danger, not
  checked), and the message of the most severe result. A header select "Check free allocations
  against" chooses the class used for free allocations (default: the class a new server would get
  from UI-11's fallback, else the chart's default class); assigned allocations are always checked
  against their server's class. Default filter: "problems only". Header action: "Add allocations"
  (UI-07). No row actions: the Panel's own node pages remain the place to edit and delete allocations,
  and each row links there.
- **States**: no problems → empty state "All N allocations are usable" with a button to clear the
  filter; a check that cannot run (for example no pool information for LoadBalancer) → "not checked"
  with the reason; gateway unreachable → §4.7.
- **Data**: the plugin reads allocations from the Panel database and sends them in batches to
  `POST /allocations/check`.
- **Phase**: P2.

#### UI-07 Add allocations

- **Where**: admin panel, header action on UI-06, opening a modal.
- **Who**: admins with "View Kubernetes" who may also create allocations on the node in the Panel.
- **Use case**: an admin adds ports for a NodePort class and wants to know before saving that 25565 is
  outside the range and 30080 is taken by another Service; or adds addresses for a shared IP and wants
  to see that the node behind it is already full.
- **UI design statement**: a modal form: node, class to check against, IP (a select fed by
  `/api/system/ips`, each option labelled with the Kubernetes node it belongs to and that node's free
  memory, or with the MetalLB pool it comes from), ports (the same syntax as the Panel: `25565`,
  `25565,25570`, `25565-25600`, at most 1000 ports), optional alias. After each change the modal
  checks the ports and lists them grouped by result, errors first. "Create" creates only ports without
  a danger result; a checkbox "Also create ports with warnings" includes warnings. A notification
  reports how many were created and how many were skipped. The allocations are created in the Panel's
  database exactly as the Panel's own form creates them; the plugin writes nothing to the cluster here.
- **States**: checking → Filament loading indicator on the list; gateway unreachable → the form shows
  §4.7's warning and "Create" is disabled.
- **Data**: `GET /api/system/ips`, `POST /allocations/check`; writes Panel allocations.
- **Phase**: P2.

#### UI-08 Capacity: nodes

- **Where**: admin panel, page Kubernetes → Capacity, tab "Nodes".
- **Who**: admins with "View Kubernetes".
- **Use case**: an admin wants to know which Kubernetes nodes have room for another server, why a
  server is `Pending`, and which allocation IPs place servers on which node, so that new allocations
  and servers go where they fit.
- **UI design statement**: a table, one row per Kubernetes node: name, ready, addresses, allocatable
  memory and CPU, requested by game pods, by agent pods and by everything else, free, running servers,
  allocation IPs that pin servers to it. Free memory and CPU are coloured: danger below the request of
  the largest server that could start there, warning below twice that. Expanding a row lists the
  servers on the node with their requests. Read-only.
- **States**: gateway unreachable → §4.7.
- **Data**: `GET /capacity`.
- **Phase**: P3.

#### UI-09 Capacity: shared addresses

- **Where**: admin panel, page Kubernetes → Capacity, tab "Shared addresses".
- **Who**: admins with "View Kubernetes".
- **Use case**: under `LoadBalancer` with `Local` and a sharing annotation, all servers on one IP run
  on one node, and the one that does not fit stays `Pending` ([classes.md, "Planning addresses under
  Local"](docs/classes.md#planning-addresses-under-local)). The admin needs to see, per IP, whether
  the servers on it fit together, before starting all of them on a busy evening.
- **UI design statement**: a table, one row per allocation IP used by more than one server under these
  conditions: IP, the node it is announced from, servers (running / total), memory and CPU the servers
  request when all run, free on that node, verdict "fits" or "X MiB short" (danger). Expanding a row
  lists the servers. A link per row opens UI-06 filtered to that IP. The page suggests nothing and
  moves nothing: allocations remain the admin's choice (classes.md).
- **States**: no shared addresses → empty state explaining when the tab has content; gateway
  unreachable → §4.7.
- **Data**: `GET /capacity`.
- **Phase**: P3.

#### UI-10 Classes

- **Where**: admin panel, page Kubernetes → Classes.
- **Who**: admins with "View Kubernetes".
- **Use case**: an admin chooses a class for a server (UI-12) or writes class rules (UI-11) and needs to
  know what each class means without reading its YAML: exposure mode and traffic policy, storage,
  how requests follow from Panel limits.
- **UI design statement**: a table, one row per class: name, default badge, servers using it, exposure
  mode and traffic policy, storage class, memory request %, CPU request %, managed by (Helm, Argo CD,
  or none, from ownership labels). A row opens a read-only infolist with the class grouped as in
  [docs/classes.md](docs/classes.md), and under each group one generated sentence on what it means for
  players and admins ("Game servers see a node's address for every player, so IP bans affect everyone
  behind that node"). No edit, create or delete actions (X-01).
- **States**: gateway unreachable → §4.7.
- **Data**: `GET /classes`.
- **Phase**: P4.

#### UI-11 Class rules

- **Where**: admin panel, page Kubernetes → Classes, section "Class for new servers" above the table.
- **Who**: viewing: "View Kubernetes"; editing: "Manage Kubernetes classes".
- **Use case**: an admin runs Minecraft servers on a class with more memory overhead and SSD storage,
  and everything else on the default class. Today every new server gets the chart's default class and
  the admin has to change it afterwards, after its volume was already created on the default class's
  storage.
- **UI design statement**: a table of rules: egg (name from the Panel, with its UUID), class. Below it
  the fallback: "Every other egg: <class>". Actions "Add rule", "Edit", "Delete" and a "Save" that
  writes the whole list at once. A sentence above the table: "Applies when a server is created.
  Existing servers keep their class." When the ConfigMap is managed by Helm or Argo CD, the section is
  read-only with a badge naming the manager and a sentence on where to change it.
- **States**: no rules → only the fallback line and "Add rule"; gateway unreachable → §4.7.
- **Data**: `GET /class-rules`, `PUT /class-rules` (write; replaces the ConfigMap's rule list; undone by
  editing again).
- **Phase**: P4.

#### UI-12 Change class

- **Where**: admin panel, header action "Change class" on UI-05, opening a modal.
- **Who**: admins with "Manage Kubernetes classes".
- **Use case**: an admin moves a server to another class, for example from a LoadBalancer class to a
  NodePort class. Its allocation port 25565 is outside the NodePort range, so the server would go to
  `Error`. The admin needs to know that, and when the change takes effect, before making it.
- **UI design statement**: a modal with a class select. Choosing a class runs a preflight and lists:
  allocation results against the new class (§7), what changes (exposure mode, traffic policy,
  resources: old and new request and limit), what does not change (the volume keeps its storage
  class; "storage class differs" is a warning), and when it applies ("next start" for a stopped
  server, "after the process is offline" for a running one). "Change class" is disabled while the
  preflight has a danger result. Confirm text: "The server's pods are recreated with the new class.
  You can change the class back the same way."
- **States**: preflight running → loading indicator; gateway unreachable → disabled with §4.7's
  message.
- **Data**: `POST /servers/{uuid}/class?dryRun=true`, then `dryRun=false` (write; sets
  `spec.className`).
- **Phase**: P4.

#### UI-13 Orphaned volumes

- **Where**: admin panel, page Kubernetes → Volumes.
- **Who**: viewing: "View Kubernetes"; deleting: "Delete Kubernetes volumes".
- **Use case**: with `deletionPolicy: Retain`, deleting a server leaves its PVC. Over months these add
  up to storage nobody sees in the Panel. The admin wants to see them, know which server they belonged
  to, and delete the ones no longer needed.
- **UI design statement**: a table of PVCs labelled `pelican-k8s.io/orphaned-at`: name, server (the
  Panel name recorded on the volume, and its UUID), size, storage class, orphaned at, age. Row action
  "Delete" with a confirmation that names the PVC and says "This deletes the volume and its data. It
  cannot be undone." No bulk delete.
- **States**: none → empty state explaining when volumes appear here; gateway unreachable → §4.7.
- **Data**: `GET /volumes/orphaned`, `DELETE /volumes/orphaned/{name}` (write; irreversible).
- **Phase**: P5.

### 5.5 Plugin settings and permissions

#### UI-14 Plugin settings

- **Where**: the plugin's settings page (`HasPluginSettings`), opened from the Panel's plugin list.
- **Who**: admins who may manage plugins.
- **Use case**: an admin installs the plugin and wants to confirm it found the pelican-k8s nodes, and
  decide whether users see Kubernetes status at all (some hosters prefer that users only see the
  Panel's own state).
- **UI design statement**: a read-only list of Panel nodes with "pelican-k8s" or "not pelican-k8s",
  gateway version and extension API version; a "Detect again" button that clears the cache of §4.2.
  Two toggles: "Show the status banner to users" (UI-01, default on), "Show the connect address to
  users" (UI-02, default on). Nothing else: everything else comes from the cluster.
- **States**: no pelican-k8s node found → a warning explaining what detection calls and linking to the
  install docs.
- **Data**: `GET /info` per node; the toggles are stored in `.env` (§4.6).
- **Phase**: P0.

#### UI-15 Permissions

- **Where**: the Panel's role editor (admin roles) via `Role::registerCustomPermissions`.
- **Who**: admins who edit roles.
- **Use case**: a hoster's support staff should see why servers fail (UI-03 to UI-10) without being
  able to change classes or delete volumes.
- **UI design statement**: three permissions in a group "Kubernetes": "View Kubernetes" (all read-only
  admin pages), "Manage Kubernetes classes" (UI-11 editing, UI-12), "Delete Kubernetes volumes"
  (UI-13). UI-07 needs "View Kubernetes" plus the Panel's own permission to create allocations. Root
  admins have all. No subuser permission: UI-01 and UI-02 follow console access and contain nothing a
  user of the server should not see.
- **Phase**: P0.

## 6. Rejected and deferred UI decisions

#### X-01 Class editor (rejected)

- **Use case**: an admin wants to tune a class (overheads, snapshot schedule, egress) from the Panel.
- **Why not**: a class controls egress CIDRs, `network.enabled`, `install.runAsRoot`, seccomp and
  storage. Editing classes from the Panel turns a compromised Panel admin account into weakened
  cluster security policy. Classes are cluster-scoped and often applied by GitOps, which would revert
  Panel edits. UI-10 shows classes; changing them stays with `kubectl`, Helm or GitOps.

#### X-02 Request and limit fields per server (rejected)

- **Use case**: an admin wants one server to reserve more CPU than its class gives.
- **Why not**: per-server overrides undermine classes as policy, and using them well needs knowledge of
  the scheduler. A different need is a different class. UI-05 shows the effective values instead.

#### X-03 Hiding ignored settings in the Panel's server form (rejected)

- **Use case**: an admin sets swap and expects it to apply.
- **Why not**: it needs changes to the Panel's own server form, which has no documented hook, and
  would have to apply per node, since a Panel can manage Docker Wings nodes too. UI-05 lists the ignored
  settings a server has.

#### X-04 Users choose their class (rejected)

- **Use case**: a user wants better hardware for their server.
- **Why not**: classes carry cost and policy; that is a hoster decision (plans, products) and belongs
  in billing systems that drive the Panel, not in the plugin.

#### X-05 Class select in the Panel's server creation form (rejected)

- **Use case**: an admin picks the class while creating a server.
- **Why not**: it needs a change to the Panel's create form, and the Panel calls the gateway during
  creation, before a plugin could pass a value along. UI-11 covers the common case (class by egg),
  UI-12 the rest.

#### X-06 Checks inside the Panel's allocation form and transfer dialog (rejected)

- **Use case**: an admin adds allocations or starts a transfer to the cluster and wants the checks
  there.
- **Why not**: no documented hooks for either. UI-07 replaces the allocation form for pelican-k8s
  nodes; allocations an incoming transfer reserves show up in UI-06.

#### X-07 Kubernetes columns in the Panel's server tables (rejected)

- **Use case**: an admin wants to see problems in the Panel's own server list.
- **Why not**: changing tables is not a documented hook. UI-04 is the list for that.

#### X-08 Snapshot restore (deferred)

- **Use case**: a user broke their world and an hourly snapshot exists.
- **Why deferred**: a restore replaces the volume of a server that has an agent, maybe running work,
  and Panel backups; it needs its own design in the operator first. UI-05 lists snapshots.

#### X-09 Pod logs and a YAML view (rejected)

- **Use case**: an admin wants to debug without `kubectl`.
- **Why not**: §1 non-goal. Logs of the game process are already the console; logs of pelican-k8s
  components belong in the cluster's logging.

## 7. Allocation checks

`POST /allocations/check` runs the same rules the operator applies, from code shared with the
operator. Each result has a code, a severity and an admin message. "Danger" means the server will not
work with this allocation; "warning" means it may stay `Pending` or works with conditions.

| Code | Modes | Severity | Detection |
|---|---|---|---|
| `IPNotInPool` | LoadBalancer | danger | IP not in the discovered MetalLB pools or `externalIPs`; "not checked" when neither is known |
| `PortOutOfRange` | NodePort | danger | server-side dry-run create of a NodePort Service with that port; the API server knows the range |
| `NodePortTaken` | NodePort | danger | same dry run: the port is allocated to another Service, inside or outside pelican-k8s |
| `HostPortTaken` | HostPort | danger | a pod on the node declares the port as `hostPort`, a `hostNetwork` pod listens on it, or it is a kubelet or kube-proxy port |
| `SameNodePort` | HostPort | danger | another allocation with the same port lands on the same node through a different IP of that node |
| `AllocationIPNotOnNode` | NodePort, HostPort (`Local`, more than one node) | danger | no node has the IP as InternalIP or ExternalIP |
| `NodeFull` | every mode that pins to a node | warning | the server's game pod requests do not fit in the free room of the node the IP pins it to |
| `SharedIPShort` | LoadBalancer, `Local`, sharing | warning | the servers on the IP together do not fit on the node announcing it |
| `VolumeElsewhere` | every mode that pins to a node | warning | the server's volume is node-local on another node than the one the IP requires |
| `ClientRangeOutsideNodePorts` | NodePort | warning | (in `/info`, UI-03) the Panel's client allocation range lies outside the NodePort range |

## 8. Phases

Each phase merges on its own and leaves the plugin usable. Gateway changes come with Go unit tests;
the plugin gets a CI job that starts the chart's Panel image with the plugin installed against a fake
gateway and checks that every page renders.

- **P0 Foundation**: `/info` and the extension API's routing and auth; plugin skeleton (`plugin.json`,
  panels `admin` and `server`), node detection (§4.2), UI-03 (versions), UI-14, UI-15; release
  workflow for the zip, its SHA-256 and the update feed (§4.6); install and update docs; CI job. Done
  when the plugin installs into the chart's Panel from the release URL, updates through the feed, and
  shows UI-03 for a kind cluster.
- **P1 Status**: `/servers`, `/servers/{uuid}`, events; reason catalog with user and admin texts;
  UI-01, UI-04, UI-05 sections 1 to 6.
- **P2 Allocations**: `/allocations/check` (§7), UI-06, UI-07, UI-02, UI-03's allocation checks.
- **P3 Capacity**: `/capacity`, UI-08, UI-09.
- **P4 Classes**: `/classes`, class rules in the gateway (§4.5), UI-10, UI-11, UI-12.
- **P5 Volumes**: snapshots and orphaned PVCs, UI-05 section 7, UI-13.

## 9. Open questions

- **Install cost in the container.** Every plugin install and uninstall runs `yarn install` and
  `yarn build` for the whole Panel (§4.6). Whether the upstream image has `yarn`, how long the build
  takes on the queue worker, and whether a failed build leaves the Panel's assets intact is measured
  in P0.
- **Pelican Hub listing.** A listing gives in-panel install from Browse. The Hub needs a Discord
  account, reviews every version, installs only free plugins from the Panel, and its terms forbid
  undisclosed remote calls, so the listing has to state that the plugin calls the gateway of each
  pelican-k8s node with that node's token. Decided after P1, when the plugin does something worth
  listing.
- **className through sync.** The gateway builds a server's desired `GameServer` with the default
  class. Before P4, confirm that updates keep an existing `spec.className` and add a test for it.
- **Installing with the chart.** Whether `charts/pelican-panel` should offer to install the plugin: an
  init container placing the release zip's folder in `plugins/` when the version differs, keeping
  `meta` in `plugin.json`, and running `p:plugin:install` only while the plugin is not installed.
  Without that care every pod start would reinstall it (composer, migrations, `yarn build`).
- **Creating allocations.** UI-07 creates allocations the way the Panel's form does. Whether that goes
  through a Panel service class (internal, may change) or the model directly is decided in P2.
- **Several gateways.** A Panel can have several pelican-k8s nodes (several clusters). The node filter
  of §5.4 covers the pages; whether classes and rules need a per-cluster grouping is decided in P4.
- **RBAC for checks.** `HostPortTaken` and UI-08 need to read pods and nodes cluster-wide. The gateway
  may get these read-only rights only when the extension API is enabled in the chart.
