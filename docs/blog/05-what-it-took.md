# What a documentation preview actually takes

The pitch is four verbs. A pull request arrives, you clone it, build it, publish it.

A weekend gets you that. What follows is the set of problems that turned up between the demo working and the
thing being safe to leave running, because those are the interesting ones and none of them were visible at the
start.

## Publishing is destructive, and cancelling cannot save you

A reviewer fixing typos pushes five commits in two minutes. Building all five wastes four builds, so a newer
push cancels the build already running for that pull request.

Cancellation is where it gets subtle.

Publishing a preview means taking a name, and taking a name means withdrawing whatever currently holds it. So a
build that was superseded, and reaches the publish step anyway, does not merely waste work. It tears down the
**newer** preview and replaces it with older content.

The obvious guard is to check "am I still the current build?" before publishing. That check is a race unless the
check and the publish are one atomic step, which is what a per-preview lock gives you.

You cannot solve it with cancellation instead. The zrok SDK call that creates a share takes no context, so a
publish already in flight cannot be stopped. Anything relying on cancellation to protect a publish is relying on
something that does not exist. This is why the guard is a lock and not a context test.

There is a second half. A superseded build must not report. Without that rule, a build cancelled halfway
finishes late, fails, and overwrites a comment that already says "ready" with a failure, for a preview sitting
there working perfectly.

## A name is an object with its own lifetime

zrok v2 changed the model in a way that suits this problem exactly, once you stop reading v1's documentation.

In v1 a stable address meant a reserved *share*, and the address was tied to that share. Rebuild it and the URL
churned. In v2, names live in a namespace independently of any share, and a share binds to one.

That is the primitive a preview system wants. Attach the name `my-feature-branch` to a fresh share on every
rebuild, and the URL somebody bookmarked on Monday still resolves on Thursday. Without it, updating one comment
in place would be pointless, because the link inside it would change every time.

The consequence to design around: a name outlives the share bound to it, and it is also the object your account's
quota counts. Something has to delete it when the preview is gone for good, and it must be exactly one caller.
Three places in this code close a share and only one of them is a teardown, so releasing the name from the wrong
one rehosts every rebuilt preview at a new address while the pull request comment advertises the old one.

There is a test named after that rule, which is the only reason it stays true.

## Restarting should not cost four minutes of 404s

The first version deleted every remote share at startup and recreated them from the database, reasoning that
anything left over belonged to a process that no longer exists.

That reasoning is true of a **listener** and false of a **share**. A listener dies with its process. A share is a
record on somebody else's controller and outlives you happily.

The bill for the difference is measured in controller round trips, and for a handful of previews it runs to
minutes, during which every preview URL 404s and no queued build can start.

What actually needs recreating is the listener, and a listener can be bound to an existing share by its token. So
startup adopts what the database already claims and only reaps what it does not recognise. A restart with an
unchanged database now costs seconds.

The ordering is load-bearing and easy to get backwards: reap first, then republish. Reversed, the reap deletes
what the republish just restored.

## A successful build can produce a broken site

Docusaurus bakes its base URL into every generated `href` and `src` at build time. Build for `/my-project/` and
serve at `/`, and `index.html` returns 200 while every stylesheet and script 404s. You get unstyled text and an
empty build log, because as far as the build was concerned it worked.

So before publishing, docpreview reads the built `index.html` and works out which base URL the site was actually
built for.

The obvious way to do that is wrong in both directions. Take the longest common prefix of the absolute
references, and one hand-written `href="/"` in a footer collapses it to `/` and hides a real mismatch, while a
site whose assets all live under `/assets/` reports its base URL as `/assets/`.

Counting works better. Tally the **first path segment** of every absolute reference. A root-mounted site scatters
across `/assets`, `/img`, `/docs` and `/blog`, and no segment dominates. A site built for `/my-project/` puts
every reference under one. If a single segment accounts for 60% or more, that is the base URL. Stray links in
either direction do not move the answer.

When it disagrees with where the preview is about to be mounted, the publish is refused and the error names both
values and the two ways to fix it.

## Two things Docker does that nobody mentions

**A bind mount is slow enough to change the design.** Running `npm ci` into a directory bind-mounted from the
host took 5m46s. The same install into an anonymous volume took 14 seconds. Package installs are thousands of
small file operations, which is the worst case for a mount translating every one of them. So `node_modules` gets
a volume and the workspace gets the mount.

**A container writes as root.** The build runs as root inside the container, so every file it produces is
root-owned on the host, and the next thing that tries to clean up cannot. The fix is to hand ownership back at
the end of the build script.

The fix has a trap in it. Chaining that step with `;` swallows the build's exit status and every failed build
reports success. Capture the status, do the cleanup, exit with what you captured.

## The credential rule that shapes everything else

Build output goes into a pull request comment. That is the most public place a leaked credential can land, and a
clone URL carries a token.

So every byte of git output is scrubbed, and the scrubber is compiled from the credential **values** rather than
their names. That is what makes a build printing its own environment produce asterisks instead of a token.

Which yields a rule with teeth: injecting a secret and rebuilding the scrubber have to be one operation. Any code
path that adds a value to the build environment without adding it to the scrubber is one `set -x` away from
publishing a live credential.

One more, worth stealing. Userinfo in a URL ends at the **last** `@`, not the first. A Bitbucket credential
authenticates with an email address as the username, so splitting on the first `@` redacts the username and emits
the token.

## The through line

Nearly every problem here came from the same place: something that behaves one way locally and another way once
it is a network resource with a lifetime of its own. A share that outlives its process. A name that outlives its
share. A publish that cannot be cancelled. A path that means something different inside a container.

None of it is visible in the four verbs. All of it is the difference between a demo and a thing you leave
running.

The code, and the design documents arguing with it, are at
[github.com/openziti-test-kitchen/docpreview](https://github.com/openziti-test-kitchen/docpreview).
