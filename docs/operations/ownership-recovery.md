# Operator ownership recovery and beta entitlement changes

The admin CLI provides narrowly scoped, audited recovery for a family whose
owner identity is unavailable, plus hosted-beta entitlement grant and
suspension. It does not automatically promote a caregiver, edit the database
directly, or send mail or messages. HTTP principals cannot obtain operator
scope: the configured operator identity and mounted secret files are available
only on the authority host.

Real ownership recovery is disabled until the operator has approved and
documented an identity-verification policy. The CLI mechanism is not itself
proof that a claimant owns a family.

## Configuration

Configure a canonical nonzero UUID in `BT_OPERATOR_ID`. Mount the independently
approved policy identifiers in
`BT_OPERATOR_OWNERSHIP_POLICY_ID_FILE` and/or
`BT_OPERATOR_ENTITLEMENT_POLICY_ID_FILE`, and mount the 32-byte lowercase-hex
HMAC key in `BT_OPERATOR_CONFIRMATION_KEY_FILE`.

Every policy/key file must be a regular, non-symlink file with exact Unix mode
`0600`. Policy files contain one value matching
`[A-Za-z][A-Za-z0-9._-]{0,127}`; the key file contains exactly 64 lowercase hex
characters. One final LF is allowed. Direct-value environment fallbacks are
rejected. Absence of a policy file disables only that command family.

Keep evidence documents outside Baby Tracker under the approved operator
policy. The ledger records only an HMAC of the opaque evidence reference, the
opaque operator ID, policy ID, reason, affected IDs, and revision.

## Procedure

1. Run `security verify` and, if necessary, complete the documented
   `security reconcile` procedure. Stop on any authority or projection
   disagreement.
2. Verify the family, current frozen/unavailable owner, proposed replacement,
   current entitlement inputs, and positive current security revision. The new
   owner must already be an active caregiver. Never choose a target
   automatically.
3. Create an opaque evidence reference and independently verify the evidence
   under the configured policy.
4. Run the appropriate `prepare` command. Review every field in the printed
   proposal against the evidence and current authority state.
5. Copy both the printed `confirmation.digest` and
   `confirmation.expires_at` into the matching `commit` command before the
   five-minute expiry. Do not modify or infer either value.
6. Re-run `security verify`, confirm the resulting authorization and positive
   revision, and confirm exactly one operator audit entry. For suspension,
   verify that the entitlement source remains `hosted_beta` and only its status
   changed.

An exact retry of an already committed command ID and proposal returns its
stored result even after confirmation expiry. Reusing the command ID with any
changed operation or proposal is rejected.

## Literal test vectors

The following commands are reproducible tests, not a production recovery
policy. Each scenario starts independently at revision 42 with clock
`2026-09-20T12:00:00Z`, operator ID
`90000000-0000-4000-8000-000000000001`, and a test-only confirmation key of 32
bytes `0x42`. In production, use the actual current revision and copy the
digest and expiry printed by that invocation of `prepare`.

```bash
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin ownership-recover prepare --command-id 30000000-0000-4000-8000-000000000061 --family-id 20000000-0000-4000-8000-000000000001 --old-owner-id 10000000-0000-4000-8000-000000000001 --new-owner-id 10000000-0000-4000-8000-000000000002 --policy-id owner-policy-test-v1 --evidence-reference case_test_0001 --expected-revision 42 --reason-code owner_identity_unavailable'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin ownership-recover commit --command-id 30000000-0000-4000-8000-000000000061 --family-id 20000000-0000-4000-8000-000000000001 --old-owner-id 10000000-0000-4000-8000-000000000001 --new-owner-id 10000000-0000-4000-8000-000000000002 --policy-id owner-policy-test-v1 --evidence-reference case_test_0001 --expected-revision 42 --reason-code owner_identity_unavailable --confirmation-digest 0e9030b6119c2f6834643fc0c17f5681d1ea044ab618e841d2c652b6f02ae37a --confirmation-expires-at 2026-09-20T12:05:00Z'

rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement grant prepare --command-id 30000000-0000-4000-8000-000000000062 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0001 --expected-revision 42 --reason-code beta_extension'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement grant commit --command-id 30000000-0000-4000-8000-000000000062 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0001 --expected-revision 42 --reason-code beta_extension --confirmation-digest 35c9bfa1a62100f8cc06d8d04f74feafd46cde8b944b40826e2e1d5b402f3e82 --confirmation-expires-at 2026-09-20T12:05:00Z'

rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement suspend prepare --command-id 30000000-0000-4000-8000-000000000063 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0002 --expected-revision 42 --reason-code abuse_suspension'
rtk proxy sh -c 'cd Server && go run ./cmd/baby-tracker-admin entitlement suspend commit --command-id 30000000-0000-4000-8000-000000000063 --family-id 20000000-0000-4000-8000-000000000001 --key family_sync --source hosted_beta --expires-at 2027-09-20T12:00:00Z --policy-id entitlement-policy-test-v1 --evidence-reference entitlement_test_0002 --expected-revision 42 --reason-code abuse_suspension --confirmation-digest 5ff8150ff438cc3a471ccba9b6bb23529de13080e19d97339518c9b519065dcc --confirmation-expires-at 2026-09-20T12:05:00Z'
```
