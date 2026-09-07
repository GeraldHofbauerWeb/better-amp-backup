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
- `forget` only removes snapshot metadata. Objects are deleted exclusively by
  `prune`, which is a separate, explicit step.

## Usage

```sh
amp-bb --repo /srv/backups/amp-bb init
amp-bb --repo /srv/backups/amp-bb backup \
    --instance SebsModpackv401 \
    --root /home/amp/.ampdata/instances/SebsModpackv401

amp-bb --repo /srv/backups/amp-bb snapshots
amp-bb --repo /srv/backups/amp-bb restore <snapshot> --target /tmp/verify
amp-bb --repo /srv/backups/amp-bb verify  <snapshot> --target /tmp/verify
amp-bb --repo /srv/backups/amp-bb stats
```

AMP's own per-directory `.backupExclude` files are honoured by default, so
exclusions curated for AMP carry over without being rewritten.

## Status

Early. v0.1 is read-only with respect to your AMP installation: it reads instance
directories and writes only to its own repository. It never writes into an
instance, never touches AMP's `Backups.json`, and ships no deleting operations.

Not yet implemented: console quiescing through the AMP API, retention and
pruning, restoring directly into an instance, materialising snapshots into AMP's
own Backups tab, and S3 targets.

## Building

```sh
CGO_ENABLED=0 go build -o amp-bb ./cmd/amp-bb
go test ./... -race
```

## Licence

MIT
