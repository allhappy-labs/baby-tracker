//go:build identitytest

// Package loadseed seeds a disposable database with families as the
// product itself would hold them, for the Phase 5 load runs: accounts,
// devices, families, invitations, entitlements and integrations through the
// product's own commands; care history encoded by care.Encode from the
// mutations a phone would send and bulk-loaded with COPY; side tables
// through the services their routes call; a consumed-session backlog; and a
// verification pass. It returns the fleet: every seeded device's private
// state, for `baby-tracker-load run`.
//
// Determinism: Spec.Seed fixes every shape, timestamp and care payload.
// The IDs the product's commands create (accounts, devices, families,
// tokens) are random by the product's design.
//
// The package builds only with the identitytest tag: it signs in through
// the test identity issuer, which exists only in that build.
package loadseed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// HistoryProfile is the tracked care history of a seeded family.
//
// DaysWeights is the tracked length (days) of a family's history and its
// weight; the rates are per baby and day (feeds, diapers, sleeps,
// supplements) or per pumping family and day (pumps). A measurement is
// logged weekly, immunizations follow the schedule for the baby's age and
// every baby has one growth profile. UpdateShare and TombstoneShare are the
// shares of records edited (to version 2-4) and deleted;
// RunningTimerShare is the share of babies with a running timer,
// RunningPumpShare the share of families with a running pump.
type HistoryProfile struct {
	DaysWeights                                                 map[int]float64
	FeedsPerDay, DiapersPerDay, SleepsPerDay, SupplementsPerDay float64
	PumpFamilyShare, PumpsPerDay, UpdateShare, TombstoneShare   float64
	RunningTimerShare, RunningPumpShare                         float64
}

// Spec is one seed: Families families of four devices each, their history,
// and the side tables and session backlog of the load the next runs drive.
//
//   - BacklogHours and BacklogLoad size the consumed-session backlog:
//     round(BacklogLoad × devices × BacklogHours × 6) rows over the
//     BacklogHours before AsOf (a device refreshes about every ten minutes
//     at 1x).
//   - ceil(Families × MCPShare) families get an MCP grant, spread evenly;
//     PushTokenShare of devices a
//     push token. ViewerShare of families have a viewer (their fourth
//     device), TwinShare two babies.
//   - HotFamily adds one more family, after the Families others: the
//     hot-family scenario's (HotFamilyAccounts accounts × 2 devices: the
//     owner and caregivers), its history planned as family Families's.
//   - Workers is how many families are provisioned or encoded at once. It
//     shapes nothing, so it is not part of the spec's JSON or digest.
type Spec struct {
	Families                 int
	Seed                     uint64
	AsOf                     time.Time
	History                  HistoryProfile
	BacklogHours             int
	BacklogLoad              float64
	MCPShare, PushTokenShare float64
	ViewerShare, TwinShare   float64
	// HotFamily is omitted from the JSON when false, so a spec without it
	// keeps its digest.
	HotFamily bool `json:",omitempty"`
	Workers   int  `json:"-"`
}

// DevicesPerFamily is every seeded family's device count: owner 2, and
// caregiver 2 or caregiver 1 and viewer 1.
const DevicesPerFamily = 4

// The hot family's shape (Task 20's hot-family cell): the owner and four
// caregivers, two devices each.
const (
	HotFamilyAccounts          = 5
	HotFamilyDevicesPerAccount = 2
)

// SeededFamilies is how many families the spec seeds: Families, plus the
// hot family.
func (s Spec) SeededFamilies() int {
	if s.HotFamily {
		return s.Families + 1
	}
	return s.Families
}

// SeededDevices is how many devices the spec seeds.
func (s Spec) SeededDevices() int {
	n := s.Families * DevicesPerFamily
	if s.HotFamily {
		n += HotFamilyAccounts * HotFamilyDevicesPerAccount
	}
	return n
}

// MeasurementEveryDays is the measurement cadence.
const MeasurementEveryDays = 7

// immunizationSchedule is the vaccines given at each age in days, about ten
// in the first year.
var immunizationSchedule = []struct {
	AgeDays int
	Vaccine string
	Dose    int
}{
	{0, "Hepatitis B", 1}, {30, "Hepatitis B", 2},
	{60, "DTaP", 1}, {60, "Hib", 1}, {60, "Prevenar 13", 1},
	{120, "DTaP", 2}, {120, "Prevenar 13", 2},
	{180, "DTaP", 3}, {180, "Hepatitis B", 3},
	{365, "MMR", 1},
}

// R10k is the reference seed (plan "Seed profile R10k"): 10,000 families,
// histories of {7, 30, 90, 180, 365} days weighted {25, 30, 25, 15, 5}%,
// about 1,100 care entities per family.
func R10k(seed uint64, asOf time.Time) Spec {
	return Spec{
		Families: 10000, Seed: seed, AsOf: asOf.UTC(),
		History: HistoryProfile{
			DaysWeights: map[int]float64{7: 25, 30: 30, 90: 25, 180: 15, 365: 5},
			FeedsPerDay: 5, DiapersPerDay: 4, SleepsPerDay: 2.5, SupplementsPerDay: 0.3,
			PumpFamilyShare: 0.20, PumpsPerDay: 3, UpdateShare: 0.10, TombstoneShare: 0.03,
			RunningTimerShare: 0.10, RunningPumpShare: 0.05,
		},
		BacklogHours: 24, BacklogLoad: 2,
		MCPShare: 0.02, PushTokenShare: 0.90, ViewerShare: 0.25, TwinShare: 0.15,
		Workers: 16,
	}
}

