package database_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	testOrigin   = "https://sync.example.test"
	testInstance = "50000000-0000-4000-8000-000000000001"
)

func TestAuthChallengeHasExactlyOneWinnerAcross100Consumers(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	digest := sha256.Sum256([]byte("single-use challenge"))
	if err := fixture.Runtime.QueryRow(t.Context(), `SELECT app.create_auth_challenge($1,$2,$3,$4,$5)`,
		digest[:], testOrigin, testInstance, now.Add(5*time.Minute), now).Scan(new(string)); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wins atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			var challengeID string
			err := fixture.Runtime.QueryRow(t.Context(), `SELECT app.consume_auth_challenge($1,$2,$3,$4)`,
				digest[:], testOrigin, testInstance, now.Add(time.Second)).Scan(&challengeID)
			if err == nil {
				wins.Add(1)
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "P0002" {
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if wins.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("challenge wins=%d unexpected_errors=%d", wins.Load(), unexpected.Load())
	}

	// Only the canonical all-zero UUID is forbidden. This catches accidental
	// reintroduction of a second UUID sentinel.
	otherDigest := sha256.Sum256([]byte("valid variant UUID"))
	if err := fixture.Runtime.QueryRow(t.Context(), `SELECT app.create_auth_challenge($1,$2,$3,$4,$5)`,
		otherDigest[:], testOrigin, "00000000-0000-0000-8000-000000000000", now.Add(5*time.Minute), now).Scan(new(string)); err != nil {
		t.Fatalf("valid nonzero UUID was rejected: %v", err)
	}
}

func TestTakeRateLimitsIsWeightedAtomicAndCappedAcross100Callers(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	windowEnd := now.Add(10 * time.Minute)
	key := hex.EncodeToString(hashBytes("concurrent-rate-key"))
	charges := rateCharges(t, rateCharge{
		Scope: "auth-challenge-ip", KeyDigest: key, Cost: 2, Limit: 50,
		WindowStart: now, WindowEnd: windowEnd,
	})

	start := make(chan struct{})
	var allowed atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			var ok bool
			var remaining, retry int
			if err := fixture.Runtime.QueryRow(t.Context(), `SELECT * FROM app.take_rate_limits($1,$2)`, charges, now).
				Scan(&ok, &remaining, &retry); err != nil {
				unexpected.Add(1)
				return
			}
			if ok {
				allowed.Add(1)
			} else if remaining != 0 || retry != 600 {
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if allowed.Load() != 25 || unexpected.Load() != 0 {
		t.Fatalf("allowed=%d unexpected=%d", allowed.Load(), unexpected.Load())
	}
	var count int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count FROM rate_limit_buckets
		WHERE scope='auth-challenge-ip' AND key_digest=$1`, hashBytes("concurrent-rate-key")).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 50 {
		t.Fatalf("stored weighted count=%d want=50", count)
	}

	// Charge A would succeed and charge B is already full. The denial must
	// rollback A rather than leaking a partial admission.
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	for i := range keyB {
		keyB[i] = 0xff
	}
	prefill := rateCharges(t, rateCharge{
		Scope: "auth-refresh-account", KeyDigest: hex.EncodeToString(keyB), Cost: 1, Limit: 1,
		WindowStart: now, WindowEnd: windowEnd,
	})
	assertRateDecision(t, fixture, prefill, now, true, 0, 0)
	multi := rateCharges(t,
		rateCharge{Scope: "auth-refresh-account", KeyDigest: hex.EncodeToString(keyA), Cost: 3, Limit: 10, WindowStart: now, WindowEnd: windowEnd},
		rateCharge{Scope: "auth-refresh-account", KeyDigest: hex.EncodeToString(keyB), Cost: 1, Limit: 1, WindowStart: now, WindowEnd: windowEnd},
	)
	assertRateDecision(t, fixture, multi, now, false, 0, 600)
	var leaked int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM rate_limit_buckets
		WHERE scope='auth-refresh-account' AND key_digest=$1`, keyA).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("denied multi-charge leaked %d first buckets", leaked)
	}

	// Retry-After is derived from the latest blocking charge, not merely the
	// first canonical key encountered.
	lateA, lateB := make([]byte, 32), make([]byte, 32)
	lateA[0], lateB[0] = 0x11, 0x22
	assertRateDecision(t, fixture, rateCharges(t, rateCharge{
		Scope: "auth-refresh-device", KeyDigest: hex.EncodeToString(lateA), Cost: 1, Limit: 1,
		WindowStart: now, WindowEnd: now.Add(5 * time.Minute),
	}), now, true, 0, 0)
	assertRateDecision(t, fixture, rateCharges(t, rateCharge{
		Scope: "auth-refresh-device", KeyDigest: hex.EncodeToString(lateB), Cost: 1, Limit: 1,
		WindowStart: now, WindowEnd: windowEnd,
	}), now, true, 0, 0)
	bothBlocked := rateCharges(t,
		rateCharge{Scope: "auth-refresh-device", KeyDigest: hex.EncodeToString(lateA), Cost: 1, Limit: 1, WindowStart: now, WindowEnd: now.Add(5 * time.Minute)},
		rateCharge{Scope: "auth-refresh-device", KeyDigest: hex.EncodeToString(lateB), Cost: 1, Limit: 1, WindowStart: now, WindowEnd: windowEnd},
	)
	assertRateDecision(t, fixture, bothBlocked, now, false, 0, 600)
}

