#!/usr/bin/env bash
#
# Install amp-bb as a systemd timer on an AMP host. Run as root:
#
#   sudo deploy/install.sh --instance SebsModpackv401 \
#        --amp-url http://127.0.0.1:8082 \
#        --root /home/amp/.ampdata/instances/SebsModpackv401
#
# Add --with-web to install the daemon that serves the panel tab and keeps the
# schedule itself. It needs the controller's URL and the instance's GUID:
#
#   sudo deploy/install.sh --instance SebsModpackv401 ... --with-web \
#        --panel-url https://games2.example.net \
#        --amp-instance-id 6b85d8b3-eaff-4722-ac97-52e4f4441948
#
# Idempotent: existing configuration and the password file are never
# overwritten. Nothing is enabled here -- do that once doctor and a first
# manual run have both come back clean. nginx is not touched at all; the
# fragment to add is printed instead.
set -euo pipefail

PREFIX=/opt/better-amp-backup
CONF=/etc/better-amp-backup
REPO=$PREFIX/repo
BINARY=${BINARY:-./amp-bb}
AMP_USER=${AMP_USER:-amp}
INSTANCE= AMP_URL= ROOT= AMP_ACCOUNT=backup
WITH_WEB=0 PANEL_URL= INSTANCE_ID= LISTEN=

die() { echo "error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case $1 in
    --instance)    INSTANCE=$2; shift 2;;
    --amp-url)     AMP_URL=$2;  shift 2;;
    --root)        ROOT=$2;     shift 2;;
    --amp-user)    AMP_ACCOUNT=$2; shift 2;;
    --binary)      BINARY=$2;   shift 2;;
    --with-web)    WITH_WEB=1;  shift 1;;
    --panel-url)   PANEL_URL=$2; shift 2;;
    --amp-instance-id) INSTANCE_ID=$2; shift 2;;
    --listen)      LISTEN=$2;   shift 2;;
    -h|--help)     sed -n '2,24p' "$0" | sed 's/^# \?//'; exit 0;;
    *)             die "unknown argument: $1";;
  esac
done

[[ -n $INSTANCE ]] || die "--instance is required"
[[ -n $AMP_URL  ]] || die "--amp-url is required"
[[ -n $ROOT     ]] || die "--root is required"
[[ $EUID -eq 0  ]] || die "run this as root"
[[ -x $BINARY   ]] || die "no amp-bb binary at $BINARY (pass --binary)"
id "$AMP_USER" >/dev/null 2>&1 || die "user $AMP_USER does not exist"
[[ -d $ROOT ]] || die "instance directory $ROOT does not exist"
if [[ $WITH_WEB -eq 1 ]]; then
  [[ -n $PANEL_URL ]] || die "--with-web needs --panel-url (the controller, not the instance)"
  [[ -n $INSTANCE_ID ]] || die "--with-web needs --amp-instance-id (the instance's AMP GUID)"
fi

install -d -m 0755 "$PREFIX/bin"
install -d -m 0750 -o "$AMP_USER" -g "$AMP_USER" "$REPO"
install -d -m 0750 "$CONF"
install -m 0755 "$BINARY" "$PREFIX/bin/amp-bb"

printf 'AMPBB_REPO=%s\n' "$REPO" > "$CONF/common.env"
chmod 0644 "$CONF/common.env"

PASSWORD_FILE=$CONF/$INSTANCE.password
if [[ ! -f $CONF/$INSTANCE.env ]]; then
  cat > "$CONF/$INSTANCE.env" <<ENV
AMPBB_AMP_URL=$AMP_URL
AMPBB_AMP_USER=$AMP_ACCOUNT
AMPBB_AMP_PASSWORD_FILE=$PASSWORD_FILE
# Set outright rather than resolved through the API: the lookup method belongs
# to the controller, not to an instance, and needs wider permissions.
AMPBB_ROOT=$ROOT
ENV
  chmod 0644 "$CONF/$INSTANCE.env"
fi
[[ -f $PASSWORD_FILE ]] || install -m 0640 /dev/null "$PASSWORD_FILE"

