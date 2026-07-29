# Tunneler-only documentation previews

Research spike: can docpreview publish a preview that is reachable **only** from a machine running a plain
OpenZiti tunneler (Ziti Desktop Edge for Windows, `ziti-edge-tunnel`) with an enrolled identity, and with no
public surface at all?

Status: research only. No code in this worktree was changed.

## Verdict

Feasible, but **not** by reusing zrok. A zrok private share is invisible to a plain tunneler by construction:
zrok attaches exactly one config to the OpenZiti service it creates — `zrok.proxy.v1` — and never an
`intercept.v1` or `host.v1`, because both ends of a zrok share are SDK code (`ListenWithOptions` on the
backend, `DialWithOptions` in `zrok access private`). A tunneler that could see the service would have no
client config to build a DNS name or an intercept from. The route that does work is docpreview talking
directly to an OpenZiti network's **edge management API**: create one wildcard service with an `intercept.v1`
for `*.docpreview.ziti`, bind it from docpreview's own identity via `sdk-golang`'s `Context.Listen` (the exact
same shape as today's zrok listener, so `Exposer` survives unchanged), and mint per-reviewer identities with
one-time enrollment JWTs. On the client side ZDEW turns out to be automatable: its GUI is a thin client over
the named pipe `\\.\pipe\ziti-edge-tunnel.sock`, and a single line of JSON — `{"Command":"AddIdentity",
"Data":{"JwtContent":"…"}}` — is the whole enrollment path, so docpreview can either emit a `.jwt` for manual
import or drive the pipe directly. The prerequisite docpreview does not have today is an OpenZiti network it
holds admin credentials for; zrok cannot supply one.

## Findings

### 1. zrok private shares are unreachable from a plain tunneler

**Service name is the share token.** `controller/share.go:488-500` (`allocatePrivateResources`) creates the
ziti service with `Name: shrToken`, tagged `zrokShareToken=<token>`.
`checkPrivateShareTokenAvailability` (`controller/share.go:418-430`) rejects a requested token if
`ziti.Services.GetByName(privateShareToken)` succeeds — the zrok token namespace *is* the ziti service-name
namespace.

**No intercept config, and no host config either.** The only config attached is type `zrok.proxy.v1`:

- `sdk/golang/sdk/config.go:8` — `const ZrokProxyConfig = "zrok.proxy.v1"`
- `controller/bootstrap.go:82-85` — `assertZrokProxyConfigType`
- `controller/share.go:473-486` — `ConfigTypeID: zrokProxyConfigId`, data `{interstitial, auth_scheme,
  basic_auth, oauth}`

Repo-wide greps for `intercept.v1`, `ziti-tunneler-client`, `clientConfig`, `host.v1`,
`ziti-tunneler-server` return **zero** matches in the zrok v2 tree, and GitHub code search for
`intercept.v1 repo:openziti/zrok` returns `total_count: 0`. Both ends are pure SDK:

- bind: `sdk/golang/sdk/listener.go:34` — `zctx.ListenWithOptions(shrToken, opts)`
- dial: `sdk/golang/sdk/dialer.go:27` — `zctx.DialWithOptions(shrToken, …)`, and the same call in every
  frontend (`endpoints/proxy/frontend.go:99`, `endpoints/tcpTunnel/frontend.go:79`,
  `endpoints/publicProxy/http.go:99`, …)

So: **zrok's services deliberately lack intercept config.** `zrok access private` dials the service by name
over the SDK and terminates traffic in a local listener it binds itself. A stock `ziti-edge-tunnel` or ZDEW
enrolled into zrok's underlying network would, at best, see the service in its service list and be unable to
produce a DNS entry or intercept a single packet for it.

**Policies.** Private share creation makes two objects (`controller/share.go:502-533`): a Bind service policy
`<envZId>-<shrZId>-bind` with `IdentityRoles: ["@"+envZId]`, and a service-edge-router policy with
`EdgeRouterRoles: ["#all"]`. There is an explicit comment at `controller/share.go:535-536` that private shares
create no dial policy — dial rights come only from `POST /access`
(`controller/access.go:100-119`), which always uses `IdentityRoles: ["@"+envZId]` where `envZId` is validated
at `controller/access.go:29-48` to be a zrok **environment** identity owned by the calling account. So an
arbitrary externally-enrolled ziti identity cannot be granted dial rights through the zrok API.

Two escape hatches, neither of which helps:

- A zrok admin can mint a ziti identity through zrok (`controller/createIdentity.go:37-56`,
  `IdentityTypeService` + a `#all` edge-router policy) and get its enrolled identity JSON — but that creates
  no dial policy for any share.
- Anyone with direct management-API credentials on the underlying network can hand-author a dial policy for
  `@<arbitraryIdentity>` → `@<shrZId>`. zrok won't know about it, and zrok's GC (`controller/gc.go`) reaps by
  zrok tags. It still would not give a tunneler an intercept.

**v1 vs v2: no substantive change.** v1.1.11 `controller/sharePrivate.go` does the same four steps via
`zrokEdgeSdk` (renamed to `controller/automation` in v2). Service naming, config type, absence of
intercept/host configs, and policy scoping are identical.

*Conclusion for Q1: a plain tunneler cannot reach a zrok private share. This route is closed.*

### 2. Provisioning identities

#### 2a. Self-hosted / raw OpenZiti — fully supported, and the dependencies are already in the tree

`go.mod` already carries `github.com/openziti/edge-api v0.26.48` and `github.com/openziti/sdk-golang v1.2.8`
as indirect dependencies (via zrok). Promoting them to direct requires no new module graph.

**There is an even shorter path than writing this from scratch.** zrok v2 — already a direct dependency —
ships public wrappers for exactly this workflow at
`zrok/v2@v2.0.4/controller/automation/{api,identity,config,service,servicePolicy,edgeRouterPolicy,serviceEdgeRouterPolicy}.go`.
`import "github.com/openziti/zrok/v2/controller/automation"` costs zero new modules. Worth reading before
writing anything, even if docpreview ends up calling `edge-api` directly for control over error handling.

**Management client, the simple way** — `edge-api@v0.26.48/rest_util/clients.go`:

```go
// rest_util/capool.go:85
caCerts, _ := rest_util.GetControllerWellKnownCas(apiEndpoint)
caPool := x509.NewCertPool()
for _, ca := range caCerts { caPool.AddCert(ca) }
// rest_util/clients.go:68 — also …WithCert (:77), …WithToken (:49), …WithAuthenticator (:86)
edge, err := rest_util.NewEdgeManagementClientWithUpdb(user, pass, apiEndpoint, caPool)
```

Default base path is `/edge/management/v1` (`rest_management_api_client/ziti_edge_management_client.go:74`).
The client exposes one sub-client per resource (`Identity`, `Service`, `Config`, `ServicePolicy`,
`EdgeRouterPolicy`, `ServiceEdgeRouterPolicy`). This is what zrok's `automation/api.go:31-42` uses.

**Management client, the other way** — `sdk-golang/edge-apis/clients.go:207-244`,
`NewManagementApiClient(apiUrls []*url.URL, caPool *x509.CertPool, totpCallback func(chan string))` plus
`BaseClient.Authenticate` (`:101`) with credentials from `edge-apis/credentials.go`
(`NewUpdbCredentials :338`, `NewCertCredentials :220`, `NewIdentityCredentials :248`). Heavier, but it is the
only one that speaks OIDC and multi-controller HA. For a single self-hosted controller, prefer `rest_util`.

**Create identity + get the one-time JWT.**

```go
// POST /edge/management/v1/identities
&rest_model.IdentityCreate{
    Name:           &name,                       // required
    Type:           rest_model.IdentityTypeDefault,
    IsAdmin:        &falsePtr,                   // required
    RoleAttributes: &rest_model.Attributes{"docpreview-reader"},
    Enrollment:     &rest_model.IdentityCreateEnrollment{Ott: true},
}
```

`IdentityCreateEnrollment` is `edge-api@v0.26.48/rest_model/identity_create.go:540-550` — fields `Ott bool`,
`Ottca string`, `Updb string`. The create response returns only the id; the **JWT is on the detail read**:
`GET /identities/{id}` → `rest_model.IdentityDetail.Enrollment`
(`rest_model/identity_detail.go:90`, type `*IdentityEnrollments`) → `.Ott.JWT`
(`rest_model/identity_enrollments.go:44-54` and `:233-236`). So the flow is create → detail → read
`detail.Enrollment.Ott.JWT`.

Note the two-call shape: `CreateIdentityCreated.Payload` is a `rest_model.CreateEnvelope` whose `Data` is a
`CreateLocation` carrying only `ID` and `_links` (`rest_model/create_location.go:43-50`) — **the JWT is not on
the create response.** The `ziti` CLI does the same second GET
(`ziti/v2@v2.0.0/ziti/cmd/edge/create_identity.go:216-229`), as does zrok
(`automation/identity.go:104-109`).

That JWT string is exactly what ZDEW's `AddIdentity` wants (see §3), and also what
`ziti-edge-tunnel enroll --jwt` and `ziti edge enroll` want.

**Re-minting a JWT** for an existing identity — needed if a reviewer loses theirs or one expires — is
`enrollment.CreateEnrollment` → `POST /enrollments` with
`rest_model.EnrollmentCreate{IdentityID, Method: "ott", ExpiresAt}` (`rest_model/enrollment_create.go:45-66`),
or `POST /enrollments/{id}/refresh`. CLI: `ziti edge create enrollment ott <identity> --jwt-output-file`.

**Well-known config type IDs** (verified in ziti source, `controller/db/migration_initialize.go`):

| config type    | id          | source |
|----------------|-------------|--------|
| `intercept.v1` | `g7cIWbcGg` | `migration_initialize.go:492-494` |
| `host.v1`      | `NH5p4FpGR` | `migration_initialize.go:448-451` |

(`ziti/cmd/ops/database/anonymize-db.go:623` carries the same `g7cIWbcGg // intercept.v1` comment; the same
pair appears at `ziti/v2@v2.0.0/controller/db/migration_initialize.go:483,528`.) Note `host.v1` is
`NH5p4Fp**GR**`, not `NH5p4FpjQ` — a grep for `NH5p4Fpj` across the whole openziti module cache returns
nothing. Safer than hardcoding either: `GET /config-types?filter=name="intercept.v1"` once at startup.

Because docpreview binds the service itself through the Go SDK rather than through a hosting tunneler, it needs
**only** the `intercept.v1` config. `host.v1` exists for tunneler-hosted services and is not required here.

**`intercept.v1` schema**, verbatim from `ziti@v1.6.0/tunnel/entities/intercept.v1.json`. Required:
`protocols`, `addresses`, `portRanges`. Optional: `allowedSourceAddresses`, `dialOptions`
(`connectTimeoutSeconds`, `identity`), `sourceIp`. `additionalProperties: false`.

```json
{
  "protocols":  ["tcp"],
  "addresses":  ["docpreview.ziti", "*.docpreview.ziti"],
  "portRanges": [{"low": 80, "high": 80}]
}
```

List the apex explicitly: the wildcard matcher keys on the suffix `name[1:] + "."`, so `*.docpreview.ziti`
matches `foo.docpreview.ziti` but **not** bare `docpreview.ziti` (`tunnel/dns/server.go:167`). Useful if you
want an index page listing live previews.

The `listenAddress` definition (line 18 of that file) documents the address forms explicitly: idn-hostname
format, and "client applications will need to look for _valid_ ips, cidrs, and wildcards when parsing intercept
addresses and treat them accordingly." Wildcards are a first-class address form.

**Objects docpreview would create**, in order:

1. `POST /configs` — `{name: "docpreview-intercept", configTypeId: "g7cIWbcGg", data: {…above…}}`
2. `POST /services` — `{name: "docpreview", encryptionRequired: true, configs: [<configId>],
   roleAttributes: ["docpreview"]}`
3. `POST /service-policies` — Bind: `{type: "Bind", identityRoles: ["@<docpreviewIdentityId>"],
   serviceRoles: ["@<serviceId>"], semantic: "AllOf"}`
4. `POST /service-policies` — Dial: `{type: "Dial", identityRoles: ["#docpreview-reader"],
   serviceRoles: ["@<serviceId>"], semantic: "AllOf"}` — attribute-based, so granting a new reviewer is just
   the `docpreview-reader` role attribute on their identity and needs no policy edit.
5. `POST /service-edge-router-policies` — `{serviceRoles: ["@<serviceId>"], edgeRouterRoles: ["#all"]}`
6. Edge-router policy for the identities — **usually already there.** `ziti edge quickstart` creates
   `all-endpoints-public-routers` (`--edge-router-roles "#public" --identity-roles "#all"`) and
   `all-routers-all-services` (`--edge-router-roles "#all" --service-roles "#all"`)
   — `ziti/v2@v2.0.0/ziti/run/quickstart.go:722-753`; the docker/k8s quickstarts create the same pair under
   the names `allEdgeRouters` / `allSvcAllRouters`. Note the ERP is `#public`, not `@all`/`@all`, so routers
   must carry the `public` attribute. List first, create only if absent.

Role syntax is validated strictly — `ziti/v2@v2.0.0/controller/db/util.go:29-49`: every entry must be prefixed
`#` (role attribute) or `@` (name or id), and `#all`, if used, must be the **only** entry in its list.

Steps 1-3, 5 are one-time bootstrap. Steps 4 and 6 are one-time too under the attribute model. Per-preview,
nothing is created at all — see §4.

**CLI fallback**, if docpreview would rather shell out than link the API:

```
ziti edge login <controller>:1280 -u admin -p <pw>

# identity + JWT in one shot; the CLI re-GETs the identity and pulls enrollment.ott.jwt
ziti edge create identity reviewer-alice \
  --role-attributes docpreview-reader --jwt-output-file ./alice.jwt

ziti edge create config docpreview-intercept intercept.v1 \
  '{"protocols":["tcp"],"addresses":["docpreview.ziti","*.docpreview.ziti"],"portRanges":[{"low":80,"high":80}]}'

ziti edge create service docpreview-svc --configs docpreview-intercept -e ON

ziti edge create service-policy docpreview-bind Bind \
  --identity-roles '@docpreview-host' --service-roles '@docpreview-svc'
ziti edge create service-policy docpreview-dial Dial \
  --identity-roles '#docpreview-reader' --service-roles '@docpreview-svc'

# only if the quickstart defaults are absent
ziti edge create edge-router-policy all-endpoints-public-routers \
  --edge-router-roles '#public' --identity-roles '#all'
ziti edge create service-edge-router-policy all-routers-all-services \
  --edge-router-roles '#all' --service-roles '#all'
```

**Self-enrolling docpreview's own hosting identity** is also in-tree:
`sdk-golang@v1.2.8/ziti/enroll/enroll.go:161` — `Enroll(enFlags EnrollmentFlags) (*ziti.Config, error)`, with
`ParseToken` at `:86`. So `docpreview` can be handed one JWT at setup and produce its own identity JSON, which
`ziti.NewContextFromFile` (`ziti/contexts.go:54`) then loads.

#### 2b. NetFoundry

**NetFoundry Cloud (v2 API): yes, it mints endpoints and returns a JWT.** In NetFoundry vocabulary an
"endpoint" is a ziti identity, and the JWT it hands back is an ordinary OTT enrollment token — exactly what
ZDEW's `AddIdentity` consumes.

Auth: download `credentials.json` from NF Console → Organization → Manage API Account, giving
`{clientId, password, authenticationUrl}`. Client-credentials POST to that `authenticationUrl` with
`grant_type=client_credentials` and
`scope=https%3A%2F%2Fgateway.production.netfoundry.io%2F%2Fignore-scope` (the literal double slash and the
dummy `/ignore-scope` are both required by Cognito), then `Authorization: Bearer …`. Legacy orgs use Auth0 at
`https://netfoundry-production.auth0.com/oauth/token` with `audience:
"https://gateway.production.netfoundry.io/"`.

```
POST https://gateway.production.netfoundry.io/core/v2/endpoints
{"networkId":"…","name":"docpreview-alice","enrollmentMethod":{"ott":true},
 "attributes":["#docpreview-reader"]}
```

Returns **202 Accepted** — creation is asynchronous. The resource carries top-level `jwt` and `jwtExpiresAt`;
because of the async create, poll `GET /core/v2/endpoints/{id}` (cache disabled) until `jwt` is non-null. It
reverts to `null` once the endpoint enrolls, which doubles as a "has this reviewer enrolled yet?" signal. The
live v2 API doc needs no auth: `https://gateway.production.netfoundry.io/core/v2/docs/index.html`. Best
call-sequence reference is `github.com/netfoundry/python-netfoundry`
(`netfoundry/network.py` `create_endpoint`, `netfoundry/organization.py` for auth).

The v3 path degenerates into the plain edge-api flow: `POST /core/v3/networks/{id}/exchange` with
`{"type":"SESSION"}` returns `.value`, a ziti session token used directly against the network's controller as
`zt-session: <value>`. Whether `/core/v3/endpoints` exists is unknown — no public v3 docs page, and
unauthenticated probes 401.

**NetFoundry-hosted networks also expose the plain ziti management API.** NetFoundry's own
"Provision a network and service" guide drives `POST /identities`, `POST /services`, `POST /service-policies`
under `https://{NF_CONTROLLER}/edge/management/v1/`, authenticating with a `zt-session` header, and documents
the JWT at `.data.enrollment.ott.jwt` on `GET /identities/{id}` — byte-for-byte the shapes in §2a. So the code
docpreview writes for a self-hosted controller works unchanged against a NetFoundry-hosted network; only
authentication differs.

**Frontdoor: no.** The Frontdoor API docpreview already talks to (`internal/expose/frontdoor.go`, `/shares`
under `https://gateway.production.netfoundry.io/frontdoor`) is a public-ingress product; its published
OpenAPI reference covers shares, frontends, custom-frontends, agents and executions, with no
endpoint-enrollment resource at all. Treat the NetFoundry Cloud v2 API, not Frontdoor, as the NetFoundry
route.

### 3. What ZDEW accepts

Source: `D:\git\github\openziti\desktop-edge-win`.

**The GUI is a thin client over a named pipe, and the pipe is the automation surface.**

- Pipes: `\\.\pipe\ziti-edge-tunnel.sock` (commands, InOut) and `\\.\pipe\ziti-edge-tunnel-event.sock`
  (events) — `ZitiDesktopEdge.Client/ServiceClient/DataClient.cs:116-117`. A `-P <discriminator>` instance
  suffix appends `.<disc>` to both (`:135-144`; enumeration in `ServiceClient/TunnelInstanceDiscovery.cs:44-135`).
- Framing: one line of UTF-8 JSON terminated by `\n`, one JSON line back
  (`ServiceClient/AbstractClient.cs:152-174`).
- The enrollment message (`DataClient.cs:228-254`, payload shape at `DataStructures/DataStructures.cs:285-298`):

```json
{"Command":"AddIdentity","Data":{
  "UseKeychain":false,
  "IdentityFilename":"my-preview-reader",
  "JwtContent":"eyJhbGciOi…",
  "Key":null,"Certificate":null,"ControllerURL":null
}}
```

`EnrollMode`/`Provider` are omitted when empty by a `ShouldSerializeContractResolver`
(`AbstractClient.cs:394-423`). `Code == 0` in the reply means success and `Data` is the new identity. Follow
with `IdentityOnOff` to enable it, mirroring `DesktopEdge/MainWindow.xaml.cs:2224`.

Answering the sub-questions directly:

- **`.jwt` file** — yes, and it is the primary path. `MainWindow.xaml.cs:2252-2319` (`AddIdentity_Click`):
  `OpenFileDialog` filtered to `"Ziti Identities (*.jwt)|*.jwt"` (`:2254-2257`), file contents go into
  `payload.JwtContent` (`:2262-2266`), the `em` claim is base64-decoded and switched on (`:2296-2314`) —
  `ott`/`network` enroll directly, `ottca`/`ca` divert to the third-party-CA dialog. Then `AddId` →
  `serviceClient.AddIdentityAsync` (`:2202-2242`).
- **Already-enrolled `.json` identity** — **no such import path.** There is no `OpenFileDialog` for `*.json`
  anywhere in the tree, and `AddIdentity` is the only add command. `ztAPI` appears only as a read-model field
  on status responses (`DataStructures.cs:300-309`, consumed at `DesktopEdge/Models/ZitiIdentity.cs:124`). The
  nearest thing is the third-party-CA flow (`DesktopEdge/Views/Controls/AddIdentityCA.xaml.cs:40-72`), which
  takes cert and key *file paths* and still performs an enrollment.
- **URL enrollment** — yes, but it is controller-URL + external JWT signer, not "fetch a JWT from a URL".
  `DesktopEdge/Views/Controls/AddIdentityUrl.xaml.cs:44-85` does `GET {base}/external-jwt-signers`, and
  `DesktopEdge/ViewModels/AddIdentityViewModel.cs:126-147` builds a payload with `ControllerURL`,
  `EnrollMode` (`"token"`/`"cert"`), `Provider` and **no JWT**. Not useful for a one-time-token flow.
- **IPC** — see above. This is the automatable path.
- **URL scheme handler** — **none.** The installer is Advanced Installer
  (`Installer/ZitiDesktopEdge.aip`), not WiX; grepping it for `ziti://`, `URL Protocol`, `ProgId`,
  `shell\open`, `.jwt`, `FileAssociation` returns nothing outside stale MSI logs under `Installer/may21/`.
- **CLI args** — **none.** `DesktopEdge/App.xaml.cs:50-98` `OnStartup` ignores `StartupEventArgs` entirely. The
  single-instance pipe `ZitiDesktopEdgePipe` (`App.xaml.cs:41,70-115`) only ever carries the literal string
  `"showscreen"`.
- **`ziti-edge-tunnel.exe` is bundled.** `Installer/build.ps1:61-118` downloads
  `ziti-edge-tunnel-Windows_x86_64.zip` from the `ziti-tunnel-sdk-c` releases and installs it
  (`ZitiDesktopEdge.aip:112`). So `ziti-edge-tunnel enroll --jwt … --identity …` exists on disk, but writing
  the resulting JSON into the service's identity directory (under
  `C:\Windows\System32\config\systemprofile\AppData\Roaming\NetFoundry\…`, per
  `ZitiUpdateService/CONFIGURATION.md:14`) needs SYSTEM rights and a tunnel restart. The pipe is strictly
  better.
