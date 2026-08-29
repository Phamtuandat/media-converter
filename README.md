# Media Converter Service

Local Go media processing service for canonical image/video artifacts. The VPS client reaches the service through the Tailscale private network and authenticates with a Bearer token.

## Runtime boundary

The service accepts artifact identities, never arbitrary filesystem paths or URLs. It detects media from content, normalizes images to JPEG, and normalizes/remuxes/transcodes videos to MP4. It does not publish to Zalo or contain album/business logic.

## Run

The service requires an explicit Tailscale bind address and application token:

```sh
export LISTEN_ADDR="100.x.y.z:8080"
export MEDIA_SERVICE_TOKEN="change-me"
export ARTIFACT_INPUT_ROOT="/private/staging"
export ARTIFACT_OUTPUT_ROOT="/private/media-artifacts"
export LEGACY_ARTIFACT_ROOT="/private/legacy-media-artifacts" # optional read-only rollback retention
export JOB_STATE_ROOT="/private/media-state"
export CACHE_ROOT="/private/media-cache"
export WORK_ROOT="/private/media-work"
export PUBLIC_BASE_URL="http://100.x.y.z:8080"
export MEDIA_OUTPUT_MODE="local"
export POLICY_VERSION="media-v1.1"
./media-converter serve
```

Bind `LISTEN_ADDR` to the local host's Tailscale address (typically `100.x.y.z:8080`). The VPS reaches this address over the Tailscale private network; the converter does not expose a public Internet listener. Keep `MEDIA_SERVICE_TOKEN` in the host's secret/environment configuration and send it as a Bearer token from the VPS.

The host must provide `ffmpeg`, `ffprobe`, and ImageMagick `magick` with HEIC/HEIF, AVIF, and WebP delegates. The service is not ready until all required tools/delegates and storage paths are usable.

## macOS v2 cutover

The independent v2 executable is built from `./cmd/media-converter-v2` and is
packaged with the macOS LaunchAgent and mount templates in `deploy/macos/`.
During cutover it keeps the existing `MEDIA_SERVICE_TOKEN`, binds the existing
`100.94.97.52:8080` address, and preserves all V1 endpoints. Its state and
staging roots are separate from the old service, while `LEGACY_ARTIFACT_ROOT`
is read-only compatibility access for old local artifacts during rollback
retention.

For WebDAV mode, `ARTIFACT_OUTPUT_ROOT` must be the mounted
`mnt/media-converter-v2/artifacts` path. The service never creates that path
when the mount is absent, returns `503` readiness, rejects new uploads/jobs,
and never falls back to a local output directory. New job download URLs are
the configured WebDAV base plus the returned filename; clients must use them
verbatim.

## API flow

```text
POST /v1/artifacts
POST /v1/jobs
GET  /v1/jobs/{job_id}
GET  /v1/jobs/{job_id}/downloads
GET  /v1/artifacts/{artifact_id}
```

Job and artifact endpoints require:

```http
Authorization: Bearer <media-service-token>
```

Upload source bytes first with `Content-Type` and `X-Filename` headers. The service returns a generated staging `artifact_id`, SHA-256, size, and declared MIME; headers are metadata only and format detection happens during job processing. `POST /v1/jobs` returns `202 Accepted`; the client polls the job and downloads committed artifacts by artifact ID. Artifact downloads support HTTP Range requests.

To retrieve service-hosted download URLs after conversion, set `policy.include_download_urls` to `true` when creating the job, then call `GET /v1/jobs/{job_id}/downloads`. The endpoint returns `202` while the job is still processing and `200` with output/thumbnail URLs after completion. URLs use `PUBLIC_BASE_URL` when configured; otherwise they are relative paths. Downloads remain protected by the Bearer token and are available while the artifact is retained by `ARTIFACT_RETENTION` (14 days by default).

## Legacy VPS WebDAV rollout template

The existing service keeps `MEDIA_OUTPUT_MODE=local` and is unchanged. The
Linux templates below describe the earlier isolated VPS instance and are kept
for rollback/reference only; they are not the Mac v2 binary or the Mac v2
writable gateway. The Mac cutover uses `./cmd/media-converter-v2` and
`deploy/macos/`.