# The directory and the password belong to root and are readable by the group
# the service runs as: it must read its password, and must not change it.
# 0600 would be wrong here -- the group would get nothing.
chown -R root:"$AMP_USER" "$CONF"
chmod 0750 "$CONF"
chmod 0640 "$CONF"/*.password

WEB_ENV=$CONF/$INSTANCE.web.env
if [[ $WITH_WEB -eq 1 && ! -f $WEB_ENV ]]; then
  cat > "$WEB_ENV" <<ENV
AMPBB_PANEL_URL=$PANEL_URL
AMPBB_INSTANCE_ID=$INSTANCE_ID
${LISTEN:+AMPBB_LISTEN=$LISTEN}
ENV
  chmod 0644 "$WEB_ENV"
fi

install -m 0644 "$(dirname "$0")"/systemd/amp-bb-web@.service /etc/systemd/system/
install -m 0644 "$(dirname "$0")"/systemd/amp-bb@.service \
                "$(dirname "$0")"/systemd/amp-bb@.timer \
                "$(dirname "$0")"/systemd/amp-bb-retention@.service \
                "$(dirname "$0")"/systemd/amp-bb-retention@.timer \
                /etc/systemd/system/
systemctl daemon-reload

cat <<NEXT

Installed $("$PREFIX/bin/amp-bb" --version) for instance $INSTANCE.

The AMP account "$AMP_ACCOUNT" needs, on this instance only:
  Core.AppManagement.ReadConsole       read the console
  Core.AppManagement.SendConsoleInput  send save-off / save-all / save-on
and, in the controller's copy of its role:
  Instances.<instance-guid>.Manage     permission to log in to the instance
Grant the first two from inside the instance, not from the controller, or
they apply to every instance on the host.

Still to do:
  1. printf %s 'THEPASSWORD' > $PASSWORD_FILE
  2. sudo -u $AMP_USER env AMPBB_REPO=$REPO $PREFIX/bin/amp-bb init
  3. sudo -u $AMP_USER bash -c 'set -a; . $CONF/common.env; . $CONF/$INSTANCE.env; set +a; \\
       $PREFIX/bin/amp-bb doctor --instance $INSTANCE --root "\$AMPBB_ROOT"'
  4. systemctl start amp-bb@$INSTANCE.service     # once, by hand
NEXT

if [[ $WITH_WEB -eq 1 ]]; then
  cat <<WEBNEXT
  5. systemctl enable --now amp-bb-web@$INSTANCE.service
  6. wire up nginx -- see deploy/nginx/README.md. In short:
       mkdir -p /etc/nginx/amp-bb.d && cp deploy/nginx/amp-bb.conf /etc/nginx/amp-bb.d/
       then, inside the panel vhost's server{} block:
         include /etc/nginx/amp-bb.d/*.conf;
       and inside its location / { }:
         proxy_set_header Accept-Encoding "";
         sub_filter '</body>' '<script src="/Plugins/AmpBB/Loader.js" defer></script></body>';
         sub_filter_once on;
         sub_filter_types text/html;

The daemon keeps the schedule itself, so the timers are replaced rather than
joined. Once it is running:

  systemctl disable --now amp-bb@$INSTANCE.timer amp-bb-retention@$INSTANCE.timer

The oneshot services stay installed on purpose: they are the way to take a
backup by hand when the daemon is down.
WEBNEXT
else
  cat <<TIMERNEXT
  5. systemctl enable --now amp-bb@$INSTANCE.timer

Or, for the panel tab and a schedule you can change from a browser, run this
again with --with-web --panel-url ... --amp-instance-id ...
TIMERNEXT
fi

if [[ $WITH_WEB -eq 1 ]] && ! nginx -V 2>&1 | grep -q http_sub_module; then
  cat <<WARN

warning: this nginx was built without --with-http_sub_module, so the sidebar
entry cannot be injected. Everything else works; the interface will be at
$PANEL_URL/amp-bb/ and you will have to bookmark it.
WARN
fi