- **Drop folder / registry / MDM provisioning** — **none for identities.** The only `FileSystemWatcher` is on
  `settings.json` (`ZitiUpdateService/utils/Settings.cs:29,70`). The policy registry keys
  `HKLM\SOFTWARE\Policies\NetFoundry\Ziti Desktop Edge for Windows\{ziti-monitor-service,ui}`
  (`DesktopEdge/Utils/ManagedSettingsReader.cs:34-92`, `ZitiUpdateService/utils/PolicySettings.cs:44-96`)
  recognize only `AutomaticUpdatesDisabled`, `AutomaticUpdateURL`, `AlivenessChecksBeforeAction`,
  `DefaultExtAuthProvider`.
- **The monitor pipe cannot enroll.** `ZitiDesktopEdge.Client/Server/IPCServer.cs:31-32` —
  `\\.\pipe\OpenZiti\ziti-monitor\ipc` and `…\events`, DACL explicitly granting `AuthenticatedUserSid`
  `CreateNewInstance | ReadWrite` (`:76-79`, `:114-117`). Its ops (`processMessageAsync`, `:205-289`) are
  `stop`/`start`/`status`/`capturelogs`/update-related only — no identity op.

**Bottom line for Q3:** docpreview can automate this. Emitting a `.jwt` for the user to import through the GUI
is the zero-risk baseline and works today; the pipe write is a strictly better UX and needs no elevation, since
the GUI that performs the identical call runs `asInvoker` (`DesktopEdge/app.manifest:20`).

