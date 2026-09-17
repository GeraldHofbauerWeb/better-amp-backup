# Putting the tab in AMP's sidebar

Two things have to happen: nginx has to route `/amp-bb/` and `/Plugins/AmpBB/`
to the daemon, and one script tag has to be injected into the panel's own page
so that AMP's plugin loader picks us up.

## Why an injection at all

AMP's frontend already has everything needed. `PluginHandler.LoadPluginAsync`
fetches `/Plugins/<name>/Plugin.js`, evaluates it, and registers whatever tabs
it declares through `UI.AddSideMenuItem` — the same call AMP's own sidebar
entries go through. It never calls it for us only because the list of plugins
to load comes from the backend, and a backend plugin has to be signed by
CubeCoders.

So the tab is a real AMP tab. The injection is one line that asks the loader to
load it.

## 1. Route to the daemon

`install.sh --with-web` does this for you: it writes
`/etc/nginx/amp-bb.d/amp-bb-<instance>.conf` from the template in
`deploy/nginx/amp-bb.conf`, with the socket path filled in. By hand it would
be:

```
sudo mkdir -p /etc/nginx/amp-bb.d
sed 's#@SOCKET@#/run/better-amp-backup/<instance>.sock#' \
    deploy/nginx/amp-bb.conf | sudo tee /etc/nginx/amp-bb.d/amp-bb-<instance>.conf
```

Then add one line inside the `server { ... }` block of the panel's vhost — on a
CubeCoders install that is `/etc/nginx/conf.d/<panel host>.conf`:

```
include /etc/nginx/amp-bb.d/*.conf;
```

## 2. Inject the loader

In the same `server` block, inside the `location / { ... }` that proxies to
AMP:

```
    # sub_filter cannot rewrite a compressed response, and the failure is
    # silent: the page arrives intact and the tab simply never appears.
    proxy_set_header Accept-Encoding "";

    sub_filter '</body>' '<script src="/Plugins/AmpBB/Loader.js" defer></script></body>';
    sub_filter_once on;
```

`sub_filter_types` is deliberately absent: `text/html` is always filtered, and
naming it again makes nginx warn about a duplicate MIME type.

The panel's page has exactly one `</body>` and loads all its scripts with
`defer`, so an appended `defer` script runs after AMP's own initialisation and
after `PluginHandler` exists.

Check the module is there before relying on any of this:

```
nginx -V 2>&1 | tr ' ' '\n' | grep http_sub_module
```

Then `sudo nginx -t && sudo systemctl reload nginx`.

## The socket

The daemon does not create its own socket. systemd does, as root, with the
group nginx runs as, and hands the daemon the open descriptor -- so the daemon
keeps running as `amp` and nginx gets access to that one socket and nothing
else.

The obvious alternative, putting nginx into the `amp` group, would also have
given it `/etc/better-amp-backup/*.password`, which is `root:amp 0640`. That is
why this is worth a socket unit.

If nginx on your distribution runs as something other than `www-data`, pass
`--nginx-group` to the installer.

## What breaks, and what happens when it does

`sub_filter` into somebody else's panel is not a supported integration, and an
AMP update can regenerate that vhost and take the `include` with it.

When that happens the sidebar entry disappears and nothing else does. The
interface stays reachable at `https://<panel host>/amp-bb/`, the daemon keeps
its schedule, and backups carry on. Keeping the additions in their own file
means putting them back is one line rather than a merge.
