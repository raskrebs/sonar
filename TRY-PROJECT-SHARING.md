# Trying project sharing on a real project

Fifteen minutes. It runs a second daemon on a socket of its own, so the daemon
your app and editor are using is never touched and nothing here is
uninstallable — when you are done, kill one process.

**It works against the relay that is deployed right now.** The redeploy is
still outstanding and is *not* a prerequisite: a project share is one share
row to the relay, with the routing done entirely by the daemon on your
machine. The relay never learns the shape of your project, which is the
property the whole design is built on.

---

## 1. Build the branch

```sh
cd ~/Development/personal/repos/sonar-wt/group
go build -o /tmp/sonar-next .
/tmp/sonar-next version     # prints: sonar dev (darwin/arm64)
```

## 2. Make a wrapper, so the socket cannot be forgotten

The test daemon listens on a socket of its own, which is what keeps your
normal daemon untouched. Rather than exporting a variable and having to
remember it in every shell, put it in the command itself:

```sh
cat > /tmp/sn <<'EOF'
#!/bin/sh
exec env SONAR_SOCKET=/tmp/sonar-next.sock /tmp/sonar-next "$@"
EOF
chmod +x /tmp/sn
```

From here on, **`/tmp/sn` is the test build and `sonar` is your normal one**.
Any terminal, any tab, no variable to lose.

Check it reached the right daemon before going further:

```sh
/tmp/sn share --list
```

- `Nothing is shared from this machine.` — correct, carry on.
- `unknown method share.list` — you reached your normal daemon, which is
  v0.9.1 and predates sharing entirely. The wrapper is not being used.

## 3. Start the test daemon

```sh
/tmp/sn serve --detach
/tmp/sn daemon status      # version should read `dev`
```

If it says **`daemon already running`**, that is a real quirk and not your
mistake: `serve` refuses when *any* daemon is running, even on a different
socket. Kill just the test one and retry:

```sh
pkill -f "sonar-next serve"
```

Never `sonar daemon stop` — that would stop the daemon your app is using.

It signs in as you already are: the account lives in the keychain, not in the
daemon, so there is no second sign-in.

## 4. Go to a real project

`aka-ai-platform` is the one that prompted this feature. It needs a committed
`sonar.yaml`, because a project share is refused without one:

```sh
cd <the project>
ls sonar.yaml || /tmp/sn init
/tmp/sn list
```

Check the list shows the services running and which ports they are on. If the
frontend and the API are not both up, start them as you normally would.

## 5. Share it

```sh
/tmp/sn share <the frontend's port> --public
```

**What should happen.** Before anything is published, it notices the port
belongs to a project and asks:

```
localhost:6873 is part of aka-ai-platform, which also runs:
  /            web
  /_sonar/api  api
               db (does not speak HTTP)

Share the whole project? [Y/n]
```

Say yes. You get a URL and the table again underneath it.

Two things to check in that prompt, because they are the decisions you made:

- **the database is listed as left out**, by name, with the reason
- **the frontend is at `/`** and everything else is under `/_sonar/`, rather
  than at `/api` where it could shadow a page the frontend already serves

## 6. Point the frontend at the API — and keep localhost working

This is the one step nobody can do for you, and the whole point of the
feature. It is also the step with a catch, so read the catch first.

**The catch.** If your frontend holds an absolute URL —
`VITE_API_URL: http://localhost:9700` or similar — a visitor's browser cannot
reach it, because they have no localhost:9700. The page will load and then
every API call will fail with a CORS error naming `http://localhost:...`,
which looks like a routing bug and is not one: the request never reached the
share at all.

**And a relative path alone is not enough.** Setting `VITE_API_URL=/_sonar/api`
fixes the share and breaks plain `localhost:6873`, because the dev server would
then serve that path itself rather than the API. You need the dev server to
proxy it too.

So there are two halves, and both are one-time:

**a. Give the service the path its application already expects.** If the API
mounts itself at `/api`, say so in `sonar.yaml` and turn the strip off, so it
is asked for the path it is already routing:

```yaml
services:
  - name: backend
    path: /api
    strip: false
```

Check first that the frontend has no route of its own at that path — this is
the shadowing the default `/_sonar/...` exists to avoid, and taking the short
path means taking responsibility for it.

**b. Make the frontend's base relative, and proxy it in dev.** Remove the
absolute URL so requests go to whatever host served the page:

```yaml
env:
  VITE_API_URL: ""
```

and teach the dev server the same path, so localhost behaves like the share:

```js
// vite.config.ts
server: {
  proxy: {
    "/api": { target: "http://backend:8000", changeOrigin: true },
  },
},
```

Restart the frontend. Now one configuration is correct on localhost and
through every share, which is the state worth getting to.

## 7. Open it on a phone

Over cellular, not the wifi — wifi can mask a problem by reaching your laptop
directly.

What to try, in order of how much it tells you:

| Try | What it proves |
| --- | --- |
| The page loads | the entry service is routed |
| Click into a page the frontend routes itself | unknown paths fall through, which is what an SPA needs |
| **The login button** | the frontend reached the API — the thing that was broken |
| Anything that streams or live-updates | WebSockets survive the routing |
| A page the frontend serves that shares a name with a service, e.g. `/admin` | the reserved prefix is doing its job |

## 8. Stop

```sh
/tmp/sn share --stop
pkill -f "sonar-next serve"
rm -f /tmp/sn
```

`pkill` rather than `sonar daemon stop`, so only the test daemon goes. Yours
was running throughout and is unaffected.

---

## What is worth reporting back

Not "it worked" — that I can see from the tests. What I cannot see:

- **Where the prompt's wording is wrong or unclear.** It is the first thing
  anyone meets and it is the part I have the least evidence about.
- **Whether `/_sonar/api` is tolerable to type and read**, or whether it looks
  like machinery leaking into your project. The override exists (`path:` in
  `sonar.yaml`) — did you reach for it?
- **Anything that 404s or 502s** — with the path, which tells me whether it is
  the table or the service.
- **Whether the frontend built an absolute URL somewhere** and broke. This is
  the failure mode I most expect and least tested: a framework that constructs
  `https://host/api/...` from a base rather than using the relative path.
  `X-Forwarded-Prefix` is set for exactly this, and some frameworks read it and
  some do not.

## Known gaps, so you do not report them as bugs

- `sonar share <project-name> --public` — the non-interactive spelling — is
  not wired yet. Only sharing a port and answering the prompt works.
- The prompt never appears with `--json` or when stdin is not a terminal. That
  is deliberate: a script gets exactly what it typed.
- A service that is declared but not running is left out and listed as "not
  running". That is not a failure.