### 4. DNS and addressing

**How the name resolves.** The tunneler builds its DNS table from the `intercept.v1` configs of services the
identity is authorized to dial. On Windows, ZDEW runs a nameserver at TUN\_IP+1 (default `100.64.0.2`) and
installs NRPT rules per intercept domain via `Add-DnsClientNrptRule`; teardown is visible in
`ZitiDesktopEdge.Client/Server/ServiceActions.cs:96-101`
(`Get-DnsClientNrptRule | Where { $_.Comment.StartsWith('Added by ziti-edge-tunnel') } | Remove-DnsClientNrptRule`).
A query for `my-branch.docpreview.ziti` gets an answer from that nameserver pointing at a synthetic IP inside
the tunneler's DNS CIDR; the browser connects there; the tunneler dials the ziti service and proxies bytes.

**Wildcards work, and the IP is allocated lazily per hostname.** In the Go tunneler,
`ziti@v1.6.0/tunnel/intercept/iputils.go:111-123`:

```go
// handle wildcard domain - IPs will be allocated when matching hostnames are queried
if hostname[0] == '*' {
    err := resolver.AddDomain(hostname, func(host string) (net.IP, error) {
        return getDnsIp(host, addrCB, svc, resolver)
    })
    …
}
```

with the resolver side at `ziti@v1.6.0/tunnel/dns/server.go:163-193` (`AddDomain`/`RemoveDomain`, keyed on the
domain suffix). One constraint: wildcards require the DNS-server resolver, which is the normal ZDEW /
`ziti-edge-tunnel` mode. The hostfile resolver rejects them outright — `tunnel/dns/file.go:54`,
`"cannot add wildcard domain[%s] to hostfile resolver"`. The C tunneler that ZDEW embeds supports the same thing — the NetFoundry docs state the
`intercept.v1` addresses list accepts "hostnames, wildcard domains, and IP subnets", and there is a NetFoundry
walkthrough specifically titled "Wildcard DNS with OpenZiti using Ziti Desktop Edge for Windows". One
documented limitation: for wildcard domains, non-A/AAAA queries proxied to the hosting tunneler are limited to
MX, SRV and TXT — irrelevant here.

