package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"media-converter-v2/internal/config"
)

// The OpenAPI document is static contract metadata. Runtime state belongs to
// /v1/capabilities and /health/ready instead.
func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPIDocumentFor(s.cfg))
}

func openAPIDocumentFor(cfg config.Config) []byte {
	version, err := json.Marshal(cfg.ProcessorVersion)
	if err != nil {
		return openAPIDocument
	}
	return bytes.Replace(openAPIDocument, []byte(`"{{PROCESSOR_VERSION}}"`), version, 1)
}

var openAPIDocument = mustJSON([]byte(`{
  "openapi": "3.0.3",
  "info": {
    "title": "Media Converter Service",
    "version": "{{PROCESSOR_VERSION}}",
    "description": "Authenticated asynchronous media conversion service."
  },
  "paths": {
    "/.well-known/media-converter.json": {
      "get": {"operationId": "getDiscovery", "responses": {"200": {"description": "Stable discovery manifest", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DiscoveryManifest"}}}}}}
    },
    "/v1/capabilities": {
      "get": {"operationId": "getCapabilities", "responses": {"200": {"description": "Runtime capabilities", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Capabilities"}}}}}}
    },
    "/v1/artifacts": {
      "post": {
        "operationId": "uploadArtifact",
        "security": [{"bearerAuth": []}],
        "parameters": [
          {"name": "Content-Type", "in": "header", "required": true, "schema": {"type": "string"}},
          {"name": "X-Filename", "in": "header", "required": true, "schema": {"type": "string"}}
        ],
        "requestBody": {"required": true, "content": {"application/octet-stream": {"schema": {"type": "string", "format": "binary"}}}},
        "responses": {
          "201": {"description": "Staged artifact", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ArtifactUpload"}}}},
          "400": {"$ref": "#/components/responses/BadRequest"}, "401": {"$ref": "#/components/responses/Unauthorized"}, "413": {"$ref": "#/components/responses/TooLarge"}, "503": {"$ref": "#/components/responses/Unavailable"}
        }
      }
    },
    "/v1/jobs": {
      "post": {
        "operationId": "createJob",
        "security": [{"bearerAuth": []}],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/JobRequest"}}}},
        "responses": {
          "202": {"description": "Job queued", "headers": {"Location": {"schema": {"type": "string"}}}, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/AcceptedJob"}}}},
          "200": {"description": "Existing idempotent job", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/JobResponse"}}}},
          "400": {"$ref": "#/components/responses/BadRequest"}, "401": {"$ref": "#/components/responses/Unauthorized"}, "409": {"$ref": "#/components/responses/Conflict"}, "429": {"$ref": "#/components/responses/TooManyRequests"}, "503": {"$ref": "#/components/responses/Unavailable"}
        }
      }
    },
    "/v1/jobs/{job_id}": {
      "get": {
        "operationId": "getJob",
        "security": [{"bearerAuth": []}],
        "parameters": [{"$ref": "#/components/parameters/JobID"}],
        "responses": {"200": {"description": "Job state or completed result", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/JobResponse"}}}}, "401": {"$ref": "#/components/responses/Unauthorized"}, "404": {"$ref": "#/components/responses/NotFound"}}
      }
    },
    "/v1/jobs/{job_id}/downloads": {
      "get": {
        "operationId": "getJobDownloads",
        "security": [{"bearerAuth": []}],
        "parameters": [{"$ref": "#/components/parameters/JobID"}],
        "responses": {
          "200": {"description": "Download URLs for committed job artifacts from the configured storage backend", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DownloadsResponse"}}}},
          "202": {"description": "Job is not completed yet", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DownloadsResponse"}}}},
          "400": {"$ref": "#/components/responses/BadRequest"}, "401": {"$ref": "#/components/responses/Unauthorized"}, "404": {"$ref": "#/components/responses/NotFound"}
        }
      }
    },
    "/v1/artifacts/{artifact_id}": {
      "get": {
        "operationId": "getArtifact",
        "security": [{"bearerAuth": []}],
        "parameters": [{"$ref": "#/components/parameters/ArtifactID"}, {"name": "Range", "in": "header", "required": false, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "Canonical artifact stream", "content": {"image/jpeg": {"schema": {"type": "string", "format": "binary"}}, "video/mp4": {"schema": {"type": "string", "format": "binary"}}, "audio/ogg": {"schema": {"type": "string", "format": "binary"}}, "audio/wav": {"schema": {"type": "string", "format": "binary"}}, "audio/mp4": {"schema": {"type": "string", "format": "binary"}}}}, "206": {"description": "Partial artifact stream", "content": {"application/octet-stream": {"schema": {"type": "string", "format": "binary"}}}}, "401": {"$ref": "#/components/responses/Unauthorized"}, "404": {"$ref": "#/components/responses/NotFound"}}
      }
    },
    "/health/live": {"get": {"operationId": "live", "responses": {"200": {"description": "Process is alive"}}}},
    "/health/ready": {"get": {"operationId": "ready", "responses": {"200": {"description": "Ready"}, "503": {"description": "Not ready"}}}},
    "/metrics": {"get": {"operationId": "metrics", "security": [{"bearerAuth": []}], "responses": {"200": {"description": "Prometheus metrics", "content": {"text/plain": {"schema": {"type": "string"}}}}, "401": {"$ref": "#/components/responses/Unauthorized"}}}}
  },
  "components": {
    "securitySchemes": {"bearerAuth": {"type": "http", "scheme": "bearer"}},
    "parameters": {
      "JobID": {"name": "job_id", "in": "path", "required": true, "schema": {"type": "string"}},
      "ArtifactID": {"name": "artifact_id", "in": "path", "required": true, "schema": {"type": "string"}}
    },
    "responses": {
      "BadRequest": {"description": "Invalid request", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}},
      "Unauthorized": {"description": "Bearer authentication required"},
      "Conflict": {"description": "Idempotency conflict", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}},
      "TooManyRequests": {"description": "Queue is full", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}},
      "TooLarge": {"description": "Artifact exceeds the configured size limit", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}},
      "Unavailable": {"description": "Service unavailable", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}}},
      "NotFound": {"description": "Resource not found"}
    },
    "schemas": {
      "DiscoveryManifest": {"type": "object", "required": ["service", "version", "api_version", "authentication", "endpoints", "job_model", "item_outcomes", "operations", "media_contract", "workflow", "agent_guidance"]},
      "Capabilities": {"type": "object", "required": ["processor_version", "policy_version", "features", "audio_policies", "runtime", "formats", "workers", "limits"], "properties": {"processor_version": {"type": "string"}, "policy_version": {"type": "string"}, "features": {"type": "object", "properties": {"artifact_upload": {"type": "boolean"}, "staging": {"type": "boolean"}, "download_urls": {"type": "boolean"}, "download_url_mode": {"type": "string", "enum": ["absolute", "relative"]}}}, "audio_policies": {"type": "array", "items": {"$ref": "#/components/schemas/AudioPolicy"}}}},
      "ArtifactUpload": {"type": "object", "required": ["artifact_id", "sha256", "size", "declared_mime", "filename", "state"], "properties": {"artifact_id": {"type": "string"}, "sha256": {"type": "string"}, "size": {"type": "integer", "format": "int64"}, "declared_mime": {"type": "string"}, "filename": {"type": "string"}, "state": {"type": "string", "enum": ["staged"]}}},
      "AudioPolicy": {"type": "object", "required": ["policy", "mime", "extension", "container", "codec", "sample_rate", "channels"], "properties": {"policy": {"type": "string"}, "mime": {"type": "string"}, "extension": {"type": "string"}, "container": {"type": "string"}, "codec": {"type": "string"}, "sample_rate": {"type": "integer"}, "channels": {"type": "integer"}}},
      "JobRequest": {"type": "object", "required": ["job_id", "items"], "properties": {"job_id": {"type": "string"}, "items": {"type": "array", "items": {"$ref": "#/components/schemas/Item"}}, "policy": {"$ref": "#/components/schemas/Policy"}}},
      "Item": {"type": "object", "required": ["id", "artifact_id"], "properties": {"id": {"type": "string"}, "artifact_id": {"type": "string"}, "declared_kind": {"type": "string"}, "expected_sha256": {"type": "string"}}},
      "Policy": {"type": "object", "properties": {"target_image": {"type": "string", "enum": ["zalo_jpeg"]}, "target_video": {"type": "string", "enum": ["zalo_mp4_h264_aac_faststart"]}, "target_audio": {"type": "string", "enum": ["voice_ogg_opus", "wav_pcm_s16le", "m4a_aac_lc"]}, "strip_metadata": {"type": "boolean"}, "generate_thumbnail": {"type": "boolean"}, "alpha_background": {"type": "string"}, "strict_format_match": {"type": "boolean"}, "include_download_urls": {"type": "boolean", "default": false}}},
      "AcceptedJob": {"type": "object", "required": ["job_id", "state"], "properties": {"job_id": {"type": "string"}, "state": {"type": "string", "enum": ["queued", "processing", "completed"]}}},
      "JobResponse": {"type": "object", "required": ["job_id", "state"], "properties": {"job_id": {"type": "string"}, "state": {"type": "string", "enum": ["queued", "processing", "completed"]}, "result": {"$ref": "#/components/schemas/JobResult"}}},
      "DownloadsResponse": {"type": "object", "required": ["job_id", "state", "downloads"], "properties": {"job_id": {"type": "string"}, "state": {"type": "string", "enum": ["queued", "processing", "completed"]}, "downloads": {"type": "array", "items": {"$ref": "#/components/schemas/DownloadItem"}}}},
      "DownloadItem": {"type": "object", "required": ["item_id"], "properties": {"item_id": {"type": "string"}, "output": {"$ref": "#/components/schemas/DownloadArtifact"}, "thumbnail": {"$ref": "#/components/schemas/DownloadArtifact"}}},
      "DownloadArtifact": {"type": "object", "required": ["artifact_id", "filename", "mime", "size", "download_url"], "properties": {"artifact_id": {"type": "string"}, "filename": {"type": "string"}, "mime": {"type": "string"}, "size": {"type": "integer", "format": "int64"}, "download_url": {"type": "string", "format": "uri-reference", "description": "Use this URL verbatim; it may point directly to the configured WebDAV output."}}},
      "JobResult": {"type": "object", "required": ["outcome", "items"], "properties": {"outcome": {"type": "string", "enum": ["success", "partial_success", "rejected", "failed"]}, "items": {"type": "array", "items": {"$ref": "#/components/schemas/ItemResult"}}}},
      "MediaDetected": {"type": "object", "properties": {"kind": {"type": "string"}, "format": {"type": "string"}, "container": {"type": "string"}, "mime": {"type": "string"}, "video_codec": {"type": "string"}, "audio_codec": {"type": "string"}, "audio_profile": {"type": "string"}, "sample_rate": {"type": "integer"}, "channels": {"type": "integer"}, "width": {"type": "integer"}, "height": {"type": "integer"}, "duration_ms": {"type": "integer"}}},
      "OutputMetadata": {"type": "object", "required": ["artifact_id", "filename", "extension", "mime", "size", "sha256"], "properties": {"artifact_id": {"type": "string"}, "filename": {"type": "string"}, "extension": {"type": "string"}, "mime": {"type": "string"}, "size": {"type": "integer", "format": "int64"}, "sha256": {"type": "string"}, "audio_codec": {"type": "string"}, "audio_profile": {"type": "string"}, "sample_rate": {"type": "integer"}, "channels": {"type": "integer"}, "video_codec": {"type": "string"}, "pixel_format": {"type": "string"}, "duration_ms": {"type": "integer"}}},
      "ItemResult": {"type": "object", "required": ["id", "status"], "properties": {"id": {"type": "string"}, "status": {"type": "string", "enum": ["success", "rejected", "failed"]}, "operation": {"type": "string", "enum": ["passthrough", "normalized", "remuxed", "transcoded"]}, "detected": {"$ref": "#/components/schemas/MediaDetected"}, "output": {"$ref": "#/components/schemas/OutputMetadata"}, "error": {"$ref": "#/components/schemas/ProcessingError"}}},
      "ProcessingError": {"type": "object", "required": ["code", "message", "retryable"], "properties": {"code": {"type": "string"}, "message": {"type": "string"}, "retryable": {"type": "boolean"}, "stage": {"type": "string"}}},
      "ErrorResponse": {"type": "object", "required": ["error"], "properties": {"error": {"$ref": "#/components/schemas/ProcessingError"}}}
    }
  }
}`))

func mustJSON(document []byte) []byte {
	var value any
	if err := json.Unmarshal(document, &value); err != nil {
		panic(err)
	}
	return document
}