func TestReserveAppleNotificationRetainedKeysAtomicAmbiguityAndRestore(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	current, previous := hashBytes("notification-current"), hashBytes("notification-previous")
	firstCommand := "30000000-0000-4000-8000-000000000100"
	firstCandidates := notificationCandidates(t, notificationCandidate{"notify.v1", hex.EncodeToString(previous)})
	command, inserted := reserveNotification(t, fixture, firstCandidates, "notify.v1", previous, firstCommand, now)
	if command != firstCommand || !inserted {
		t.Fatalf("first reservation command=%s inserted=%t", command, inserted)
	}

	rotatedCandidates := notificationCandidates(t,
		notificationCandidate{"notify.v2", hex.EncodeToString(current)},
		notificationCandidate{"notify.v1", hex.EncodeToString(previous)},
	)
	command, inserted = reserveNotification(t, fixture, rotatedCandidates, "notify.v2", current,
		"30000000-0000-4000-8000-000000000101", now.Add(time.Second))
	if command != firstCommand || inserted {
		t.Fatalf("retained-key reservation command=%s inserted=%t", command, inserted)
	}
	var rows int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM apple_notification_dedup`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("dedup rows=%d err=%v", rows, err)
	}

	// Concurrent retries with different proposed commands all converge on the
	// one stored command. A unique violation is not a valid public outcome.
	raceDigest := hashBytes("notification-race")
	raceCandidates := notificationCandidates(t, notificationCandidate{"notify.v3", hex.EncodeToString(raceDigest)})
	start := make(chan struct{})
	var raceFailures atomic.Int32
	results := make(chan string, 100)
	var wait sync.WaitGroup
	for i := range 100 {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			var got string
			var fresh bool
			err := fixture.Runtime.QueryRow(t.Context(), `SELECT * FROM app.reserve_apple_notification($1,$2,$3,$4,$5)`,
				raceCandidates, "notify.v3", raceDigest, fmt.Sprintf("30000000-0000-4000-8000-%012x", 0x200+i), now.Add(2*time.Second)).Scan(&got, &fresh)
			if err != nil {
				raceFailures.Add(1)
				return
			}
			results <- got
		}(i)
	}
	close(start)
	wait.Wait()
	close(results)
	var winner string
	for got := range results {
		if winner == "" {
			winner = got
		} else if got != winner {
			raceFailures.Add(1)
		}
	}
	if raceFailures.Load() != 0 || winner == "" {
		t.Fatalf("notification race winner=%q failures=%d", winner, raceFailures.Load())
	}

	// Multiple retained-key matches are ambiguous and fail without inserting
	// the current candidate.
	secondOld := hashBytes("notification-second-old")
	reserveNotification(t, fixture, notificationCandidates(t, notificationCandidate{"notify.v4", hex.EncodeToString(secondOld)}),
		"notify.v4", secondOld, "30000000-0000-4000-8000-000000000400", now.Add(3*time.Second))
	ambiguousCurrent := hashBytes("notification-ambiguous-current")
	ambiguous := notificationCandidates(t,
		notificationCandidate{"notify.v5", hex.EncodeToString(ambiguousCurrent)},
		notificationCandidate{"notify.v1", hex.EncodeToString(previous)},
		notificationCandidate{"notify.v4", hex.EncodeToString(secondOld)},
	)
	var ignoredCommand string
	var ignoredFresh bool
	err := fixture.Runtime.QueryRow(t.Context(), `SELECT * FROM app.reserve_apple_notification($1,$2,$3,$4,$5)`, ambiguous,
		"notify.v5", ambiguousCurrent, "30000000-0000-4000-8000-000000000401", now.Add(4*time.Second)).Scan(&ignoredCommand, &ignoredFresh)
	assertSQLState(t, err, "23505")
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM apple_notification_dedup
		WHERE notification_key_id='notify.v5'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("ambiguous reservation inserted current row=%d err=%v", rows, err)
	}

	// Dedup is deliberately only an optimization: after restore loss, SQL
	// reserves again and the ledger command receipt remains final authority.
	if _, err := fixture.Owner.Exec(t.Context(), `DELETE FROM apple_notification_dedup
		WHERE notification_key_id='notify.v1' AND notification_digest=$1`, previous); err != nil {
		t.Fatal(err)
	}
	restoredCommand := "30000000-0000-4000-8000-000000000500"
	command, inserted = reserveNotification(t, fixture, firstCandidates, "notify.v1", previous, restoredCommand, now.Add(5*time.Second))
	if command != restoredCommand || !inserted {
		t.Fatalf("post-restore reservation command=%s inserted=%t", command, inserted)
	}
}

func TestStageAuthMaterialCanonicalBytesDuplicateKeysAndConflictingRetry(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	canonical := canonicalExchangeMaterial(now)
	commandID := "30000000-0000-4000-8000-000000000600"
	stageBytes(t, fixture, commandID, canonical, now)
	stageBytes(t, fixture, commandID, canonical, now)

	changed := []byte(string(canonical))
	changed = []byte(replaceOnce(t, string(changed), `"identity_subject":"subject-fixture"`, `"identity_subject":"changed-subject"`))
	err := stageBytesError(t, fixture, commandID, changed, now)
	assertSQLState(t, err, "23505")
	stageBytes(t, fixture, commandID, canonical, now.Add(time.Second))
	var createdAt, expiresAt time.Time
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT created_at,expires_at FROM auth_exchange_material WHERE command_id=$1`, commandID).
		Scan(&createdAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if !createdAt.Equal(now) || !expiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("exact restage changed lifetime created=%s expires=%s", createdAt, expiresAt)
	}

	for name, invalid := range map[string][]byte{
		"leading whitespace": append([]byte(" "), canonical...),
		"duplicate escaped-equivalent key": []byte(replaceOnce(t, string(canonical),
			`{"version":"v1",`, `{"version":"v1","ver\u0073ion":"v1",`)),
		"unknown key": []byte(replaceOnce(t, string(canonical),
			`{"version":"v1",`, `{"future":true,"version":"v1",`)),
	} {
		t.Run(name, func(t *testing.T) {
			err := stageBytesError(t, fixture, fmt.Sprintf("30000000-0000-4000-8000-%012x", 0x700+len(name)), invalid, now)
			assertSQLState(t, err, "22023")
		})
	}

	var storedDigest []byte
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT projection_material_digest FROM auth_exchange_material WHERE command_id=$1`, commandID).Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(canonical)
	if hex.EncodeToString(storedDigest) != hex.EncodeToString(want[:]) {
		t.Fatalf("stored material digest=%x want=%x", storedDigest, want)
	}
}

func TestAccountIdentityConstraintRejectsIncompleteMaterialAndCredentialQuartets(t *testing.T) {
	fixture := testpg.New(t)
	accountID := "10000000-0000-4000-8000-000000000099"
	if _, err := fixture.Owner.Exec(t.Context(), `SELECT set_config('app.account_id',$1,false)`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), `INSERT INTO accounts(id,display_name) VALUES($1,'Constraint')`, accountID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	insert := func(audience, subject, credentialID, credentialKeyID, purpose, objectID any) error {
		_, err := fixture.Owner.Exec(t.Context(), `INSERT INTO account_identities(
			provider,identity_key_id,identity_digest,account_id,identity_audience,identity_subject,
			apple_credential_id,apple_credential_key_id,apple_credential_purpose,apple_credential_object_id,
			material_missing,created_at,updated_at)
			VALUES('apple','identity.v1',decode(repeat('aa',32),'hex'),$1,$2,$3,$4,$5,$6,$7,false,$8,$8)`,
			accountID, audience, subject, credentialID, credentialKeyID, purpose, objectID, now)
		return err
	}
	assertSQLState(t, insert(nil, "subject", nil, nil, nil, nil), "23514")
	assertSQLState(t, insert("audience", nil, nil, nil, nil, nil), "23514")
	credentialValues := []any{"credential", "credential-key", "apple-refresh-revocation-v1", "90000000-0000-4000-8000-000000000001"}
	for mask := 1; mask < 15; mask++ {
		values := make([]any, 4)
		for index := range values {
			if mask&(1<<index) != 0 {
				values[index] = credentialValues[index]
			}
		}
		t.Run(fmt.Sprintf("partial_quartet_%02d", mask), func(t *testing.T) {
			assertSQLState(t, insert("audience", "subject", values[0], values[1], values[2], values[3]), "23514")
		})
	}
}

func TestAuthExchangeMaterialConstraintRejectsUnknownAndPartialPerKindFields(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	for name, mutate := range map[string]func(*ownerAuthMaterial){
		"missing identity provider": func(row *ownerAuthMaterial) { row.identityProvider = nil },
		"missing identity key":      func(row *ownerAuthMaterial) { row.identityKeyID = nil },
		"missing identity digest":   func(row *ownerAuthMaterial) { row.identityDigest = nil },
		"missing identity audience": func(row *ownerAuthMaterial) { row.identityAudience = nil },
		"missing identity subject":  func(row *ownerAuthMaterial) { row.identitySubject = nil },
		"missing profile confirmed": func(row *ownerAuthMaterial) { row.profileConfirmed = nil },
		"missing register account":  func(row *ownerAuthMaterial) { row.registerAccount = nil },
		"missing register device":   func(row *ownerAuthMaterial) { row.registerDevice = nil },
	} {
		t.Run(name, func(t *testing.T) {
			row := validOwnerExchangeMaterial(now)
			mutate(&row)
			assertSQLState(t, insertOwnerAuthMaterial(t, fixture, row), "23514")
		})
	}

	for mask := 1; mask < 3; mask++ {
		t.Run(fmt.Sprintf("partial_installation_%02d", mask), func(t *testing.T) {
			row := validOwnerExchangeMaterial(now)
			row.installationKeyID, row.installationDigest = nil, nil
			if mask&1 != 0 {
				row.installationKeyID = "install.v1"
			}
			if mask&2 != 0 {
				row.installationDigest = hashBytes("installation")
			}
			assertSQLState(t, insertOwnerAuthMaterial(t, fixture, row), "23514")
		})
		t.Run(fmt.Sprintf("partial_continuity_%02d", mask), func(t *testing.T) {
			row := validOwnerExchangeMaterial(now)
			row.continuityKeyID, row.continuityDigest = nil, nil
			if mask&1 != 0 {
				row.continuityKeyID = "device.v1"
			}
			if mask&2 != 0 {
				row.continuityDigest = hashBytes("continuity")
			}
			assertSQLState(t, insertOwnerAuthMaterial(t, fixture, row), "23514")
		})
	}

	appleValues := []any{"credential", "credential-key", "apple-refresh-revocation-v1", "90000000-0000-4000-8000-000000000001"}
	for mask := 1; mask < 15; mask++ {
		t.Run(fmt.Sprintf("partial_apple_%02d", mask), func(t *testing.T) {
			row := validOwnerExchangeMaterial(now)
			if mask&1 != 0 {
				row.appleCredentialID = appleValues[0]
			}
			if mask&2 != 0 {
				row.appleCredentialKeyID = appleValues[1]
			}
			if mask&4 != 0 {
				row.appleCredentialPurpose = appleValues[2]
			}
			if mask&8 != 0 {
				row.appleCredentialObjectID = appleValues[3]
			}
			assertSQLState(t, insertOwnerAuthMaterial(t, fixture, row), "23514")
		})
	}

	for name, mutate := range map[string]func(*ownerAuthMaterial){
		"refresh identity provider":   func(row *ownerAuthMaterial) { row.identityProvider = "apple" },
		"refresh identity key":        func(row *ownerAuthMaterial) { row.identityKeyID = "identity.v1" },
		"refresh identity digest":     func(row *ownerAuthMaterial) { row.identityDigest = hashBytes("identity") },
		"refresh identity audience":   func(row *ownerAuthMaterial) { row.identityAudience = "audience" },
		"refresh identity subject":    func(row *ownerAuthMaterial) { row.identitySubject = "subject" },
		"refresh display name":        func(row *ownerAuthMaterial) { row.displayName = "Parent" },
		"refresh profile confirmed":   func(row *ownerAuthMaterial) { row.profileConfirmed = false },
		"refresh register account":    func(row *ownerAuthMaterial) { row.registerAccount = false },
		"refresh register device":     func(row *ownerAuthMaterial) { row.registerDevice = false },
		"refresh installation key":    func(row *ownerAuthMaterial) { row.installationKeyID = "install.v1" },
		"refresh installation digest": func(row *ownerAuthMaterial) { row.installationDigest = hashBytes("installation") },
		"refresh continuity key":      func(row *ownerAuthMaterial) { row.continuityKeyID = "device.v1" },
		"refresh continuity digest":   func(row *ownerAuthMaterial) { row.continuityDigest = hashBytes("continuity") },
		"refresh Apple credential id": func(row *ownerAuthMaterial) { row.appleCredentialID = "credential" },
		"refresh Apple key id":        func(row *ownerAuthMaterial) { row.appleCredentialKeyID = "credential-key" },
		"refresh Apple purpose":       func(row *ownerAuthMaterial) { row.appleCredentialPurpose = "apple-refresh-revocation-v1" },
		"refresh Apple object":        func(row *ownerAuthMaterial) { row.appleCredentialObjectID = "90000000-0000-4000-8000-000000000001" },
	} {
		t.Run(name, func(t *testing.T) {
			row := validOwnerRefreshMaterial(now)
			mutate(&row)
			assertSQLState(t, insertOwnerAuthMaterial(t, fixture, row), "23514")
		})
	}
}

type ownerAuthMaterial struct {
	kind                    string
	parentSessionID         any
	identityProvider        any
	identityKeyID           any
	identityDigest          any
	identityAudience        any
	identitySubject         any
	displayName             any
	profileConfirmed        any
	registerAccount         any
	registerDevice          any
	installationKeyID       any
	installationDigest      any
	continuityKeyID         any
	continuityDigest        any
	appleCredentialID       any
	appleCredentialKeyID    any
	appleCredentialPurpose  any
	appleCredentialObjectID any
	now                     time.Time
}

func validOwnerExchangeMaterial(now time.Time) ownerAuthMaterial {
	return ownerAuthMaterial{
		kind: "exchange", identityProvider: "apple", identityKeyID: "identity.v1", identityDigest: hashBytes("identity"),
		identityAudience: "audience", identitySubject: "subject", profileConfirmed: false,
		registerAccount: true, registerDevice: true, installationKeyID: "install.v1", installationDigest: hashBytes("installation"),
		continuityKeyID: "device.v1", continuityDigest: hashBytes("continuity"), now: now,
	}
}

func validOwnerRefreshMaterial(now time.Time) ownerAuthMaterial {
	return ownerAuthMaterial{kind: "refresh", parentSessionID: "60000000-0000-4000-8000-000000000002", now: now}
}

func insertOwnerAuthMaterial(t *testing.T, fixture *testpg.Fixture, row ownerAuthMaterial) error {
	t.Helper()
	_, err := fixture.Owner.Exec(t.Context(), `INSERT INTO auth_exchange_material(
		command_id,command_hash,projection_material_digest,material_kind,account_id,device_id,session_id,parent_session_id,
		lineage_root_session_id,server_instance_id,canonical_origin,token_key_id,access_digest,refresh_digest,recovery_epoch,
		security_revision,scope,access_expires_at,refresh_expires_at,reauthenticate_at,absolute_expires_at,
		identity_provider,identity_key_id,identity_digest,identity_audience,identity_subject,display_name,profile_confirmed,
		register_account,register_device,installation_key_id,installation_digest,continuity_key_id,continuity_digest,
		apple_credential_id,apple_credential_key_id,apple_credential_purpose,apple_credential_object_id,created_at,expires_at)
		VALUES(gen_random_uuid(),decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),$1,
		'10000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000001',
		'60000000-0000-4000-8000-000000000099',$2,'60000000-0000-4000-8000-000000000099',
		'50000000-0000-4000-8000-000000000001','https://sync.example.test','session.v1',decode(repeat('03',32),'hex'),
		decode(repeat('04',32),'hex'),1,1,'account',$3,$4,$5,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		row.kind, row.parentSessionID, row.now.Add(5*time.Minute), row.now.Add(time.Hour), row.now.Add(24*time.Hour),
		row.identityProvider, row.identityKeyID, row.identityDigest, row.identityAudience, row.identitySubject, row.displayName,
		row.profileConfirmed, row.registerAccount, row.registerDevice, row.installationKeyID, row.installationDigest,
		row.continuityKeyID, row.continuityDigest, row.appleCredentialID, row.appleCredentialKeyID,
		row.appleCredentialPurpose, row.appleCredentialObjectID, row.now, row.now.Add(10*time.Minute))
	return err
}