**Because the tunneler is a layer-4 proxy, the HTTP `Host` header survives end to end.** The browser sends
`Host: my-branch.docpreview.ziti`; nothing rewrites it; docpreview's overlay listener sees it verbatim. That is
what makes the wildcard model work.

**Recommendation: one wildcard service, docpreview routes by Host.** Service-per-PR is the wrong shape:

- Every new service triggers a service-list update and a DNS/NRPT rule churn on **every** connected tunneler.
  A busy repo would have ZDEW rewriting NRPT rules continuously.
- Each PR would need a config, a service, a Bind policy, and a SERP — four management-API objects per PR, four
  deletions per reap, and four ways to leak.
- ZDEW's identity tile lists services; twenty PRs means twenty entries scrolling past the reviewer.
- With `sdk-golang`, one bound service means one `Context.Listen` and one `http.Server`, versus one listener
  per PR.

The wildcard model creates **zero** management-API objects per preview. Publishing a preview becomes "register
`my-branch` in an in-process map"; reaping becomes "delete the map entry". The single `intercept.v1` for
`*.docpreview.ziti` port 80 covers every preview that will ever exist, and the tunneler allocates a synthetic
IP the first time each name is queried.

The one thing you give up is per-preview authorization: everyone with the `docpreview-reader` attribute can
reach every preview. If per-PR access control is ever needed, do it in the HTTP handler (the ziti listener can
report the dialing identity — `edge.Conn`/`Identifiable` carries it) rather than by multiplying services.