// Small is R10k's shape over a 7-day history for families families, for
// tests and the Task 12 smoke.
func Small(families int, seed uint64, asOf time.Time) Spec {
	s := R10k(seed, asOf)
	s.Families = families
	s.History.DaysWeights = map[int]float64{7: 1}
	s.Workers = 4
	return s
}

// Validate refuses a spec the seeder cannot honour.
func (s Spec) Validate() error {
	var errs []error
	if s.Families < 1 || s.Families > 1_000_000 {
		errs = append(errs, fmt.Errorf("families %d outside 1..1000000", s.Families))
	}
	if s.AsOf.IsZero() || s.AsOf.Location() != time.UTC || s.AsOf.Year() < 2000 || s.AsOf.Year() > 2100 {
		errs = append(errs, errors.New("as-of must be a UTC time"))
	}
	if len(s.History.DaysWeights) == 0 {
		errs = append(errs, errors.New("history has no lengths"))
	}
	total := 0.0
	for days, weight := range s.History.DaysWeights {
		if days < 1 || days > 3650 || weight < 0 {
			errs = append(errs, fmt.Errorf("history length %d weighted %v", days, weight))
		}
		total += weight
	}
	if total <= 0 {
		errs = append(errs, errors.New("history weights sum to zero"))
	}
	h := s.History
	for name, rate := range map[string]float64{"feeds": h.FeedsPerDay, "diapers": h.DiapersPerDay, "sleeps": h.SleepsPerDay,
		"supplements": h.SupplementsPerDay, "pumps": h.PumpsPerDay} {
		if rate < 0 || rate > 50 {
			errs = append(errs, fmt.Errorf("%s per day %v outside 0..50", name, rate))
		}
	}
	for name, share := range map[string]float64{"pump family": h.PumpFamilyShare, "update": h.UpdateShare, "tombstone": h.TombstoneShare,
		"running timer": h.RunningTimerShare, "running pump": h.RunningPumpShare, "mcp": s.MCPShare, "push token": s.PushTokenShare,
		"viewer": s.ViewerShare, "twin": s.TwinShare} {
		if share < 0 || share > 1 {
			errs = append(errs, fmt.Errorf("%s share %v outside 0..1", name, share))
		}
	}
	if s.BacklogHours < 0 || s.BacklogHours > 24*31 || s.BacklogLoad < 0 || s.BacklogLoad > 10 {
		errs = append(errs, fmt.Errorf("backlog %d hours at load %v", s.BacklogHours, s.BacklogLoad))
	}
	if s.Workers < 1 || s.Workers > 256 {
		errs = append(errs, fmt.Errorf("workers %d outside 1..256", s.Workers))
	}
	return errors.Join(errs...)
}

// SHA256 is the hex SHA-256 of the spec's JSON: the fleet header's
// SeedSpecSHA256.
func (s Spec) SHA256() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// BacklogRows is the consumed-session backlog's size for devices devices.
func (s Spec) BacklogRows(devices int) int {
	return int(s.BacklogLoad*float64(devices)*float64(s.BacklogHours)*6 + 0.5)
}

// sortedDays is the history lengths in ascending order (map order is
// random; sampling must not be).
func (p HistoryProfile) sortedDays() []int {
	days := make([]int, 0, len(p.DaysWeights))
	for d := range p.DaysWeights {
		days = append(days, d)
	}
	slices.Sort(days)
	return days
}

// MeanDays is the expected tracked history length.
func (p HistoryProfile) MeanDays() float64 {
	total, weighted := 0.0, 0.0
	for _, d := range p.sortedDays() {
		total += p.DaysWeights[d]
		weighted += p.DaysWeights[d] * float64(d)
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}

// expectedImmunizations is the expected count of scheduled vaccines a baby
// tracked for its whole life of days receives.
func (p HistoryProfile) expectedImmunizations() float64 {
	total, weighted := 0.0, 0.0
	for _, d := range p.sortedDays() {
		given := 0
		for _, dose := range immunizationSchedule {
			if dose.AgeDays < d {
				given++
			}
		}
		total += p.DaysWeights[d]
		weighted += p.DaysWeights[d] * float64(given)
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}

// MeanEntitiesPerFamily is the expected care_entities rows of a family:
// per baby the baby, its growth profile, its daily records, weekly
// measurements, scheduled immunizations and running timer; per family its
// pumps and running pump. twinShare is the share of families with two
// babies.
func (p HistoryProfile) MeanEntitiesPerFamily(twinShare float64) float64 {
	days := p.MeanDays()
	perBaby := 2 + days*(p.FeedsPerDay+p.DiapersPerDay+p.SleepsPerDay+p.SupplementsPerDay) +
		days/MeasurementEveryDays + p.expectedImmunizations() + p.RunningTimerShare
	perFamily := p.PumpFamilyShare*p.PumpsPerDay*days + p.RunningPumpShare
	return perBaby*(1+twinShare) + perFamily
}
