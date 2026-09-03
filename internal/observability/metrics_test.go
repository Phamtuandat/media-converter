package observability

import (
	"strings"
	"testing"
	"time"
)

func TestRecordPhaseExportsOnlyKnownLowCardinalityLabels(t *testing.T) {
	metrics := NewMetrics()
	metrics.RecordPhase(PhaseValidation, 1500*time.Millisecond)
	metrics.RecordPhase("job-123", time.Second)

	output := metrics.Prometheus()
	if !strings.Contains(output, `media_converter_phase_duration_seconds_count{phase="validation"} 1`) {
		t.Fatalf("validation phase count missing from metrics:\n%s", output)
	}
	if !strings.Contains(output, `media_converter_phase_duration_seconds_sum{phase="validation"} 1.500000`) {
		t.Fatalf("validation phase duration missing from metrics:\n%s", output)
	}
	if strings.Contains(output, `phase="job-123"`) {
		t.Fatalf("unexpected high-cardinality phase label in metrics:\n%s", output)
	}
}