### 5. Where this lives in docpreview

**It fits behind `Exposer` cleanly on the hosting side, and needs one new thing beside it on the identity
side.**

The hosting side is nearly the zrok implementation with the zrok bits deleted.
`sdk-golang@v1.2.8/ziti/ziti.go:122,1362` gives `Listen(serviceName string) (edge.Listener, error)`, and
`edge.Listener` embeds `net.Listener` (`ziti/edge/conn.go:68-77`). So the same "serve directly into the mesh,
bind no local port" property that `internal/expose/zrok.go:166-184` relies on holds here.

The one structural difference from every existing exposer: with one wildcard service there is a **single**
listener and a single `http.Server` for the whole process, and `Publish` mutates a routing table instead of
creating a listener. That is still comfortably inside the interface:

```go
type Ziti struct {
    cfg config.ZitiConfig
    log *slog.Logger

    ctx      ziti.Context   // from ziti.NewContextFromFile(cfg.IdentityFile)
    listener edge.Listener  // ctx.Listen(cfg.ServiceName), opened once in Validate
    srv      *http.Server   // Handler: the mux below

    mu   sync.Mutex
    live map[string]http.Handler // host label -> preview handler
}

func (z *Ziti) Kind() string { return "ziti" }

// Validate: load identity, confirm the service is visible and bindable, open the
// listener, start the server. Reaching the controller here satisfies the
// "Validate must reach the remote" contract for free.

func (z *Ziti) Publish(ctx context.Context, spec Spec, h http.Handler) (*Publication, error) {
    z.mu.Lock()
    z.live[spec.Name] = h          // idempotent on Name by construction
    z.mu.Unlock()
    url := JoinURL("http://"+spec.Name+"."+z.cfg.Domain, spec.BaseURL)
    return NewPublication(url, spec.Name, func() error { z.withdraw(spec.Name); return nil }), nil
}

// ServeHTTP on the shared mux: strip the port, take the first label of r.Host,
// look it up in live, 404 with a useful body if absent.

// Reap: delete map entries whose preview ID is not in keep. No remote objects
// exist per preview, so there is nothing on the controller to collect.
```

