package capacity

import (
	"babytracker/server/internal/config"
	"babytracker/server/internal/diagnostics"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type CompletionReason string

const (
	Complete         CompletionReason = "complete"
	Interrupted      CompletionReason = "interrupted"
	Deadline         CompletionReason = "deadline"
	SetupFailed      CompletionReason = "setup_failed"
	SampleIncomplete CompletionReason = "sample_incomplete"
	InvalidResult    CompletionReason = "invalid_result"
	CleanupFailed    CompletionReason = "cleanup_failed"
	Gated            CompletionReason = "gated"
	Unmeasured       CompletionReason = "unmeasured"
)

type Percentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_seconds"`
	P95   float64 `json:"p95_seconds"`
	P99   float64 `json:"p99_seconds"`
}
type Counts struct {
	GlobalAdmissionRefusals int `json:"global_admission_refusals"` // Subset of Refusals; the remainder are quota 429s.
	Attempts                int `json:"attempts"`
	Successes               int `json:"successes"`
	Refusals                int `json:"refusals"`
	UnexpectedErrors        int `json:"unexpected_errors"`
}
type ResourceBudget struct {
	CPUs            float64 `json:"cpus"`
	MemoryBytes     int64   `json:"memory_bytes"`
	MemorySwapBytes int64   `json:"memory_swap_bytes"`
}
type CellPlan struct {
	Devices       int    `json:"devices"`
	ActiveWriters int    `json:"active_writers"`
	Rate          int    `json:"rate"`
	Seconds       int    `json:"seconds"`
	Shape         string `json:"shape,omitempty"`
}

