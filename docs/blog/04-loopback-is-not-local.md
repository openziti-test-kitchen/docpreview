# Loopback is not local

Here is a check that appears in a lot of code, and it looks like a security boundary.

```go
if !isLoopback(r.RemoteAddr) {
    http.Error(w, "local requests only", http.StatusForbidden)
    return
}
```

The reasoning behind it is sound as far as it goes. Anybody who can reach `127.0.0.1` on this machine can
already run the binary, read its config file and open its database. An HTTP endpoint restricted to loopback
hands them nothing a shell did not already give them.

Then somebody starts a tunnel, and the check keeps returning true while the endpoint is on the internet.

## What the tunnel does

Tools like zrok, ngrok and Cloudflare Tunnel publish a local service without opening a firewall port. The
common form takes an origin:

```
zrok2 share public http://127.0.0.1:8471
```

Two things about that command matter more than they appear to.

**It shares an origin, not a route.** Every path the service serves goes with it. In our case that meant the
dashboard, the previews, and the credential API, all reachable from the public internet at once.

**In proxy mode the tunnel process runs on the same host.** It accepts the connection from outside, then makes
its own connection to `127.0.0.1:8471`. So the server sees a connection from a local process, and `RemoteAddr`
is loopback, because it truthfully is.

The request came from the internet. The address is `127.0.0.1`. Both are correct.

The listener is still bound to loopback too, so a check on the listener's address agrees. Every signal available
at that layer says local, and every one of them is right, and the conclusion drawn from them is wrong.

For us the consequence was specific: with the vault unlocked, `PUT`, `DELETE` and a credential-generating
endpoint would all have succeeded for anyone holding the share URL.

## Why no smarter check fixes it

The temptation is to look harder at the request. There is nothing to find. `Host` is whatever the client sent.
`RemoteAddr` is the tunnel. The TLS terminated somewhere else. At the point the handler runs, a request that
travelled the internet and a request from a script on the same box are the same bytes from the same address.

What actually closes it is not one test but two that fail independently.

**A property of the daemon.** Every listener must be loopback. This catches a service bound to `0.0.0.0`, and
it refuses outright when a listener is an overlay listener, because "enrolled on the network" is not
authorization to write credentials.

**A property of the request.** Loopback `RemoteAddr` **and** no `X-Forwarded-For`, `X-Forwarded-Host`,
`X-Real-Ip` or `Forwarded` header.

The second one is what covers the tunnel, and it works for an unglamorous reason: anything proxying to a service
sets one of those headers, and a caller who adds one themselves only makes the check stricter. It costs nothing
and it fails safe.

It is still a boundary rather than authentication. It assumes nothing proxies to the daemon while stripping
forwarding headers. That assumption holds for everything we ship and might not hold for a proxy you put in
front, so the honest description is "this is where the edge is", not "this is authenticated".

Which is why the credential surface eventually grew a real password as well. A boundary and a credential answer
different questions, and only one of them survives somebody rearranging the network.

## The structural fix

The check is the small half. The real repair is not putting the admin surface and the public route on the same
origin in the first place.

So the thing we expose is a separate process that forwards exactly one path, `POST /webhook/github`, and refuses
a non-loopback upstream, because forwarding anywhere else would make it a relay. Given a reserved name it binds
**no local TCP port at all** — its listener is the overlay share, so there is nothing on `127.0.0.1` for anything
else on the machine to find either.

It does not verify the webhook signature. That is the daemon's job, using the secret from its vault, and
duplicating it would mean a second copy of the secret in a second process. This is a router. The guard is one
hop further in.

The general shape: when a service has both a public route and a privileged one, the way to keep them apart is a
process boundary, not a conditional. A conditional is one refactor away from being wrong, and it fails silently
when it is.

## What to take from it

If you have a loopback check standing between the internet and something that matters, ask what happens when
somebody runs a tunnel at it. Not whether they are allowed to. Whether the check still returns the answer you
meant.

The answer is usually that it returns true, cheerfully, forever, and nothing in any log looks unusual — because
from the daemon's point of view nothing unusual happened.

Three questions worth asking of any check of this shape:

1. Does it distinguish "arrived from this machine" from "arrived at this machine"? Those are different facts and
   only one of them is observable.
2. Does it survive a proxy? If a forwarding header would change the answer, test for the header.
3. Is the privileged thing on the same origin as the public thing? If it is, the check is load-bearing, and
   load-bearing checks should be boundaries you can see rather than conditions you can forget.

docpreview is at [github.com/openziti-test-kitchen/docpreview](https://github.com/openziti-test-kitchen/docpreview).
The reasoning above lives beside the code that implements it, which is the only place it stays true.
