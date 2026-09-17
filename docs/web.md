# The panel tab

`amp-bb serve` runs a daemon that does two things: it keeps the backup
schedule, and it serves an interface that appears as a tab in AMP's own
sidebar, beside AMP's own Backups entry.

```
sudo deploy/install.sh --instance MyServer \
     --amp-url http://127.0.0.1:8082 \
     --root /home/amp/.ampdata/instances/MyServer \
     --with-web \
     --panel-url https://panel.example.net \
     --amp-instance-id 6b85d8b3-eaff-4722-ac97-52e4f4441948
```

Wiring nginx up is a separate, manual step, described in
[`deploy/nginx/README.md`](../deploy/nginx/README.md). The installer prints the
fragment rather than editing the panel's vhost: that file is AMP's, and an
installer that silently rewrites somebody's reverse proxy is one nobody trusts
twice.

## Why there is a daemon at all

Changing how often a backup runs has to work from a browser. The service runs
as the `amp` user; it cannot rewrite a systemd timer or ask systemd to reload
one, and giving it the ability to would undo the reason it runs unprivileged.

So it keeps its own clock. The cadence lives in
`/var/lib/better-amp-backup/settings.json` and the last run in `state.json`
beside it. The combination is what `Persistent=true` gave the timer for free: a
restart neither loses an hour nor fires immediately, and a host rebooting in a
loop does not back up in a loop.

The two oneshot units stay installed. They are how you take a backup by hand
when the daemon is down, and the way back if any of this turns out to be a bad
idea:

```
systemctl disable --now amp-bb-web@MyServer.service
systemctl enable  --now amp-bb@MyServer.timer amp-bb-retention@MyServer.timer
```

The installer also writes a per-instance drop-in naming the instance
directory, because systemd does not expand environment variables in
`ReadWritePaths=` — it reports "path is not absolute, ignoring" and carries on,
and the symptom would be a restore failing with `EROFS` months later. If you
move an instance, run the installer again rather than editing the unit.

## Who may do what

There are no accounts. A backup tool that grows a user database is how a backup
tool becomes a breach. Whoever is signed into the panel is signed into this,
and what they may do here is derived from what AMP says they may do there.

The derivation uses a property of AMP worth knowing about: `Core.GetAPISpec` is
filtered by the caller's permissions, so the spec doubles as a capability list.

| Here | Granted when |
| --- | --- |
| browse snapshots, read settings, back up now | the panel session is valid and not anonymous |
| stop, start the instance | the spec lists the method, i.e. AMP would allow it |
| restore, change settings, forget, prune | the session may both stop **and** start |

The last row is a judgement worth stating plainly: somebody who can stop and
start the instance can already destroy its state by other means, so putting the
destructive amp-bb operations behind the same bar adds no exposure that was not
already there. An account that can only read the console gets a read-only view
and a "back up now" button, neither of which can lose data.

Two identities are in play and they stay separate:

- **The service account** — the narrow one in the password file, with
  `ReadConsole`, `SendConsoleInput` and `Instances.<guid>.Manage` and nothing
  else. It takes the scheduled backups and quiesces the server. It deliberately
  cannot start or stop anything.
- **Your own panel session** — used for what you click. Stopping the server
  from the banner runs as you, not as the service account, which is why the
  service account never needed that permission.

AMP 2.8 has no call that names the session's own user, so the name shown beside
a change is the one the panel reported and is labelled unverified. It decides
nothing; it only labels a record somebody will read later.

## Restoring

Two targets, and never a path from the browser — an absolute path in a request
body is a write primitive handed to anyone who can reach the panel.

- **Into a scratch copy**, under the state directory. Works while the server is
  running. This is what the dialog offers first.
- **Into the live instance**, which is refused unless the instance is fully
  stopped. Not "not ready": starting, stopping, restarting, configuring and
  pending-user all mean a process may still hold those files open.

A restore into the instance takes a snapshot of the current state first, tagged
`pre-restore`. The default retention policy keeps that tag for ever, so the
state you replaced is recoverable afterwards. It then dry-runs, logs the
counts, and only then writes. Every restored file is re-hashed and compared
against the snapshot as it lands; a mismatch aborts the restore rather than
writing plausible-looking garbage.

It does not start the server afterwards. An automatic start hides a failed
restore behind a server that boots and then behaves strangely.

## Selecting part of a snapshot

Ticking boxes sends paths, not patterns. The pattern language has no escape for
its metacharacters, and a modpack is full of names like `[1.21.1] Some Mod.jar`
— there is no glob that selects that file and only that file. A pattern without
a slash also matches at any depth, so ticking a top-level `mods` would quietly
drag in `config/foo/mods`.

The browser sends the minimal cover: a fully ticked directory is one path, not
thirty thousand. Selecting more than ten thousand paths is refused with a
suggestion to tick the parent instead.

## The event stream

Progress arrives over server-sent events on `/amp-bb/api/events`. Two things
matter operationally:

- nginx must not buffer it. The fragment sets `proxy_buffering off` and the
  daemon sends `X-Accel-Buffering: no`; without either, a working daemon looks
  dead.
- The stream replays. Every event carries a sequence number, the browser
  reconnects with `Last-Event-ID`, and a reconnect from further back than the
  buffer reaches is told to start over rather than handed a torn prefix.

## When AMP's own backups are also running

The status page warns when AMP still has a backup task scheduled for the same
instance. Two schedules backing up one instance is the most expensive
misconfiguration available here — AMP writes a complete archive every time,
where amp-bb writes only what changed.

It is read and reported, nothing more. Switching AMP's schedule off is done in
AMP's own Schedule tab.
