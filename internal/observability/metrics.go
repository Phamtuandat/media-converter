package observability

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics stores process-local counters for the daemon. It intentionally has
// no external exporter dependency; the HTTP layer renders the snapshot.
type Metrics struct {
	jobsSubmitted    atomic.Int64
	jobsIdempotent   atomic.Int64
	queueRejected    atomic.Int64
	jobsCompleted    atomic.Int64
	jobDurationCount atomic.Int64
	jobDurationNS    atomic.Int64
	queueDepth       atomic.Int64
	inputBytes       atomic.Int64
	outputBytes      atomic.Int64
	cleanupSuccess   atomic.Int64
	cleanupFailure   atomic.Int64
	recoverySuccess  atomic.Int64
	recoveryFailure  atomic.Int64

	mu         sync.Mutex
	outcomes   map[string]int64
	statuses   map[string]int64
	operations map[string]int64
	errors     map[string]int64
	phaseCount map[string]int64
	phaseNS    map[string]int64
}

const (
	PhaseQueue       = "queue"
	PhaseStateIO     = "state_io"
	PhaseDetectProbe = "detect_probe"
	PhaseProcessing  = "processing"
	PhaseValidation  = "validation"
	PhaseCommit      = "commit"
)

var phaseNames = []string{
	PhaseQueue,
	PhaseStateIO,
	PhaseDetectProbe,
	PhaseProcessing,
	PhaseValidation,
	PhaseCommit,
}

func NewMetrics() *Metrics {
	return &Metrics{
		outcomes:   make(map[string]int64),
		statuses:   make(map[string]int64),
		operations: make(map[string]int64),
		errors:     make(map[string]int64),
		phaseCount: makePhaseMap(),
		phaseNS:    makePhaseMap(),
	}
}

func (m *Metrics) IncJobSubmitted()  { m.jobsSubmitted.Add(1) }
func (m *Metrics) IncIdempotent()    { m.jobsIdempotent.Add(1) }
func (m *Metrics) IncQueueRejected() { m.queueRejected.Add(1) }
func (m *Metrics) SetQueueDepth(depth int) {
	m.queueDepth.Store(int64(depth))
}

func (m *Metrics) RecordJobCompleted(outcome string, duration time.Duration) {
	m.jobsCompleted.Add(1)
	m.jobDurationCount.Add(1)
	m.jobDurationNS.Add(duration.Nanoseconds())
	m.mu.Lock()
	m.outcomes[outcome]++
	m.mu.Unlock()
}

func (m *Metrics) RecordItem(status, operation, errorCode string, inputBytes, outputBytes int64) {
	if inputBytes > 0 {
		m.inputBytes.Add(inputBytes)
	}
	if outputBytes > 0 {
		m.outputBytes.Add(outputBytes)
	}
	m.mu.Lock()
	if status != "" {
		m.statuses[status]++
	}
	if operation != "" {
		m.operations[operation]++
	}
	if errorCode != "" {
		m.errors[errorCode]++
	}
	m.mu.Unlock()
}

func (m *Metrics) RecordPhase(phase string, duration time.Duration) {
	if !knownPhase(phase) {
		return
	}
	if duration < 0 {
		duration = 0
	}
	m.mu.Lock()
	m.phaseCount[phase]++
	m.phaseNS[phase] += duration.Nanoseconds()
	m.mu.Unlock()
}

func (m *Metrics) RecordError(code string) {
	if code == "" {
		return
	}
	m.mu.Lock()
	m.errors[code]++
	m.mu.Unlock()
}

func (m *Metrics) RecordCleanup(err error) {
	if err == nil {
		m.cleanupSuccess.Add(1)
		return
	}
	m.cleanupFailure.Add(1)
	m.RecordError("cleanup_failed")
}

func (m *Metrics) RecordRecovery(err error) {
	if err == nil {
		m.recoverySuccess.Add(1)
		return
	}
	m.recoveryFailure.Add(1)
	m.RecordError("recovery_failed")
}

func (m *Metrics) Prometheus() string {
	m.mu.Lock()
	outcomes := copyMap(m.outcomes)
	statuses := copyMap(m.statuses)
	operations := copyMap(m.operations)
	errors := copyMap(m.errors)
	phaseCount := copyMap(m.phaseCount)
	phaseNS := copyMap(m.phaseNS)
	m.mu.Unlock()

	var b strings.Builder
	writeGauge(&b, "media_converter_queue_depth", m.queueDepth.Load())
	writeCounter(&b, "media_converter_input_bytes_total", m.inputBytes.Load())
	writeCounter(&b, "media_converter_output_bytes_total", m.outputBytes.Load())
	writeCounter(&b, "media_converter_jobs_submitted_total", m.jobsSubmitted.Load())
	writeCounter(&b, "media_converter_jobs_idempotent_total", m.jobsIdempotent.Load())
	writeCounter(&b, "media_converter_queue_rejected_total", m.queueRejected.Load())
	writeCounter(&b, "media_converter_jobs_completed_total", m.jobsCompleted.Load())
	writeCounter(&b, "media_converter_cleanup_success_total", m.cleanupSuccess.Load())
	writeCounter(&b, "media_converter_cleanup_failure_total", m.cleanupFailure.Load())
	writeCounter(&b, "media_converter_recovery_success_total", m.recoverySuccess.Load())
	writeCounter(&b, "media_converter_recovery_failure_total", m.recoveryFailure.Load())
	writeCounter(&b, "media_converter_job_duration_seconds_count", m.jobDurationCount.Load())
	fmt.Fprintf(&b, "media_converter_job_duration_seconds_sum %.6f\n", float64(m.jobDurationNS.Load())/float64(time.Second))
	writeLabeledCounters(&b, "media_converter_job_outcomes_total", "outcome", outcomes)
	writeLabeledCounters(&b, "media_converter_items_total", "status", statuses)
	writeLabeledCounters(&b, "media_converter_operations_total", "operation", operations)
	writeLabeledCounters(&b, "media_converter_errors_total", "code", errors)
	writeLabeledCounters(&b, "media_converter_phase_duration_seconds_count", "phase", phaseCount)
	writeLabeledDurationSums(&b, "media_converter_phase_duration_seconds_sum", "phase", phaseNS)
	return b.String()
}

func makePhaseMap() map[string]int64 {
	result := make(map[string]int64, len(phaseNames))
	for _, phase := range phaseNames {
		result[phase] = 0
	}
	return result
}

func knownPhase(phase string) bool {
	for _, name := range phaseNames {
		if phase == name {
			return true
		}
	}
	return false
}

func copyMap(source map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func writeCounter(b *strings.Builder, name string, value int64) {
	fmt.Fprintf(b, "%s %d\n", name, value)
}

func writeGauge(b *strings.Builder, name string, value int64) {
	fmt.Fprintf(b, "%s %d\n", name, value)
}

func writeLabeledCounters(b *strings.Builder, name, label string, values map[string]int64) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, key, values[key])
	}
}

func writeLabeledDurationSums(b *strings.Builder, name, label string, values map[string]int64) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %.6f\n", name, label, key, float64(values[key])/float64(time.Second))
	}
}
