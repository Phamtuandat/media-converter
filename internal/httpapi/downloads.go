package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"media-converter-v2/internal/config"
	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/state"
)

type downloadsResponse struct {
	JobID     string          `json:"job_id"`
	State     domain.JobState `json:"state"`
	Downloads []downloadItem  `json:"downloads"`
}

type downloadItem struct {
	ItemID    string            `json:"item_id"`
	Output    *downloadArtifact `json:"output,omitempty"`
	Thumbnail *downloadArtifact `json:"thumbnail,omitempty"`
}

type downloadArtifact struct {
	ArtifactID  string `json:"artifact_id"`
	Filename    string `json:"filename"`
	MIME        string `json:"mime"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobID := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	jobID = strings.TrimSuffix(jobID, "/downloads")
	if jobID == "" || filepath.Base(jobID) != jobID {
		http.NotFound(w, r)
		return
	}

	record, err := s.manager.Get(r.Context(), jobID)
	if errors.Is(err, state.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !record.Request.Policy.IncludeDownloadURLs {
		writeError(w, http.StatusBadRequest, domain.NewError(
			"download_urls_disabled",
			"job was created without download URLs enabled",
			"downloads",
			false,
			nil,
		))
		return
	}

	response := downloadsResponse{JobID: record.JobID, State: record.State, Downloads: []downloadItem{}}
	if record.State != domain.JobCompleted {
		writeJSON(w, http.StatusAccepted, response)
		return
	}
	if record.Result != nil {
		response.Downloads = downloadsForResult(s, *record.Result)
	}
	writeJSON(w, http.StatusOK, response)
}

func downloadsForResult(s *Server, result domain.JobResult) []downloadItem {
	downloads := make([]downloadItem, 0, len(result.Items))
	for _, item := range result.Items {
		entry := downloadItem{ItemID: item.ID}
		if item.Output != nil && item.Output.ArtifactID != "" {
			entry.Output = artifactDownload(item.Output.ArtifactID, item.Output.Filename, item.Output.MIME, item.Output.Size, s)
		}
		if item.Thumbnail != nil && item.Thumbnail.ArtifactID != "" {
			entry.Thumbnail = artifactDownload(item.Thumbnail.ArtifactID, item.Thumbnail.Filename, item.Thumbnail.MIME, item.Thumbnail.Size, s)
		}
		if entry.Output != nil || entry.Thumbnail != nil {
			downloads = append(downloads, entry)
		}
	}
	return downloads
}

func artifactDownload(artifactID, filename, mime string, size int64, s *Server) *downloadArtifact {
	return &downloadArtifact{
		ArtifactID:  artifactID,
		Filename:    filename,
		MIME:        mime,
		Size:        size,
		DownloadURL: s.artifactURLForFile(artifactID, filename),
	}
}

func (s *Server) artifactURL(artifactID string) string {
	return s.artifactURLForFile(artifactID, "")
}

func (s *Server) artifactURLForFile(artifactID, filename string) string {
	path := "/v1/artifacts/" + url.PathEscape(artifactID)
	base := configuredDownloadBaseURL(s.cfg)
	if base == "" {
		return path
	}
	if s.cfg.OutputMode == config.OutputModeWebDAV {
		if filename == "" {
			filename = artifactID
		}
		return strings.TrimRight(base, "/") + "/" + url.PathEscape(filename)
	}
	return strings.TrimRight(base, "/") + path
}

func configuredDownloadBaseURL(cfg config.Config) string {
	if cfg.OutputMode == config.OutputModeWebDAV {
		return cfg.WebDAVBaseURL
	}
	return cfg.PublicBaseURL
}
