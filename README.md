# Media Converter Service

Local-first Go service for turning uploaded images, videos, and audio files into
canonical artifacts. The service is designed to run on a private host and be
called by another application over a Tailscale or private network connection.

Repository: https://github.com/Phamtuandat/media-converter

## Contents

- [What this service does](#what-this-service-does)
- [Processing contract](#processing-contract)
- [Architecture](#architecture)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Build targets](#build-targets)
- [Configuration reference](#configuration-reference)
- [Storage and lifecycle](#storage-and-lifecycle)
- [API](#api)
- [Policies](#policies)
- [WebDAV output mode](#webdav-output-mode)
- [Long-running deployments](#long-running-deployments)
- [Operations](#operations)
- [Security guidance](#security-guidance)
- [Troubleshooting](#troubleshooting)
- [Development checks](#development-checks)

## What this service does

- Accepts source bytes through an authenticated HTTP API.
- Assigns an artifact ID and stages the source on local persistent storage.
- Detects media from file contents. Filename extensions and declared MIME types
  are metadata only and are not treated as authoritative.
- Processes one or more staged artifacts asynchronously as a job.
- Normalizes images to JPEG.
- Normalizes, remuxes, or transcodes videos to MP4.
- Normalizes audio into one of the supported canonical audio profiles.
- Stores job state, outputs, thumbnails, and cache entries on disk.
- Exposes health, runtime capabilities, discovery, OpenAPI, and Prometheus
  metrics endpoints.

The service intentionally does not contain album management, business logic,
Zalo publishing, or arbitrary filesystem-path/URL processing. Callers identify
media only with artifact IDs returned by the service.

## Processing contract

| Input kind | Supported inputs | Canonical output |
| --- | --- | --- |
| Image | JPEG, PNG, WebP, HEIC, HEIF, AVIF | JPEG, .jpg, image/jpeg |
| Video | MOV, QuickTime, MP4, M4V | MP4, .mp4, H.264, yuv420p, AAC when audio is present, faststart enabled |
| Audio | OGG, WAV, M4A | Selected audio policy below |

Supported audio policies:

| Policy | Container | Codec | Sample rate | Channels | MIME |
| --- | --- | --- | ---: | ---: | --- |
| voice_ogg_opus | OGG | Opus | 48 kHz | 1 | audio/ogg |
| wav_pcm_s16le | WAV | PCM signed 16-bit little-endian | 16 kHz | 1 | audio/wav |
| m4a_aac_lc | M4A | AAC-LC | 48 kHz | 1 | audio/mp4 |

Video jobs can also produce a JPEG thumbnail when
policy.generate_thumbnail is enabled.

## Architecture

The request lifecycle is:

~~~text
Client
  |
  | POST /v1/artifacts (raw source bytes)
  v
Staging storage
  |
  | POST /v1/jobs (artifact IDs + policy)
  v
Persistent job state -> bounded in-process queue -> worker
                                      |
                                      v
                           detect -> process -> validate
                                      |
                                      v
                              committed artifacts
~~~

The service uses external media tools rather than Go media libraries:

- ffmpeg performs video and audio processing.
- ffprobe inspects video/audio streams and validates media metadata.
- ImageMagick magick processes images and checks image format delegates.

### Repository layout

~~~text
cmd/
  media-converter/          Compatibility executable.
  media-converter-v2/       Independent v2 executable.

internal/
  app/                      Shared process startup and shutdown.
  cache/                    Filesystem-backed result cache.
  config/                   Environment loading and validation.
  detection/                Content-based image/media detection.
  domain/                   API and job domain models.
  httpapi/                  HTTP handlers, discovery, and OpenAPI.
  job/                      Queue, workers, recovery, and cleanup.
  mediaaudio/               Audio conversion and validation.
  mediaimage/               Image conversion and validation.
  mediavideo/               Video conversion, probing, and thumbnails.
  observability/             Metrics.
  processor/                External tool execution and hashing.
  state/                    Persistent job state.
  storage/                  Staging, committed artifacts, and workspaces.

deploy/
  macos/                    macOS v2 LaunchAgent and WebDAV templates.
  *.service                 Linux systemd templates for the WebDAV rollout.
  *.env.example             Example environment files.
~~~

## Requirements

- Go 1.25 or newer.
- ffmpeg.
- ffprobe.
- ImageMagick 7 or another installation that provides the magick command.
- ImageMagick delegates for HEIC/HEIF, AVIF, and WebP.
- A writable persistent location for staging, output, state, cache, and work
  data.
- A loopback, private, or Tailscale address for the listening interface.

The default configuration accepts loopback and private/Tailscale listen
addresses, but rejects unspecified addresses. Use 127.0.0.1 when all clients
run on the same host; otherwise use a literal private/Tailscale IP such as
192.168.1.20 or an address in the 100.64.0.0/10 range. Do not use 0.0.0.0.

### Install tool examples

macOS with Homebrew:

~~~sh
brew install go ffmpeg imagemagick
~~~

Debian or Ubuntu:

~~~sh
sudo apt-get update
sudo apt-get install -y ffmpeg imagemagick
~~~

Install Go 1.25 separately if the distribution package is older. Some older
Linux ImageMagick packages provide convert but not magick; install ImageMagick
7 or set MAGICK_PATH to an executable that supports the identify -list format
check used by the service.

Verify the tools before starting the service:

~~~sh
go version
ffmpeg -version
ffprobe -version
magick -version
magick identify -list format | grep -Ei 'HEIC|HEIF|AVIF|WEBP'
~~~

The final command must show the required image delegates. The service reports
503 Service Unavailable from /health/ready until the tools, delegates, and
configured storage roots are usable.

## Quick start

### 1. Clone the repository

~~~sh
git clone https://github.com/Phamtuandat/media-converter.git
cd media-converter
~~~

### 2. Configure local output mode

The following example stores all runtime data below the repository data/
directory. The data/ directory is ignored by Git and must be provisioned again
on each new machine.

Replace 100.64.0.10 with the machine's actual private or Tailscale address.

~~~sh
export LISTEN_ADDR="100.64.0.10:8080"
export MEDIA_SERVICE_TOKEN="$(openssl rand -hex 32)"
export MEDIA_OUTPUT_MODE="local"

export MC_DATA_ROOT="$PWD/data"
export ARTIFACT_INPUT_ROOT="$MC_DATA_ROOT/staging"
export ARTIFACT_OUTPUT_ROOT="$MC_DATA_ROOT/artifacts"
export JOB_STATE_ROOT="$MC_DATA_ROOT/state"
export CACHE_ROOT="$MC_DATA_ROOT/cache"
export WORK_ROOT="$MC_DATA_ROOT/work"

mkdir -p "$ARTIFACT_INPUT_ROOT" "$ARTIFACT_OUTPUT_ROOT" \
  "$JOB_STATE_ROOT" "$CACHE_ROOT" "$WORK_ROOT"
~~~

PUBLIC_BASE_URL is optional. Set it when clients need absolute download URLs:

~~~sh
export PUBLIC_BASE_URL="http://100.64.0.10:8080"
~~~

### 3. Run the v2 executable

For development, run directly from the source tree:

~~~sh
go run ./cmd/media-converter-v2 serve
~~~

For a built binary:

~~~sh
go build -o ./media-converter-v2 ./cmd/media-converter-v2
./media-converter-v2 serve
~~~

Both executables accept only the serve command. Configuration is read from the
environment when the process starts; the binary does not load a .env file
automatically.

### 4. Check health and discovery

In a second terminal, export the same address:

~~~sh
export MC_BASE_URL="http://100.64.0.10:8080"

curl -fsS "$MC_BASE_URL/health/live"
curl -i "$MC_BASE_URL/health/ready"
curl -fsS "$MC_BASE_URL/.well-known/media-converter.json"
curl -fsS "$MC_BASE_URL/v1/capabilities"
~~~

Expected live response:

~~~json
{"status":"alive"}
~~~

Readiness returns status 200 when the service can accept work. A 503 response
includes a checks object showing which dependency is unavailable.

## Build targets

There are two small command packages sharing the same internal application:

| Package | Output name | Intended use |
| --- | --- | --- |
| ./cmd/media-converter | media-converter | Existing/compatibility deployments |
| ./cmd/media-converter-v2 | media-converter-v2 | Independent v2 deployment and new installs |

Build either target with:

~~~sh
go build -o ./media-converter ./cmd/media-converter
go build -o ./media-converter-v2 ./cmd/media-converter-v2
~~~

The v2 command is the preferred target for a new host. The compatibility
target is kept for existing service-manager configurations.

## Configuration reference

All configuration is read at startup. Environment files used by systemd or
LaunchAgent wrapper scripts are external to the Go process.

### Connection and output mode

| Variable | Required/default | Description |
| --- | --- | --- |
| LISTEN_ADDR | Required | Literal host:port using a loopback, private, or Tailscale address. |
| MEDIA_SERVICE_TOKEN | Required | Bearer token required by artifact, job, download, and metrics endpoints. Keep it outside Git. |
| PUBLIC_BASE_URL | Empty | Absolute HTTP(S) base URL used for local-mode download URLs. No query string or fragment. |
| MEDIA_OUTPUT_MODE | local | Must be local or webdav. |
| WEBDAV_PUBLIC_BASE_URL | Empty | Required in WebDAV mode. Direct public/download base used verbatim with the output filename. |
| WEBDAV_BASE_URL | Compatibility alias | Fallback value for WEBDAV_PUBLIC_BASE_URL. |
| TOOL_FINGERPRINT | Auto-generated | Optional manual tool identity. When empty, it is built from detected tool versions. |

### Storage paths

| Variable | Default | Description |
| --- | --- | --- |
| ARTIFACT_INPUT_ROOT | cwd/data/staging | Raw uploaded artifacts waiting for processing. |
| ARTIFACT_OUTPUT_ROOT | cwd/data/artifacts | Committed canonical outputs. In WebDAV mode this must be the mounted output path. |
| LEGACY_ARTIFACT_ROOT | Empty | Optional read-only compatibility root for old artifacts. It must be separate from input and output roots. |
| LEGACY_ARTIFACT_OUTPUT_ROOT | Compatibility alias | Fallback value for LEGACY_ARTIFACT_ROOT. |
| JOB_STATE_ROOT | cwd/data/state | Persistent job records and recovery state. |
| CACHE_ROOT | cwd/data/cache | Cached conversion results. |
| WORK_ROOT | cwd/data/work | Temporary per-job workspaces. |

cwd is the process working directory. For production, use explicit paths on
private persistent storage rather than relying on the current directory.

### External tools and version metadata

| Variable | Default | Description |
| --- | --- | --- |
| FFMPEG_PATH | ffmpeg | Executable name or path. |
| FFPROBE_PATH | ffprobe | Executable name or path. |
| MAGICK_PATH | magick | ImageMagick executable name or path. |
| PROCESSOR_VERSION | 1.0.0 | Processor version included in job/discovery metadata. |
| POLICY_VERSION | media-v1.1 | Policy version included in job/discovery metadata. |

### Concurrency and queue

| Variable | Default | Description |
| --- | ---: | --- |
| IMAGE_WORKERS | 4 | Maximum concurrent image conversions. |
| VIDEO_WORKERS | 3 | Maximum concurrent video/audio conversions. |
| JOB_WORKERS | 4 | Maximum concurrent job workers. |
| QUEUE_SIZE | 32 | Bounded in-process job queue capacity. A full queue returns 429. |
| FFMPEG_THREADS | 3 | FFmpeg thread setting used by processing commands. |

Video work is CPU, memory, and disk intensive. Increase these values only after
measuring the target machine.

### Media and job limits

| Variable | Default | Description |
| --- | ---: | --- |
| MAX_INPUT_BYTES | 1 GiB | Maximum size of one uploaded/input artifact. |
| MAX_OUTPUT_BYTES | 2 GiB | Maximum size of one committed output. |
| MAX_WIDTH | 12000 | Maximum image/video width. |
| MAX_HEIGHT | 12000 | Maximum image/video height. |
| MAX_PIXELS | 144,000,000 | Maximum pixels for one media item. |
| MAX_DURATION | 30m | Maximum duration for one media item. Uses Go duration syntax. |
| MAX_ITEMS_PER_JOB | 64 | Maximum items in one job. |
| MAX_CONCURRENT_ITEMS_PER_JOB | 4 | Maximum items processed concurrently within one job. |
| MAX_AGGREGATE_INPUT_BYTES | 4 GiB | Aggregate input byte budget for one job. |
| MAX_AGGREGATE_PIXELS | 1,152,000,000 | Aggregate pixel budget for one job. |
| MAX_AGGREGATE_DURATION | 4h | Aggregate duration budget for one job. |
| JOB_TIMEOUT | 45m | Maximum processing time for one job. |

Positive integer values are required for worker, queue, and media-limit
settings. Duration values use Go syntax such as 30s, 45m, or 4h.

### Retention and HTTP timeouts

| Variable | Default | Description |
| --- | ---: | --- |
| WORKSPACE_RETENTION | 24h | Retention for temporary workspace data. |
| STATE_RETENTION | 168h (7 days) | Retention for completed/old job state. |
| CACHE_RETENTION | 168h (7 days) | Retention for cached results. |
| ARTIFACT_RETENTION | 336h (14 days) | Retention for committed artifacts. |
| STAGING_RETENTION | 168h (7 days) | Retention for staged input artifacts. |
| JANITOR_INTERVAL | 1h | Cleanup interval. |
| SHUTDOWN_TIMEOUT | 30s | Grace period for HTTP and worker shutdown. |
| HTTP_READ_TIMEOUT | 30s | HTTP request read timeout. |
| HTTP_WRITE_TIMEOUT | 2m | HTTP response write timeout. |
| HTTP_IDLE_TIMEOUT | 2m | HTTP keep-alive idle timeout. |

## Storage and lifecycle

The default local layout is:

~~~text
data/
  staging/       Raw uploaded input artifacts.
  artifacts/     Committed canonical outputs and thumbnails.
  state/         Persistent job state for polling and recovery.
  cache/         Reusable conversion results.
  work/          Temporary processing workspaces.
~~~

The service:

- Never accepts arbitrary filesystem paths or URLs in a job request.
- Resolves input and output data through validated artifact IDs.
- Hashes input and output data with SHA-256.
- Keeps job state on disk so queued/completed work can survive a restart.
- Runs periodic cleanup using the retention settings.
- Uses LEGACY_ARTIFACT_ROOT only as a separate read-only compatibility source;
  it is never used as a new output location.

The data directory is intentionally not part of the Git repository. Back up
the output and state roots if artifacts and recoverable job history matter.
Cache and work data can generally be rebuilt, but should still be on private
storage while the service is running.

## API

Set a base URL and token before running the examples:

~~~sh
export MC_BASE_URL="http://100.64.0.10:8080"
export MEDIA_SERVICE_TOKEN="the-token-configured-on-the-service"
~~~

### Authentication

The following endpoints require:

~~~http
Authorization: Bearer <media-service-token>
~~~

- POST /v1/artifacts
- POST /v1/jobs
- GET /v1/jobs/{job_id}
- GET /v1/jobs/{job_id}/downloads
- GET /v1/artifacts/{artifact_id}
- GET /metrics

Discovery, capabilities, health, and OpenAPI endpoints do not require the
Bearer token by default:

- GET /.well-known/media-converter.json
- GET /v1/capabilities
- GET /health/live
- GET /health/ready
- GET /openapi.json

### Endpoint summary

| Method | Endpoint | Auth | Purpose |
| --- | --- | --- | --- |
| GET | /.well-known/media-converter.json | No | Stable service and workflow manifest. |
| GET | /v1/capabilities | No | Runtime tools, formats, workers, policies, and limits. |
| GET | /health/live | No | Process liveness. |
| GET | /health/ready | No | Whether the service can accept work. |
| GET | /openapi.json | No | Machine-readable API contract. |
| POST | /v1/artifacts | Yes | Stage raw source bytes. |
| POST | /v1/jobs | Yes | Queue an asynchronous conversion job. |
| GET | /v1/jobs/{job_id} | Yes | Poll a job and retrieve its result. |
| GET | /v1/jobs/{job_id}/downloads | Yes | Retrieve output download URLs when opted in. |
| GET | /v1/artifacts/{artifact_id} | Yes | Stream a committed artifact. Supports HTTP Range. |
| GET | /metrics | Yes | Prometheus text metrics. |

The live OpenAPI document is available from the running service. It should be
treated as the authoritative schema for generated clients.

### 1. Upload an artifact

Upload the source as the raw request body. Content-Type and X-Filename are
required headers:

~~~sh
curl -sS -X POST "$MC_BASE_URL/v1/artifacts" \
  -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN" \
  -H "Content-Type: image/heic" \
  -H "X-Filename: vacation.heic" \
  --data-binary @vacation.heic
~~~

Example response:

~~~json
{
  "artifact_id": "stg-01JEXAMPLE",
  "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "size": 1234567,
  "declared_mime": "image/heic",
  "filename": "vacation.heic",
  "state": "staged"
}
~~~

The returned artifact_id is the only identity that should be sent in a job.
The source is detected from its contents during processing. The filename must
be a safe basename, not a path, and is limited to 255 bytes.

Common upload responses:

| Status | Meaning |
| ---: | --- |
| 201 | Artifact staged successfully. |
| 400 | Missing/invalid headers or unsafe filename. |
| 401 | Missing or invalid Bearer token. |
| 413 | Input exceeds MAX_INPUT_BYTES. |
| 503 | Storage is not ready or cannot write. |

### 2. Create a job

A job references one or more staged artifacts:

~~~sh
curl -sS -X POST "$MC_BASE_URL/v1/jobs" \
  -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN" \
  -H "Content-Type: application/json" \
  --data-binary @- <<'JSON'
{
  "job_id": "demo-photo-001",
  "items": [
    {
      "id": "photo",
      "artifact_id": "stg-01JEXAMPLE",
      "declared_kind": "image"
    }
  ],
  "policy": {
    "target_image": "zalo_jpeg",
    "target_video": "zalo_mp4_h264_aac_faststart",
    "target_audio": "voice_ogg_opus",
    "strip_metadata": true,
    "generate_thumbnail": true,
    "alpha_background": "#FFFFFF",
    "strict_format_match": false,
    "include_download_urls": true
  }
}
JSON
~~~

The service responds with 202 Accepted:

~~~json
{
  "job_id": "demo-photo-001",
  "state": "queued"
}
~~~

The Location response header contains /v1/jobs/demo-photo-001.

Job request fields:

| Field | Required | Description |
| --- | --- | --- |
| job_id | Yes | Safe job identifier, maximum 128 bytes. |
| items | Yes | One to MAX_ITEMS_PER_JOB items. Each item ID must be unique. |
| items[].id | Yes | Safe identifier within the job. |
| items[].artifact_id | Yes | ID returned by artifact upload. |
| items[].declared_kind | No | Optional image, video, or audio hint. |
| items[].expected_sha256 | No | Optional SHA-256 digest checked before processing. |
| policy | No | Conversion policy. Empty or omitted policy uses defaults. |

Idempotency behavior:

- Repeating the same job_id with the same normalized request returns the
  existing job with 200 OK.
- Reusing a job_id for a different request returns 409 Conflict.
- A full queue returns 429 Too Many Requests with a retryable error.

### 3. Poll job status

~~~sh
curl -sS "$MC_BASE_URL/v1/jobs/demo-photo-001" \
  -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN"
~~~

Job states are:

1. queued
2. processing
3. completed

Completed jobs include a result object. The result outcome is one of:

- success
- partial_success
- rejected
- failed

Each result item includes its own status, detected media metadata, operation,
output metadata, optional thumbnail metadata, warnings, and an error when
applicable. Item operations are passthrough, normalized, remuxed, or
transcoded.

Example completed response:

~~~json
{
  "job_id": "demo-photo-001",
  "state": "completed",
  "processor": {
    "version": "1.0.0",
    "policy": "media-v1.1"
  },
  "result": {
    "outcome": "success",
    "items": [
      {
        "id": "photo",
        "status": "success",
        "operation": "normalized",
        "detected": {
          "kind": "image",
          "format": "heic",
          "mime": "image/heic",
          "width": 4032,
          "height": 3024
        },
        "output": {
          "artifact_id": "out-01JEXAMPLE",
          "filename": "photo.jpg",
          "extension": ".jpg",
          "mime": "image/jpeg",
          "size": 456789,
          "sha256": "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
        }
      }
    ]
  }
}
~~~

### 4. Retrieve download URLs

Set policy.include_download_urls to true when creating the job:

~~~sh
curl -sS "$MC_BASE_URL/v1/jobs/demo-photo-001/downloads" \
  -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN"
~~~

Responses:

- 202 Accepted while the job is not completed.
- 200 OK with output and thumbnail download entries after completion.
- 400 Bad Request when URL opt-in was not enabled.
- 404 Not Found when the job does not exist.

Example:

~~~json
{
  "job_id": "demo-photo-001",
  "state": "completed",
  "downloads": [
    {
      "item_id": "photo",
      "output": {
        "artifact_id": "out-01JEXAMPLE",
        "filename": "photo.jpg",
        "mime": "image/jpeg",
        "size": 456789,
        "download_url": "http://100.64.0.10:8080/v1/artifacts/out-01JEXAMPLE"
      }
    }
  ]
}
~~~

In local mode, PUBLIC_BASE_URL produces absolute service URLs; when it is
empty, URLs are relative. In WebDAV mode, the URL is built from
WEBDAV_PUBLIC_BASE_URL and the committed filename. Use a returned download_url
verbatim. Do not reconstruct it from an artifact ID.

### 5. Download an artifact

~~~sh
curl -sS -L "$MC_BASE_URL/v1/artifacts/out-01JEXAMPLE" \
  -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN" \
  -o photo.jpg
~~~

Artifact downloads:

- Return the canonical content type when the extension is known.
- Include Content-Disposition with the stored filename.
- Support Range requests and return 206 Partial Content.
- Require the same Bearer token as the upload and job endpoints.

Example range request:

~~~sh
curl -sS -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN" \
  -H "Range: bytes=0-1023" \
  "$MC_BASE_URL/v1/artifacts/out-01JEXAMPLE" \
  -o first-kilobyte.bin
~~~

## Policies

The default policy is:

~~~json
{
  "target_image": "zalo_jpeg",
  "target_video": "zalo_mp4_h264_aac_faststart",
  "target_audio": "voice_ogg_opus",
  "strip_metadata": true,
  "generate_thumbnail": true,
  "alpha_background": "#FFFFFF",
  "strict_format_match": false,
  "include_download_urls": false
}
~~~

Supported policy values:

| Field | Allowed values/format | Notes |
| --- | --- | --- |
| target_image | zalo_jpeg | Canonical JPEG output. |
| target_video | zalo_mp4_h264_aac_faststart | Canonical H.264/AAC MP4 with faststart. |
| target_audio | voice_ogg_opus, wav_pcm_s16le, m4a_aac_lc | Selects the audio profile. |
| strip_metadata | Boolean | Controls metadata stripping during conversion. |
| generate_thumbnail | Boolean | Enables video thumbnail generation. |
| alpha_background | #RRGGBB | Background used when image alpha must be flattened. |
| strict_format_match | Boolean | Rejects declared-kind or extension mismatches when true. |
| include_download_urls | Boolean | Opts the job into the downloads endpoint. Default false. |

If policy is omitted or is {}, all default values are applied. When a non-empty
policy is supplied, missing target strings and alpha_background are filled in,
but boolean fields should be sent explicitly when their value matters.

The service always detects content before processing. With
strict_format_match=false, a mismatch between a declared kind/extension and
detected content produces a warning and detected content wins. With
strict_format_match=true, the item is rejected.

## WebDAV output mode

WebDAV mode is intended for a mounted writable output filesystem:

~~~sh
export MEDIA_OUTPUT_MODE="webdav"
export ARTIFACT_INPUT_ROOT="/var/lib/media-converter/staging"
export ARTIFACT_OUTPUT_ROOT="/mnt/media-webdav/media-converter-v2/artifacts"
export JOB_STATE_ROOT="/var/lib/media-converter/state"
export CACHE_ROOT="/var/lib/media-converter/cache"
export WORK_ROOT="/var/lib/media-converter/work"
export PUBLIC_BASE_URL="http://100.64.0.10:8081"
export WEBDAV_PUBLIC_BASE_URL="https://files.example.test/media-converter-v2/artifacts"
~~~

WebDAV requirements:

- WEBDAV_PUBLIC_BASE_URL is mandatory.
- ARTIFACT_OUTPUT_ROOT must be inside the intended mounted filesystem.
- The mount must exist and be writable before readiness becomes 200.
- The service performs bounded mount/readiness checks.
- If the mount disappears, readiness becomes 503 and the service does not fall
  back to a local directory beneath the mount point.
- Returned WebDAV download URLs contain the configured base plus the output
  filename and must be used exactly as returned.

The service itself does not mount WebDAV. Mounting is handled by the host
service manager, the macOS wrapper, or another infrastructure component.

## Long-running deployments

### macOS v2

The templates in deploy/macos/ describe the v2 LaunchAgent and its optional
WebDAV mount helper. They are templates, not an installer, and do not
automatically load or unload LaunchAgents.

The detailed rollout runbook is in [deploy/macos/README.md](deploy/macos/README.md). The high-level
layout is:

~~~text
~/Library/Application Support/media-converter-v2/
  bin/
  config/
  staging/
  state/
  cache/
  work/
  mnt/
~~~

Typical preparation:

~~~sh
export MC_APP_ROOT="$HOME/Library/Application Support/media-converter-v2"
mkdir -p "$MC_APP_ROOT"/{bin,config,staging,state,cache,work,mnt}

go build -o "$MC_APP_ROOT/bin/media-converter-v2" ./cmd/media-converter-v2
cp deploy/macos/run-media-converter-v2 "$MC_APP_ROOT/bin/"
cp deploy/macos/mount-media-converter-v2-webdav "$MC_APP_ROOT/bin/"
cp deploy/macos/media-converter-v2.env.example \
  "$MC_APP_ROOT/config/media-converter-v2.env"
chmod 700 "$MC_APP_ROOT/bin/run-media-converter-v2" \
  "$MC_APP_ROOT/bin/mount-media-converter-v2-webdav"
chmod 600 "$MC_APP_ROOT/config/media-converter-v2.env"
~~~

Before loading the plists:

1. Replace every REPLACE_ME value.
2. Set the real token and tool paths in the environment file.
3. Configure the WebDAV mount URL when using WebDAV mode.
4. Run plutil -lint on both plist files.
5. Load the mount helper before the converter when WebDAV is required.
6. Verify /health/ready, then run an upload/job/download smoke test.

Do not reuse the legacy service's writable output path for the v2 WebDAV
mount. The v2 cutover, rollback, and backup procedures are documented in
[deploy/macos/README.md](deploy/macos/README.md).

### Linux systemd

The files in deploy/ are target-specific WebDAV rollout templates:

- media-converter-webdav.service runs a system instance.
- media-converter-webdav.user.service runs a user instance.
- The associated .env.example files show the expected storage and WebDAV
  variables.

The system service expects:

- Binary at /opt/media-converter/media-converter.
- Environment file at /etc/media-converter/media-converter-webdav.env.
- A media-converter system user and group.
- A mounted output path at /mnt/media-webdav.
- Existing rclone-webdav.service and caddy-webdav.service dependencies.

Review and adapt the unit paths, user, group, mount path, and network
dependencies before enabling the unit. Do not copy production tokens into a
tracked file.

Example installation flow after editing the environment file:

~~~sh
go build -o ./media-converter ./cmd/media-converter
sudo install -Dm755 ./media-converter \
  /opt/media-converter/media-converter
sudo install -Dm644 deploy/media-converter-webdav.service \
  /etc/systemd/system/media-converter-webdav.service
sudo install -Dm600 deploy/media-converter-webdav.env.example \
  /etc/media-converter/media-converter-webdav.env

sudo systemctl daemon-reload
sudo systemctl enable --now media-converter-webdav.service
sudo systemctl status media-converter-webdav.service
~~~

The supplied unit is for the isolated WebDAV instance, not a universal
local-mode service definition. Confirm the ExecStart, environment file, and
mount dependencies before production use.

## Operations

### Health and readiness

/health/live only checks that the HTTP process is alive. /health/ready checks
the storage roots, external tools, ImageMagick delegates, job manager, and
output mount when WebDAV mode is enabled.

Health endpoints are intentionally unauthenticated so they can be used by a
service manager or load balancer. They do not expose tokens, filesystem paths,
or raw tool diagnostics.

### Metrics

/metrics returns Prometheus text and requires authentication:

~~~sh
curl -sS "$MC_BASE_URL/metrics" \
  -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN"
~~~

The service records queue depth, submitted/rejected jobs, idempotent retries,
processing outcomes, errors, and conversion timing.

### Shutdown and recovery

Send SIGINT or SIGTERM for a graceful shutdown. The service:

1. Stops accepting new work.
2. Marks readiness false.
3. Shuts down HTTP.
4. Waits up to SHUTDOWN_TIMEOUT.
5. Terminates remaining converter process groups.
6. Cleans temporary workspaces during the next cleanup pass.

On restart, persisted job state is reconciled so completed results are not
reprocessed unnecessarily and interrupted work can be represented correctly.

## Security guidance

- Keep MEDIA_SERVICE_TOKEN in a protected environment file or secret manager.
  Never commit it to the repository.
- Use a long random token, for example openssl rand -hex 32.
- Expose the service only on a Tailscale/private interface and firewall the
  port to trusted clients.
- Do not bind to a public interface unless a separate security design provides
  TLS, access control, rate limiting, and network isolation.
- Keep all five data roots on private storage with least-privilege ownership.
- Treat output and staging artifacts as sensitive media; retention is not a
  substitute for backups or access control.
- Do not pass user-controlled paths or URLs to the service. Upload bytes and
  use returned artifact IDs.
- Do not assume an extension is authoritative. Use detected/output metadata.

The service uses Bearer authentication for data and metrics endpoints, but the
HTTP listener itself is plain HTTP. The intended security boundary is a
private/Tailscale network or a trusted TLS reverse proxy.

## Troubleshooting

### LISTEN_ADDR is required

Set LISTEN_ADDR before starting the process:

~~~sh
export LISTEN_ADDR="100.64.0.10:8080"
~~~

Use 127.0.0.1 for a same-host deployment, or a literal private/Tailscale IP
when other hosts need access. Unspecified addresses are rejected by
configuration validation.

### /health/ready returns 503

Check:

~~~sh
curl -sS "$MC_BASE_URL/health/ready"
curl -sS "$MC_BASE_URL/v1/capabilities"
~~~

Then verify:

- ffmpeg, ffprobe, and magick are executable.
- ImageMagick reports HEIC/HEIF, AVIF, and WebP delegates.
- Input, output, state, cache, and work roots exist and are writable.
- ARTIFACT_OUTPUT_ROOT is actually mounted in WebDAV mode.
- WEBDAV_PUBLIC_BASE_URL is set in WebDAV mode.

### 401 Unauthorized

Send the exact header expected by the service:

~~~sh
curl -H "Authorization: Bearer $MEDIA_SERVICE_TOKEN" \
  "$MC_BASE_URL/v1/capabilities"
~~~

Health and capabilities are public by default; artifact and job endpoints are
not.

### 400 unsupported or invalid policy

Use only the policy values documented in the Policies section. In particular,
alpha_background must be a six-digit hexadecimal color such as #FFFFFF.

### 409 Conflict

The job_id already exists with a different normalized request. Use a new job ID
or submit the exact same request to get the existing job.

### 429 Too Many Requests

The bounded queue is full. Retry with backoff or tune QUEUE_SIZE and worker
counts for the machine.

### Download URL points to the wrong host

Set PUBLIC_BASE_URL for local output or WEBDAV_PUBLIC_BASE_URL for WebDAV
output. For WebDAV, use the returned URL verbatim and do not replace it with a
converter artifact URL.

## Development checks

Format, test, and statically check the project:

~~~sh
gofmt -w cmd internal
go test ./...
go vet ./...
go test -race ./...
~~~

Some integration-style tests use real ffmpeg and ffprobe fixtures and may skip
when those tools are unavailable. A production image should install all
required tools and delegates so the readiness check and end-to-end tests
exercise the real processing path.

## Contract discovery

Clients can discover the service without hardcoding implementation details:

~~~sh
curl "$MC_BASE_URL/.well-known/media-converter.json"
curl "$MC_BASE_URL/v1/capabilities"
curl "$MC_BASE_URL/openapi.json"
~~~

The discovery manifest describes authentication, endpoints, job states,
supported media, canonical outputs, and the required upload/job/poll/download
workflow. Capabilities report runtime tool availability, supported delegates,
audio policies, worker counts, and active limits.

## License

No license file has been declared for this repository yet.
