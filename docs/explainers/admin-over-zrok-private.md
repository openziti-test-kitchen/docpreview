# Managing NetFoundry Preview over zrok private

NetFoundry Preview has two kinds of pages. The **dashboard** shows builds, previews and logs. The **admin pages**
edit projects and store credentials, and they decide what runs on the build host and with which secrets.

Until now, reaching the admin pages on a remote install meant an ssh tunnel to `127.0.0.1`. That works, but it
means shell access to the box for anyone who manages it.

## What changes

The daemon can now serve itself over a **zrok private share**. You add one listener to the config:

```yaml
listeners:
  - tcp: "127.0.0.1:8471"
  - zrok_private:
      token: nfpadmin
```

On your own machine you run `zrok2 access private nfpadmin`, open the address it prints, and sign in. With the
admin password you get the whole daemon, admin pages included. With the viewer password you get it read-only.

## Why this is safe

- **The share is closed.** Only the zrok account that owns the install, and any accounts you list in
  `access_grants`, can open it. Nobody else can even reach the login page.
- **The login still applies.** Getting through zrok gets you to the sign-in form, not past it. The admin password
  is the admin password everywhere.
- **Public stays public-only.** The public dashboard still goes out through `nfpreview dashboard-only`, which only
  forwards the dashboard's own routes. Every admin page and admin API answers 404 there, even for someone signed in
  as admin. We tested exactly that.

## In one line

zrok public shows the dashboard to the world, and zrok private gives operators the whole daemon. Previews don't
change.
