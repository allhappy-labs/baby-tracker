package database_test

import (
	"errors"
	"testing"

	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

var lifecycleWorkerFunctions = []string{
	"app.claim_apple_revocation_job(text,timestamp with time zone,timestamp with time zone)",
	"app.validate_apple_revocation_claim(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)",
	"app.apple_credential_referenced_outside_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)",
	"app.mark_apple_provider_revoked(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)",
	"app.begin_apple_credential_cleanup(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)",
	"app.complete_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)",
	"app.retry_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,text,timestamp with time zone,timestamp with time zone)",
	"app.claim_credential_cleanup(uuid,uuid,text,uuid,text,text,text,timestamp with time zone,timestamp with time zone)",
	"app.claim_unfinished_credential_cleanup(text,timestamp with time zone,timestamp with time zone)",
	"app.validate_credential_cleanup_claim(text,text,uuid,uuid,uuid,uuid,text,text,timestamp with time zone)",
	"app.credential_cleanup_referenced(text,text,uuid,uuid,uuid,uuid,text,text,timestamp with time zone)",
	"app.finish_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,timestamp with time zone)",
	"app.retry_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,text,timestamp with time zone,timestamp with time zone)",
}

func TestLifecycleMigrationFreezesWorkerFunctionSignatures(t *testing.T) {
	fixture := testpg.New(t)
	for _, signature := range lifecycleWorkerFunctions {
		assertFunctionPrivilege(t, fixture.Revoker, signature, true)
		assertFunctionPrivilege(t, fixture.Runtime, signature, false)
		assertFunctionPrivilege(t, fixture.Projector, signature, false)
		assertPublicCannotExecute(t, fixture, signature)
	}
	assertExactAppExecutables(t, fixture, "baby_tracker_apple_revoker", lifecycleWorkerFunctions)
	for signature, want := range map[string]string{
		"app.claim_apple_revocation_job(text,timestamp with time zone,timestamp with time zone)":                             "TABLE(job_id uuid, lease_token text, operation_version text, operation_id uuid, command_id uuid, status text, account_id uuid, server_instance_id uuid, purpose text, object_id uuid, reference_id text, reference_key_id text, attempts integer, lease_expires_at timestamp with time zone)",
		"app.claim_credential_cleanup(uuid,uuid,text,uuid,text,text,text,timestamp with time zone,timestamp with time zone)": "TABLE(lease_token text, operation_version text, operation_id uuid, command_id uuid, account_id uuid, object_id uuid, reference_id text, reference_key_id text)",
		"app.claim_unfinished_credential_cleanup(text,timestamp with time zone,timestamp with time zone)":                    "TABLE(lease_token text, operation_version text, operation_id uuid, command_id uuid, account_id uuid, server_instance_id uuid, purpose text, object_id uuid, reference_id text, reference_key_id text)",
	} {
		var got string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_function_result($1::regprocedure)`, signature).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("result schema %s=%q want=%q", signature, got, want)
		}
	}
}

func TestLifecycleWorkerCannotReadOrWriteTablesDirectly(t *testing.T) {
	fixture := testpg.New(t)
	for _, table := range []string{"apple_revocation_jobs", "credential_cleanup_jobs", "account_identities", "auth_exchange_material", "family_recovery_claims", "operator_audit_events"} {
		for _, query := range []string{"SELECT * FROM public." + table + " LIMIT 0", "DELETE FROM public." + table} {
			_, err := fixture.Revoker.Exec(t.Context(), query)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("%s error=%v, want permission denied", query, err)
			}
		}
	}
}

func TestFamilyRecoveryClaimsAreUniqueOnlyWhileActive(t *testing.T) {
	fixture := testpg.New(t)
	var accountUniqueConstraints int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint c
		WHERE c.conrelid='public.family_recovery_claims'::regclass AND c.contype='u'
		  AND pg_get_constraintdef(c.oid) LIKE 'UNIQUE (account_id)%'`).Scan(&accountUniqueConstraints); err != nil {
		t.Fatal(err)
	}
	if accountUniqueConstraints != 0 {
		t.Fatalf("global account uniqueness constraints=%d", accountUniqueConstraints)
	}
	var definition string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE schemaname='public'
		AND tablename='family_recovery_claims' AND indexname='family_recovery_claims_one_active_per_account_idx'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if definition != "CREATE UNIQUE INDEX family_recovery_claims_one_active_per_account_idx ON public.family_recovery_claims USING btree (account_id) WHERE (status = 'active'::text)" {
		t.Fatalf("active-claim index=%q", definition)
	}
}