```sh
export LISTEN_ADDR="100.x.y.z:8081"
export MEDIA_SERVICE_TOKEN="separate-token"
export MEDIA_OUTPUT_MODE="webdav"
export ARTIFACT_INPUT_ROOT="/var/lib/media-converter-webdav/staging"
export ARTIFACT_OUTPUT_ROOT="/mnt/media-webdav/media-converter-v2/artifacts"
export JOB_STATE_ROOT="/var/lib/media-converter-webdav/state"
export CACHE_ROOT="/var/lib/media-converter-webdav/cache"
export WORK_ROOT="/var/lib/media-converter-webdav/work"
export PUBLIC_BASE_URL="http://100.x.y.z:8081"
export WEBDAV_PUBLIC_BASE_URL="https://files.example.test/media-converter-v2/artifacts"
```

`MEDIA_OUTPUT_MODE=webdav` requires `WEBDAV_PUBLIC_BASE_URL`. The URL is used
verbatim as the direct download base; the service does not infer it from an
artifact ID or fall back to the converter URL. On Linux, the output path must
be inside a non-root mounted filesystem. The service checks the mount and a
create/delete probe before reporting ready, and checks the mount again before
output writes, reads, cleanup, and downloads. If the mount disappears, the
instance becomes not ready and does not write to a local directory beneath the
mount point.

The client rollout is intentionally independent: keep the existing client
backend flag at `MEDIA_CONVERTER_BACKEND=legacy`, then enable a separate
`webdav` backend only for selected workers/users/jobs. Clients should use the
`download_url` returned by `GET /v1/jobs/{job_id}/downloads` exactly as given;
legacy jobs continue to use the converter artifact endpoint. Turning the flag
off returns traffic to the old instance without database or artifact migration.

Deployment templates for the second instance are in `deploy/`. They reference
the existing WebDAV services only through ordering and require a separate
mount path; they do not modify or restart those services.

The supported V1.1 audio policies are:

```text
voice_ogg_opus  -> OGG/Opus, 48 kHz, mono
wav_pcm_s16le   -> WAV/PCM s16le, 16 kHz, mono
m4a_aac_lc      -> M4A/AAC-LC, 48 kHz, mono
```

## Service Discovery

The VPS can discover the stable service contract through the Tailscale address:

```sh
curl http://<tailscale-host>:8080/.well-known/media-converter.json
curl http://<tailscale-host>:8080/v1/capabilities
curl http://<tailscale-host>:8080/health/ready
curl http://<tailscale-host>:8080/openapi.json
```

The `.well-known` document describes stable identity, endpoints, authentication, the asynchronous job workflow, supported media and canonical outputs. `/v1/capabilities` reports current runtime tool/delegate support and configured limits, while `/health/ready` answers whether the service can accept jobs now. OpenAPI is the complete machine-readable API schema. Discovery and capabilities do not expose tokens, filesystem paths, or raw tool diagnostics.

The authenticated `/metrics` endpoint exposes process-local Prometheus text metrics. Health endpoints do not require authentication by default:

```text
GET /health/live   # process loop is alive
GET /health/ready  # storage, workspace, tools and job manager are usable
GET /metrics       # requires Bearer token
```

Readiness is expected to return `503` until `ffmpeg`, `ffprobe`, ImageMagick `magick`, and the HEIC/HEIF, AVIF, and WebP delegates are installed and all configured storage roots are writable. Startup logs report tool availability and versions but never expose token values or media/EXIF contents.

The daemon uses a bounded in-process queue. Tune `IMAGE_WORKERS`, `VIDEO_WORKERS`, `JOB_WORKERS`, `QUEUE_SIZE`, `FFMPEG_THREADS`, and the input/output/resource/time limits for the local machine. Video concurrency should remain conservative because FFmpeg consumes CPU, RAM, and disk I/O. A full queue returns `429` with a retryable error.

On `SIGINT`/`SIGTERM`, the service stops readiness and intake, shuts down HTTP, waits up to `SHUTDOWN_TIMEOUT`, terminates remaining converter process groups, and cleans temporary workspaces on the next cleanup pass. Job state, results, cache entries, and committed artifacts are stored under the configured local roots so queued jobs can be recovered after restart.

For a long-running host, run `media-converter serve` under the host's service manager with the five data roots on private persistent storage. Do not grant the process access to arbitrary paths; `artifact_id` values resolve only inside `ARTIFACT_INPUT_ROOT`, and returned artifacts are streamed by ID through `/v1/artifacts/{artifact_id}`.

## Checks

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
go test -race ./...
```