Config, alongside `zrok2`/`frontdoor` in `internal/config/config.go:42-48`:

```yaml
exposer:
  kind: ziti
  ziti:
    identity_file: /etc/docpreview/docpreview.json   # docpreview's own enrolled identity
    service: docpreview
    domain: docpreview.ziti
    name_template: "{{.Name}}"
    # management API, only needed for the identity verb and bootstrap
    management_api: https://ziti-controller:1280
    reader_role: docpreview-reader
```

**Yes, it wants a new CLI verb.** `cmd/docpreview/main.go` dispatches with a plain switch (`:44-66`), so this
is additive:

- `docpreview identity add <name> [-o file.jwt]` — create the identity with `enrollment.ott = true` and the
  `docpreview-reader` role attribute, read back `detail.Enrollment.Ott.JWT`, write it to a file or stdout.
- `docpreview identity list` / `docpreview identity remove <name>` — the obvious companions; without `remove`
  there is no revocation story.
- `docpreview identity bootstrap` — one-shot creation of the config, service, Bind/Dial policies and SERP
  described in §2a, so an operator does not hand-build six objects.

Management-API credentials belong in the existing vault (`internal/vault`, same treatment as
`vault.KeyFrontdoorToken` at `cmd/docpreview/main.go:229`), not in the YAML.