type CellResult struct {
	PlannedCell             *CellPlan                    `json:"planned_cell,omitempty"`
	RequiredAnchor          *CellPlan                    `json:"required_anchor,omitempty"`
	MeasurementState        string                       `json:"measurement_state,omitempty"`
	RefusalClassification   string                       `json:"refusal_classification,omitempty"`
	DatabaseProfile         string                       `json:"database_profile,omitempty"`
	DatabasePools           *config.DatabasePools        `json:"database_pools,omitempty"`
	DiagnosticsEnabled      bool                         `json:"diagnostics_enabled"`
	AppResources            []diagnostics.ResourceSample `json:"app_resource_samples"`
	UnresolvedWrites        int                          `json:"unresolved_writes"`
	PlannedEmptyPulls       int                          `json:"planned_empty_pulls"`
	UnfiredEmptyPulls       int                          `json:"unfired_empty_pulls"`
	PlannedRefresh          int                          `json:"planned_refresh"`
	UnfiredRefresh          int                          `json:"unfired_refresh"`
	PlannedReconnect        int                          `json:"planned_reconnect"`
	UnfiredReconnect        int                          `json:"unfired_reconnect"`
	Live                    LiveEvidence                 `json:"live"`
	EnforcedLimits          map[string]ResourceBudget    `json:"enforced_limits"`
	MeasurementHTTP         Counts                       `json:"measurement_http"`
	MeasuredRefresh         Counts                       `json:"measured_refresh"`
	MeasuredReconnect       Counts                       `json:"measured_socket_reconnect"`
	ErrorCauses             map[string]int               `json:"error_causes"`
	DrainSeconds            float64                      `json:"drain_seconds"`
	DeadlineUnfiredWrites   int                          `json:"deadline_unfired_writes"`
	AcknowledgedWithoutPeer int                          `json:"acknowledged_without_peer"`
	FailureCode             string                       `json:"failure_code"`
	Version                 int                          `json:"version"`
	Scenario                string                       `json:"scenario"`
	Requested               Workload                     `json:"requested"`
	ActualDevices           int                          `json:"actual_devices"`
	ActualWriters           int                          `json:"actual_writers"`
	PlannedWrites           int                          `json:"planned_writes"`
	ScheduledWrites         int                          `json:"scheduled_writes"`
	SkippedWrites           int                          `json:"skipped_writes"`
	Counts
	Setup           Counts `json:"setup"`
	Warmup          Counts `json:"warmup"`
	PeerPulls       Counts `json:"peer_pulls"`
	CountsSemantics string `json:"counts_semantics"`

	Refresh          Counts             `json:"refresh"`
	RefreshReconnect Counts             `json:"refresh_reconnect"`
	ScheduleEvidence map[string]float64 `json:"schedule_evidence"`

	EmptyPulls           Counts    `json:"empty_pulls"`
	ContentionProofPairs int       `json:"contention_proof_pairs"`
	ProofAcknowledged    int       `json:"proof_acknowledged"`
	ProofPeerObserved    int       `json:"proof_peer_observed"`
	BacklogAcknowledged  int       `json:"backlog_acknowledged"`
	CatchupObserved      int       `json:"catchup_observed"`
	CatchupSeconds       []float64 `json:"catchup_seconds"`
	ReconnectAttempts    int       `json:"reconnect_attempts"`
	SlowConsumerDevices  int       `json:"slow_consumer_devices"`

	SetupSeconds             float64           `json:"setup_seconds"`
	MeasurementSeconds       float64           `json:"measurement_seconds"`
	Acknowledged             int               `json:"acknowledged"`
	PeerApplied              int               `json:"peer_applied"`
	LatencySeconds           []float64         `json:"latency_seconds"`
	PeerHTTPSeconds          []float64         `json:"peer_http_seconds"`
	Percentiles              *Percentiles      `json:"percentiles"`
	PeerHTTPPercentiles      *Percentiles      `json:"peer_http_percentiles"`
	Completion               CompletionReason  `json:"completion"`
	Passing                  bool              `json:"passing"`
	ResourceAvailability     map[string]string `json:"resource_availability"`
	ResourcesEnforced        bool              `json:"resources_enforced"`
	SealedAuthorityHead      uint64            `json:"sealed_authority_head"`
	RestoredAuthorityHead    uint64            `json:"restored_authority_head"`
	FixtureInputSHA256       string            `json:"fixture_input_sha256"`
	FixtureSourceInputSHA256 string            `json:"fixture_source_input_sha256"`
	ProductSHA               string            `json:"product_sha"`
	ProductInputSHA256       string            `json:"product_input_sha256"`
	ProductBinarySHA256      string            `json:"product_binary_sha256"`
	AppImageID               string            `json:"app_image_id"`
	DatabaseImageID          string            `json:"database_image_id"`
	ProxyImageID             string            `json:"proxy_image_id"`
	Profile                  string            `json:"profile"`
	LoadGenerator            string            `json:"load_generator"`
	PeerSemantics            string            `json:"peer_semantics"`
	FixtureProfile           string            `json:"fixture_profile"`
	// MCP is present exactly when the cell enabled MCP. It is never folded
	// into sync counts or sync acceptance.
	MCP *MCPEvidence `json:"mcp,omitempty"`
}

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func distribution(values []float64) *Percentiles {
	if len(values) < 200 {
		return nil
	}
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	return &Percentiles{len(values), values[int(math.Ceil(float64(len(values))*.50))-1], values[int(math.Ceil(float64(len(values))*.95))-1], values[int(math.Ceil(float64(len(values))*.99))-1]}
}
func coherentCounts(c Counts) bool {
	return c.GlobalAdmissionRefusals >= 0 && c.GlobalAdmissionRefusals <= c.Refusals && c.Attempts >= 0 && c.Successes >= 0 && c.Refusals >= 0 && c.UnexpectedErrors >= 0 && c.Successes+c.Refusals+c.UnexpectedErrors == c.Attempts
}
func (r *CellResult) accountingValid() bool {
	if (r.MCP != nil) != (r.Requested.MCPMode != "") || (r.MCP != nil && (r.MCP.Mode != r.Requested.MCPMode || !r.MCP.coherent())) {
		return false
	}
	for _, v := range []int{r.Version, r.ActualDevices, r.ActualWriters, r.PlannedWrites, r.ScheduledWrites, r.SkippedWrites, r.PlannedEmptyPulls, r.PlannedRefresh, r.PlannedReconnect, r.ProofAcknowledged, r.ProofPeerObserved, r.BacklogAcknowledged, r.CatchupObserved, r.ContentionProofPairs, r.ReconnectAttempts, r.SlowConsumerDevices} {
		if v < 0 {
			return false
		}
	}

	for _, c := range []Counts{r.Counts, r.Setup, r.Warmup, r.PeerPulls, r.Refresh, r.RefreshReconnect, r.EmptyPulls, r.MeasuredRefresh, r.MeasuredReconnect} {
		if !coherentCounts(c) {
			return false
		}
	}
	for _, v := range append(append(append([]float64{r.MeasurementSeconds, r.SetupSeconds, r.DrainSeconds}, r.LatencySeconds...), r.PeerHTTPSeconds...), r.CatchupSeconds...) {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	for _, v := range r.ScheduleEvidence {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	if r.DrainSeconds > 10.1 || len(r.LatencySeconds) != r.Successes || len(r.PeerHTTPSeconds) != r.PeerApplied || r.Acknowledged != r.Successes || r.PeerApplied != r.PeerPulls.Successes || r.PeerPulls.Attempts > r.Acknowledged {
		return false
	}
	if r.Live.Connected < 0 || r.Live.Established < 0 || r.Live.UnexpectedLosses < 0 || r.Live.PlannedRenewals < 0 || r.Live.PlannedShutdowns < 0 || r.Live.Connected > r.ActualDevices || r.Live.Established < r.Live.Connected {
		return false
	}
	complete := r.Completion == Complete || r.Completion == SampleIncomplete
	if !complete && r.MeasurementSeconds == 0 && r.Attempts == 0 && r.ActualDevices == 0 {
		return true
	} // honest pre-admission checkpoint
	w := r.Requested
	if w.Validate(FixtureManifest{Devices: w.Devices}) != nil || r.ActualDevices < 0 || r.ActualDevices > w.Devices || r.ActualWriters < 0 || r.ActualWriters > w.ActiveWriters || r.PlannedWrites != plannedSlots(w.ActiveWriters, w.WritesPerWriterPerMinute, w.Measure) || r.ScheduledWrites < r.Attempts+r.SkippedWrites || r.ScheduledWrites > r.PlannedWrites || r.SkippedWrites < 0 || r.MeasurementSeconds > w.Measure.Seconds() {
		return false
	}
	for _, pair := range [][2]int{{r.PlannedEmptyPulls, r.EmptyPulls.Attempts}, {r.PlannedRefresh, r.MeasuredRefresh.Attempts}, {r.PlannedReconnect, r.MeasuredReconnect.Attempts}} {
		if pair[0] < 0 || pair[1] > pair[0] {
			return false
		}
	}
	if r.MeasuredRefresh.Attempts > r.Refresh.Attempts || r.MeasuredReconnect.Attempts > r.RefreshReconnect.Attempts {
		return false
	}
	if !complete {
		return true
	}
	if r.Version != 1 || r.ActualDevices != w.Devices || r.ActualWriters != w.ActiveWriters || r.MeasurementSeconds != w.Measure.Seconds() || r.ScheduledWrites != r.Attempts+r.SkippedWrites || r.PeerPulls.Attempts != r.Acknowledged {
		return false
	}
	if r.PlannedEmptyPulls != w.Devices*int((w.Measure-1)/w.EmptyPullInterval) {
		return false
	}
	renewals := 0
	if w.Measure > 10*time.Minute {
		renewals = w.Devices
	}
	if r.PlannedRefresh != renewals || r.PlannedReconnect != renewals {
		return false
	}
	return r.provenanceValid()
}
func (r *CellResult) provenanceValid() bool {
	_, budget, err := databaseProfile(r.DatabaseProfile)
	if err != nil {
		return false
	}
	if budget.Explicit() {
		if r.DatabasePools == nil || *r.DatabasePools != budget {
			return false
		}
	} else if r.DatabasePools != nil {
		return false
	}
	if !sourcePattern.MatchString(r.ProductSHA) || !digestPattern.MatchString(r.ProductInputSHA256) || !digestPattern.MatchString(r.ProductBinarySHA256) || !digestPattern.MatchString(r.FixtureInputSHA256) || !digestPattern.MatchString(r.FixtureSourceInputSHA256) || r.SealedAuthorityHead == 0 || r.RestoredAuthorityHead < r.SealedAuthorityHead {
		return false
	}
	for _, id := range []string{r.AppImageID, r.DatabaseImageID, r.ProxyImageID} {
		if !strings.HasPrefix(id, "sha256:") || !digestPattern.MatchString(strings.TrimPrefix(id, "sha256:")) {
			return false
		}
	}
	budgets := map[string]ResourceBudget{"app": {1, 536870912, 536870912}, "database": {2.5, 3221225472, 3221225472}, "proxy": {.5, 536870912, 536870912}}
	if r.Profile != "standard" && r.Profile != "small" || len(r.EnforcedLimits) != 3 {
		return false
	}
	for name, want := range budgets {
		if r.Profile == "small" {
			want.CPUs /= 2
			want.MemoryBytes /= 2
			want.MemorySwapBytes /= 2
		}
		if r.EnforcedLimits[name] != want {
			return false
		}
	}
	return r.ResourcesEnforced
}

func (r *CellResult) Finalize() {
	r.UnfiredEmptyPulls = r.PlannedEmptyPulls - r.EmptyPulls.Attempts
	r.UnfiredRefresh = r.PlannedRefresh - r.MeasuredRefresh.Attempts
	r.UnfiredReconnect = r.PlannedReconnect - r.MeasuredReconnect.Attempts
	r.MeasurementHTTP = Counts{}
	for _, c := range []Counts{r.Counts, r.PeerPulls, r.EmptyPulls, r.MeasuredRefresh} {
		r.MeasurementHTTP.Attempts += c.Attempts
		r.MeasurementHTTP.Successes += c.Successes
		r.MeasurementHTTP.Refusals += c.Refusals
		r.MeasurementHTTP.GlobalAdmissionRefusals += c.GlobalAdmissionRefusals
		r.MeasurementHTTP.UnexpectedErrors += c.UnexpectedErrors
	}
	r.DeadlineUnfiredWrites = r.PlannedWrites - r.ScheduledWrites
	if r.DeadlineUnfiredWrites < 0 {
		r.DeadlineUnfiredWrites = 0
	}
	r.AcknowledgedWithoutPeer = r.Acknowledged - r.PeerApplied
	if r.AcknowledgedWithoutPeer < 0 {
		r.AcknowledgedWithoutPeer = 0
	}
	r.UnresolvedWrites = r.ScheduledWrites - r.SkippedWrites - r.Attempts
	r.Percentiles = nil
	r.PeerHTTPPercentiles = nil
	r.Passing = false
	if r.MCP != nil {
		r.MCP.finalize()
	}
	if !r.accountingValid() {
		r.Completion = InvalidResult
		return
	}
	if r.MeasurementSeconds < 60 || r.Successes < 200 || r.Completion != Complete || len(r.LatencySeconds) != r.Successes {
		return
	}
	r.Percentiles = distribution(r.LatencySeconds)
	if len(r.PeerHTTPSeconds) == r.PeerApplied {
		r.PeerHTTPPercentiles = distribution(r.PeerHTTPSeconds)
	}
	provenance := r.provenanceValid()
	r.Passing = r.RefusalClassification == refusalClassification && provenance && r.Live.Observed && r.Live.Connected == r.Requested.Devices && r.Live.UnexpectedLosses == 0 && r.UnfiredEmptyPulls == 0 && r.UnfiredRefresh == 0 && r.UnfiredReconnect == 0 && r.ResourcesEnforced && r.Attempts >= r.Successes && r.MeasurementHTTP.Refusals == 0 && r.MeasuredReconnect.Refusals == 0 && r.MeasuredReconnect.UnexpectedErrors == 0 && r.Setup.Refusals == 0 && r.Setup.UnexpectedErrors == 0 && r.Warmup.Refusals == 0 && r.Warmup.UnexpectedErrors == 0 && r.Refresh.Refusals == 0 && r.Refresh.UnexpectedErrors == 0 && r.RefreshReconnect.Refusals == 0 && r.RefreshReconnect.UnexpectedErrors == 0 && r.DeadlineUnfiredWrites == 0 && r.SkippedWrites == 0 && r.ScheduledWrites == r.PlannedWrites && r.Acknowledged == r.PeerApplied && r.PeerApplied >= 200 && r.PeerHTTPPercentiles != nil && r.PeerHTTPPercentiles.P95 < 1 && float64(r.MeasurementHTTP.UnexpectedErrors)/float64(r.MeasurementHTTP.Attempts) < .001
}
func ReadResult(path string) (CellResult, error) {
	var r CellResult
	if err := privateRegular(path); err != nil {
		return r, err
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 16<<20 {
		return r, errors.New("capacity_result_invalid")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return r, errors.New("capacity_result_invalid")
	}
	switch r.Completion {
	case Complete, Interrupted, Deadline, SetupFailed, SampleIncomplete, InvalidResult, CleanupFailed, Gated, Unmeasured:
	default:
		return r, errors.New("capacity_result_invalid")
	}
	for _, v := range append(append([]float64{r.MeasurementSeconds, r.SetupSeconds, r.DrainSeconds}, r.LatencySeconds...), r.PeerHTTPSeconds...) {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return r, errors.New("capacity_result_invalid")
		}
	}
	for _, v := range []int{r.Attempts, r.Successes, r.Refusals, r.UnexpectedErrors, r.Acknowledged, r.PeerApplied, r.Setup.Attempts, r.Setup.Refusals, r.Warmup.Refusals, r.SkippedWrites} {
		if v < 0 {
			return r, errors.New("capacity_result_invalid")
		}
	}
	for key, count := range r.ErrorCauses {
		valid := false
		for _, phase := range []string{"setup", "setup_mutation", "setup_peer", "warmup_mutation", "warmup_peer", "warmup_empty_pull", "measurement_mutation", "measurement_peer", "measurement_empty_pull", "refresh", "refresh_reconnect"} {
			prefix := phase + "_"
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			for _, cause := range []string{"deadline_exceeded", "canceled", "http_401", "http_403", "http_409", "http_429", "http_500", "http_503", "server_busy", "http_other", "client_timeout", "protocol_decode", "transport", "mutation_not_applied", "identity_or_unknown_change", "peer_effect_mismatch", "refresh_identity", "other"} {
				if strings.TrimPrefix(key, prefix) == cause {
					valid = true
				}
			}
		}
		if !valid || count < 1 {
			return r, errors.New("capacity_result_invalid")
		}
	}
	if r.Completion == Complete || r.Completion == SampleIncomplete {
		expectedHTTP := Counts{}
		for _, c := range []Counts{r.Counts, r.PeerPulls, r.EmptyPulls, r.MeasuredRefresh} {
			expectedHTTP.Attempts += c.Attempts
			expectedHTTP.Successes += c.Successes
			expectedHTTP.Refusals += c.Refusals
			expectedHTTP.GlobalAdmissionRefusals += c.GlobalAdmissionRefusals
			expectedHTTP.UnexpectedErrors += c.UnexpectedErrors
		}
		if r.MeasurementHTTP != expectedHTTP || r.DeadlineUnfiredWrites != r.PlannedWrites-r.ScheduledWrites || r.AcknowledgedWithoutPeer != r.Acknowledged-r.PeerApplied || r.UnresolvedWrites != r.ScheduledWrites-r.SkippedWrites-r.Attempts || r.UnfiredEmptyPulls != r.PlannedEmptyPulls-r.EmptyPulls.Attempts || r.UnfiredRefresh != r.PlannedRefresh-r.MeasuredRefresh.Attempts || r.UnfiredReconnect != r.PlannedReconnect-r.MeasuredReconnect.Attempts {
			r.Completion = InvalidResult
		}
	}
	if r.RefusalClassification == "" {
		r.RefusalClassification = "unavailable_legacy"
	}
	r.Finalize()
	return r, nil
}
