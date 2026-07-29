# Tunneler-only preview trial

Stands up a throwaway OpenZiti network, provisions everything the `ziti` exposer needs, and leaves you with an
enrolment token to import into Ziti Desktop Edge.

Background and design: [`www/docs/future/ziti-native-previews.md`](../../www/docs/future/ziti-native-previews.md).

These scripts are the hand-rolled stand-in for the `docpreview identity` verbs, which are not built yet. They
assume a Windows host with the `ziti` CLI at `~/.ziti/bin/v2.0.0/ziti.exe` — adjust `ZITI` at the top of each
if yours lives elsewhere.

## Run it

```bash
# 1. Controller + router, natively on this host.
#
#    Native rather than Docker on purpose: the tunneler runs on this same
#    machine, so advertising "localhost" avoids port publishing,
#    host.docker.internal, and advertised-address problems entirely.
bash demo/ziti-trial/up.sh          # leave running

# 2. The six bootstrap objects, docpreview's hosting identity, and a
#    reviewer identity. Idempotent — safe to re-run.
bash demo/ziti-trial/bootstrap.sh

# 3. Only if you want to run the integration tests. A separate identity,
#    because an OTT enrolment token is single-use and enrolling the
#    reviewer's here would consume the one meant for the tunneler.
bash demo/ziti-trial/test-client-identity.sh
```

Then point docpreview at it:

```yaml
exposer:
  kind: ziti
  ziti:
    identity_file: "C:\\temp\\tangent\\replace-vercel\\ziti-trial\\docpreview-host.json"
    service: docpreview-svc
    domain: docpreview.ziti
```

```powershell
docpreview doctor -config <that file>
docpreview preview -config <that file> -name my-branch .\www\build
```

Import `reviewer-alice.jwt` into Ziti Desktop Edge (**+** → Add Identity from JWT), enable the identity, and
open `http://my-branch.docpreview.ziti/`.

Turn the tunneler off and the hostname stops resolving. That is the whole point.

## Integration tests

```bash
export DOCPREVIEW_ZITI_HOST_IDENTITY='…\docpreview-host.json'
export DOCPREVIEW_ZITI_READER_IDENTITY='…\test-reader.json'
go test ./internal/expose/ -run Ziti -v
```

They skip without those variables, so `go test ./...` stays green on a machine with no network.

The one that matters is `TestZitiRoutesByHostHeader`: three previews, three hostnames, **one service**, correct
content each time. The whole one-wildcard-service design rests on the tunneler being a layer-4 proxy so the
`Host` header survives — that test measures it rather than assuming it.

## Notes

Enrolment tokens expire in about three hours. Re-run `bootstrap.sh` for a fresh one; it clears the previous
objects first.

Everything lands under `D:worktrees	angentsercel-replacementdemoziti-trial\`. Deleting that directory and stopping
`up.sh` removes all trace of the trial.
