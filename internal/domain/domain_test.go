package domain

import (
	"encoding/json"
	"testing"
)

func TestPolicyDownloadURLsDefaultsToOptIn(t *testing.T) {
	var request JobRequest
	if err := json.Unmarshal([]byte(`{"job_id":"job","items":[],"policy":{"include_download_urls":true}}`), &request); err != nil {
		t.Fatal(err)
	}
	if !request.Policy.IncludeDownloadURLs {
		t.Fatal("include_download_urls should be enabled when requested")
	}

	defaultPolicy := DefaultPolicy()
	if defaultPolicy.IncludeDownloadURLs {
		t.Fatal("include_download_urls must default to false")
	}
}

func TestJobResultAggregate(t *testing.T) {
	tests := []struct {
		name   string
		status []ItemStatus
		want   Outcome
	}{
		{"all success", []ItemStatus{ItemSuccess, ItemSuccess}, OutcomeSuccess},
		{"mixed", []ItemStatus{ItemSuccess, ItemRejected}, OutcomePartial},
		{"all rejected", []ItemStatus{ItemRejected}, OutcomeRejected},
		{"all failed", []ItemStatus{ItemFailed}, OutcomeFailed},
		{"failed and rejected", []ItemStatus{ItemFailed, ItemRejected}, OutcomeFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			items := make([]ItemResult, len(test.status))
			for i, status := range test.status {
				items[i].Status = status
			}
			if got := (JobResult{Items: items}).Aggregate(); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