A nicety worth considering: `docpreview identity add` could, on Windows, offer to write the JWT straight to
`\\.\pipe\ziti-edge-tunnel.sock` as described in §3, turning enrollment into one command with no file
handling. Keep the file output as the default and the pipe write behind a flag — the pipe is an internal
contract of a component docpreview does not own.

## Open questions

- **NetFoundry v2 create-endpoint exact body and poll semantics.** The shape in §2b comes from NetFoundry's
  docs and `python-netfoundry`, not from a run against a tenant; the 202-then-poll behaviour in particular
  wants confirming. One `curl` against a real org, or reading the live spec at
  `https://gateway.production.netfoundry.io/core/v2/docs/index.html`, settles it. Whether
  `/core/v3/endpoints` exists is likewise unknown — no public v3 docs, and unauthenticated probes 401.
- **The DACL on `\\.\pipe\ziti-edge-tunnel.sock`.** ZDEW sets the DACL only on its *monitor* pipe
  (`IPCServer.cs:76-79`); the data pipe is created by `ziti-edge-tunnel.exe`, whose source is in
  `openziti/ziti-tunnel-sdk-c`, not in this tree. Every indication is that a non-elevated user can write to it
  — the GUI that does exactly that runs `asInvoker`, and `TunnelInstanceDiscovery.EnumerateAsync`
  (`:98`) connect-probes pipes from the same context — but this is inferred. Settled by one line on a real
  install: `(Get-Acl \\.\pipe\ziti-edge-tunnel.sock).Access`.
