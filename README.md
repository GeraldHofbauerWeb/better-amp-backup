# better-amp-backup

Incremental, deduplicating backups for [CubeCoders AMP](https://cubecoders.com/AMP) instances.

AMP's built-in backup writes a complete ZIP of the entire instance on every run.
On a real Minecraft server that means the same gigabytes are re-read,
re-compressed and re-written every hour, whether or not anything changed.

`amp-bb` reads only what actually changed since the last snapshot.

## Measured on a live instance

A production AMP 2.8 instance running a NeoForge 1.21.1 modpack:

|                                  | AMP's backup       | amp-bb              |
| -------------------------------- | ------------------ | ------------------- |
| Bytes written per hourly snapshot | 9.7 GiB            | **0 B** when idle   |
| Disk held for 14 snapshots        | 67 GiB             | ~1.3 GiB            |
| Time per snapshot                 | minutes            | **0.15 s**          |
| Server held with saves disabled   | full staging copy  | **0 ms** when idle  |

Every snapshot still describes the *complete* instance, not a delta: any one of
them can be restored on its own.

## How it stays this cheap

- **The quiesce window covers the delta, not the copy.** The cold parts of an
  instance — mods, configs, jars — are read while the server runs normally.
  Only the paths Minecraft rewrites in place are read between `save-off` and
  `save-on`, and of those only the ones a `stat()` diff says changed. On an idle
  hour that is nothing at all.
- **Content-addressed storage.** Files are addressed by their BLAKE3 hash and
  compressed with zstd, skipping payloads that are already compressed. Identical
  content is stored once, no matter how many snapshots reference it.
- **No filesystem features required.** No btrfs, no ZFS, no reflinks, no root.
  It works the same on the ext4 that most AMP hosts actually run.

## Safety

A backup tool earns trust by being boring about the dangerous parts:

- Every restored file is re-hashed and checked against the snapshot. A mismatch
  aborts the restore instead of writing plausible-looking garbage.
- `save-on` is guaranteed by a watchdog independent of the main code path. A
  crashed backup must never leave a server with saving disabled.
- A run refuses to start if it would fill the disk, and says what to exclude.
- AMP's own `Backups/` directory, its file-manager trash, logs, locks and
  rendered map tiles are excluded by default — the things that make a naive
  first run enormous.
- The `save-on` that re-enables saving is also sent by a watchdog running
  independently of the backup, so a wedged or crashed run cannot leave a server
  unable to save. It is sent again on daemon startup, in case a previous
  process died holding a quiesce.
- If the server does not confirm the flush, the run fails. A snapshot of a
  half-written region file is worse than no snapshot.

## Usage

Against a stopped instance, or one you are happy to read while it runs:

```sh
export AMPBB_REPO=/srv/backups/amp-bb
amp-bb init
amp-bb backup --instance MyServer --root /home/amp/.ampdata/instances/MyServer
amp-bb snapshots
amp-bb restore <snapshot> --target /tmp/verify
amp-bb verify  <snapshot> --target /tmp/verify
amp-bb stats
```

Housekeeping, in the order it is safe to do it:

```sh
amp-bb check --read-data     # prove the repository is intact first
amp-bb forget                # report what a retention policy would drop
amp-bb forget --apply        # move those snapshots to the trash
amp-bb prune                 # report what could then be deleted
amp-bb prune --force --empty-trash
```

Against a **running** server, let AMP hold it still for the world files:

```sh
export AMPBB_AMP_URL=http://127.0.0.1:8080
export AMPBB_AMP_USER=backup
export AMPBB_AMP_PASSWORD_FILE=/etc/better-amp-backup/amp.password

amp-bb doctor --instance MyServer      # check this before trusting anything
amp-bb backup --instance MyServer --quiesce
```

With `--quiesce`, the run sends `save-off`, then `save-all flush`, then **waits
for the server to confirm in its console** before reading a single world file,
and sends `save-on` afterwards. It does not sleep and hope. With `--amp-url`
set, `--root` can be omitted: the instance directory is looked up through the
API rather than guessed from a path that varies between AMP versions and
datastores.

Give the tool its own AMP account with a read-only-ish role rather than the
admin login, and put the password in a file rather than the environment.

AMP's own per-directory `.backupExclude` files are honoured by default, so
exclusions curated for AMP carry over without being rewritten.

A caveat worth knowing, measured on AMP 2.8.0.4: on a Minecraft instance AMP
itself no longer reads those files. Its backup plugin builds an internal
exclusion map instead, written through the panel's file manager. So a
`.backupExclude` you place by hand shapes what `amp-bb` stores, but does not
shrink AMP's own ZIPs.

## Running it on a schedule

`deploy/` installs amp-bb as a systemd timer, with the service running as the
`amp` user rather than root — it needs no more access than the instances it
reads already have.

```sh
sudo deploy/install.sh --instance MyServer \
     --amp-url http://127.0.0.1:8082 \
     --root /home/amp/.ampdata/instances/MyServer
```

The script prints what is left to do: the password file, `init`, a `doctor`
run, one backup by hand, and only then `systemctl enable --now
amp-bb@MyServer.timer`. It never overwrites an existing configuration and does
not enable the timer for you.

Give the tool's AMP account exactly three permissions, no more:

| Permission | Where to set it |
| ---------- | --------------- |
| `Core.AppManagement.ReadConsole` | inside the instance |
| `Core.AppManagement.SendConsoleInput` | inside the instance |
| `Instances.<guid>.Manage` | in the controller |

The first two must be set from *inside* the instance's own role management. Set
from the controller they apply to every instance on the host. The third is what
lets the account log in to the instance at all: an AMP instance does not
authenticate on its own, it asks the controller, and the controller refuses
unless the role may manage that specific instance.

## Status

Early. v0.1 is read-only with respect to your AMP installation: it reads instance
directories and writes only to its own repository. It never writes into an
instance, never touches AMP's `Backups.json`, and ships no deleting operations.

Not yet implemented: restoring directly into a live instance, materialising
snapshots into AMP's own Backups tab, a scheduler daemon, and S3 targets.

## Building

```sh
CGO_ENABLED=0 go build -o amp-bb ./cmd/amp-bb
go test ./... -race
```

## Licence

MIT
