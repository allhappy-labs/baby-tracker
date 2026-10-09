package family

import (
	"context"
	"testing"
	"time"
)

func TestSelfHostEntitlementAlwaysOnForFamilySync(t *testing.T) {
	policy := SelfHostedEntitlementPolicy{}
	if err := policy.Check(context.Background(), "family", "family_sync", time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := Code(policy.Check(context.Background(), "family", "unknown", time.Now())); code != "entitlement_required" {
		t.Fatalf("code=%q", code)
	}
}