- **Which OpenZiti network.** docpreview has no controller today and zrok cannot lend one (§1). Is the
  intention to stand up a self-hosted controller + router, or to use a NetFoundry-hosted network? This is the
  real gate on the whole feature, and it is a decision, not a research finding.
- **`.ziti` as a TLD.** `*.docpreview.ziti` never resolves publicly, which is the point, but it also means the
  name is dead on any machine without the tunneler running — including the machine that renders the PR
  comment's link preview. Worth confirming nobody's tooling chokes on an unresolvable link. An alternative is
  a real subdomain the org owns (`*.preview.internal.example.com`) intercepted by the tunneler, which
  degrades to NXDOMAIN rather than an invalid TLD.
- **HTTPS or not.** Over the overlay the transport is already mutually authenticated and encrypted, so plain
  HTTP on port 80 inside the tunnel is defensible. But browsers increasingly treat non-HTTPS origins as
  second-class (no service workers, mixed-content warnings on embedded assets), and Docusaurus sites are
  static enough that it probably does not bite. Unverified.
- **Multiple ZDEW identities and DNS collisions.** If a reviewer has other identities with overlapping
  intercept domains, NRPT rule precedence decides. Not investigated.

## Effort

Rough sizing, assuming an OpenZiti network already exists and docpreview has admin credentials for it.

| Piece | Size |
|---|---|
| `internal/expose/ziti.go` — context, single listener, Host-header mux, `Exposer` methods | ~250 lines, 1-2 days. It is `local.go` plus a ziti listener; the hard part (serving into an overlay listener) is already proven in `zrok.go`. |
| `internal/config` additions + `buildExposer` case | trivial, under an hour |
| `internal/ziti` management client — auth, create/list/delete identity, read OTT JWT | ~200 lines, 1 day — or much less: `zrok/v2/controller/automation` already wraps every one of these calls and is a dependency today. |
| `docpreview identity {add,list,remove}` | ~150 lines, half a day |
| `docpreview identity bootstrap` — six management objects, idempotent | ~200 lines, 1 day. Idempotency is the fiddly bit: check-then-create on each object. |
| Tests | 1 day. The management client mocks like `frontdoor_test.go` does; the exposer's Host-routing is unit-testable without a network. |
| Docs — `www/docs/exposers.md` section + a runbook alongside `runbooks/zrok2.md` | half a day |
| End-to-end validation against a real controller and a real ZDEW install | 1-2 days, and this is where the surprises will be |

**Total: roughly one to one-and-a-half weeks** for a working, documented implementation, of which the last item
is the only genuinely uncertain part. Optional extras: the named-pipe auto-enroll on Windows (+half a day, and
it needs the DACL question settled first), and per-preview authorization by dialing identity (+1 day).

## Sources

Source trees read: `D:\git\github\openziti\zrok` (v2), zrok v1.1.11 from GitHub,
`D:\git\github\openziti\desktop-edge-win`, and the module cache —
`C:\Users\claude\go\pkg\mod\github.com\openziti\{ziti@v1.6.0,ziti/v2@v2.0.0,sdk-golang@v1.2.8,edge-api@v0.26.48,zrok/v2@v2.0.4}`.
The `edge-api` shapes cited are identical in the cached v0.35.0.

Web:

- [Tunneler config type intercept.v1](https://netfoundry.io/docs/openziti/learn/core-concepts/config-store/config-type-intercept-v1/)
- [Windows tunneler](https://netfoundry.io/docs/openziti/how-to-guides/tunnelers/windows/)
- [Private DNS on Windows using OpenZiti](https://blog.openziti.io/private-dns-on-windows)
- [Wildcard DNS with OpenZiti using ZDEW (video)](https://www.youtube.com/watch?v=1GC7gt3rsrg)
- [Provision a network and service](https://netfoundry.io/docs/platform/api-guides/provision-network/)
- [OpenZiti enrollment](https://netfoundry.io/docs/openziti/learn/core-concepts/security/enrollment/)
- [NetFoundry v2 API docs](https://gateway.production.netfoundry.io/core/v2/docs/index.html)
- [NetFoundry platform authentication](https://netfoundry.io/docs/platform/api-guides/authentication)
- [python-netfoundry](https://github.com/netfoundry/python-netfoundry) — best call-sequence reference for the v2 API
- [Frontdoor OpenAPI reference](https://netfoundry.io/docs/frontdoor/reference/openapi-reference/)
