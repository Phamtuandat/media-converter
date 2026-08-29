# macOS v2 rollout

This directory contains templates only. It does not unload or load any
LaunchAgent and does not change the live Hermes endpoint.

The v2 data boundary is:

```text
~/Library/Application Support/media-converter-v2/
  bin/  staging/  state/  cache/  work/  mnt/
```

The converter writes only to `mnt/media-converter-v2/artifacts/`. The mount is
provided by the separate writable WebDAV gateway template
`deploy/media-converter-mac-v2-writer.service`; existing `rclone-webdav.service`
and `caddy-webdav.service` are not modified.

## Shadow test

1. Build `go build -o "$HOME/Library/Application Support/media-converter-v2/bin/media-converter-v2" ./cmd/media-converter-v2`.
2. Install both wrapper scripts, create the six data directories, copy the env example, and set mode `600` on the env file.
3. Configure `WEBDAV_MOUNT_URL` for the new writable gateway; the macOS
   wrapper uses the native `/sbin/mount_webdav` client and keeps the mount
   alive until the LaunchAgent is stopped.
4. Replace `100.94.97.52:8080` with a shadow address such as `100.94.97.52:18080` in the env file and validate `plutil -lint` on both shadow plists.
5. Bootstrap `com.datpham.media-converter-v2.webdav-mount` and then `com.datpham.media-converter-v2` in the GUI launch domain.
6. Verify readiness is `200`, upload image/video/audio fixtures, poll jobs, validate direct WebDAV downloads, and compare downloaded SHA-256 values.
7. Stop the shadow service and verify no artifact was created under the legacy local output root.

## Baseline and backup

Record health, Hermes smoke results, the old binary checksum, the old plist,
the configured roots, and local artifact count/bytes. Save the old binary,
plist, and metadata outside the v2 data root. Never print the token in the
baseline log.

## Cutover

1. Confirm the old queue is drained and the old job count is stable.
2. Confirm the new writable WebDAV gateway and Mac mount are healthy.
3. Stop the experimental VPS converter WebDAV writer, if it is still present; do not restart or stop `rclone-webdav.service` or `caddy-webdav.service`.
4. Unload the old `com.datpham.media-converter` LaunchAgent.
5. Load `com.datpham.media-converter.cutover.plist`, whose label is the same old label and whose env binds `100.94.97.52:8080`.
6. Immediately run the existing Hermes upload/job/download smoke test.
7. Monitor readiness, errors, latency, completions, direct-download success, and hash mismatches. Confirm only one converter owns port `8080`.

## Rollback

Unload the v2 cutover plist, restore the backed-up old plist and binary, then
load `com.datpham.media-converter` again. On macOS, restore the binary with
`ditto --rsrc --extattr all` and verify it with `codesign --verify`; if launchd
rejects an ad-hoc-signed copy, re-sign that restored rollback copy with
`codesign --force --sign -` before loading the plist. Verify `/health/ready`
before sending traffic. Do not delete v2 artifacts, old artifacts, state, or backups during
rollback.