type rateCharge struct {
	Scope       string    `json:"scope"`
	KeyDigest   string    `json:"key_digest"`
	Cost        int       `json:"cost"`
	Limit       int       `json:"limit"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
}

type notificationCandidate struct {
	KeyID  string `json:"key_id"`
	Digest string `json:"digest"`
}

func rateCharges(t *testing.T, charges ...rateCharge) []byte {
	t.Helper()
	bytes, err := json.Marshal(charges)
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

func notificationCandidates(t *testing.T, candidates ...notificationCandidate) []byte {
	t.Helper()
	bytes, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

func assertRateDecision(t *testing.T, fixture *testpg.Fixture, charges []byte, now time.Time, wantAllowed bool, wantRemaining, wantRetry int) {
	t.Helper()
	var allowed bool
	var remaining, retry int
	if err := fixture.Runtime.QueryRow(t.Context(), `SELECT * FROM app.take_rate_limits($1,$2)`, charges, now).
		Scan(&allowed, &remaining, &retry); err != nil {
		t.Fatal(err)
	}
	if allowed != wantAllowed || remaining != wantRemaining || retry != wantRetry {
		t.Fatalf("rate decision=(%t,%d,%d) want=(%t,%d,%d)", allowed, remaining, retry, wantAllowed, wantRemaining, wantRetry)
	}
}

func reserveNotification(t *testing.T, fixture *testpg.Fixture, candidates []byte, keyID string, digest []byte, commandID string, at time.Time) (string, bool) {
	t.Helper()
	var got string
	var fresh bool
	if err := fixture.Runtime.QueryRow(t.Context(), `SELECT * FROM app.reserve_apple_notification($1,$2,$3,$4,$5)`,
		candidates, keyID, digest, commandID, at).Scan(&got, &fresh); err != nil {
		t.Fatal(err)
	}
	return got, fresh
}

func hashBytes(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func canonicalExchangeMaterial(now time.Time) []byte {
	return []byte(fmt.Sprintf(`{"version":"v1","kind":"exchange","account_id":"10000000-0000-4000-8000-000000000001","device_id":"40000000-0000-4000-8000-000000000001","session_id":"60000000-0000-4000-8000-000000000001","lineage_root_session_id":"60000000-0000-4000-8000-000000000001","server_instance_id":"50000000-0000-4000-8000-000000000001","canonical_origin":"https://sync.example.test","token_key_id":"session.v1","access_digest":"%s","refresh_digest":"%s","recovery_epoch":1,"security_revision":1,"scope":"account","family_id":null,"access_expires_at":"%s","refresh_expires_at":"%s","reauthenticate_at":"%s","absolute_expires_at":"%s","identity_provider":"apple","identity_key_id":"identity.v1","identity_digest":"%s","identity_audience":"com.example.babytracker","identity_subject":"subject-fixture","display_name":null,"profile_confirmed":false,"register_account":true,"register_device":true,"installation_key_id":null,"installation_digest":null,"continuity_key_id":"device.v1","continuity_digest":"%s","apple_credential_id":null,"apple_credential_key_id":null,"apple_credential_purpose":null,"apple_credential_object_id":null}`,
		hex.EncodeToString(hashBytes("access")), hex.EncodeToString(hashBytes("refresh")),
		now.Add(10*time.Minute).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano),
		now.Add(24*time.Hour).Format(time.RFC3339Nano), now.Add(24*time.Hour).Format(time.RFC3339Nano),
		hex.EncodeToString(hashBytes("identity")), hex.EncodeToString(hashBytes("continuity"))))
}

func stageBytes(t *testing.T, fixture *testpg.Fixture, commandID string, material []byte, now time.Time) {
	t.Helper()
	if err := stageBytesError(t, fixture, commandID, material, now); err != nil {
		t.Fatal(err)
	}
}

func stageBytesError(t *testing.T, fixture *testpg.Fixture, commandID string, material []byte, now time.Time) error {
	t.Helper()
	commandHash := sha256.Sum256([]byte("command"))
	materialDigest := sha256.Sum256(material)
	_, err := fixture.Runtime.Exec(t.Context(), `SELECT app.stage_auth_material($1,$2,$3,$4,$5)`,
		commandID, commandHash[:], materialDigest[:], material, now)
	return err
}

func assertSQLState(t *testing.T, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != want {
		t.Fatalf("error=%v, want SQLSTATE %s", err, want)
	}
}

func replaceOnce(t *testing.T, source, old, replacement string) string {
	t.Helper()
	if len(source) == 0 || old == "" {
		t.Fatal("invalid replacement fixture")
	}
	for i := 0; i+len(old) <= len(source); i++ {
		if source[i:i+len(old)] == old {
			return source[:i] + replacement + source[i+len(old):]
		}
	}
	t.Fatalf("fixture substring %q not found", old)
	return ""
}
