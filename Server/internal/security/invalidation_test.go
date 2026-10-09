package security

import (
	"context"
	"testing"
	"time"
)

type channelInvalidationSink chan SecurityInvalidation

func (sink channelInvalidationSink) Invalidate(_ context.Context, invalidation SecurityInvalidation) {
	sink <- invalidation
}

func TestSecurityInvalidationFollowsAuthoritativeIdentityLoss(t *testing.T) {
	h := newAuthority(t)
	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000071"))
	})
	sink := make(channelInvalidationSink, 2)
	unsubscribe := h.Service.Subscribe(sink)
	defer unsubscribe()

	executeExactPrepared(t, h, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentityUnavailable(actor, IdentityUnavailableCommandInput{Meta: preparedMeta("30000000-0000-4000-8000-000000000072"),
			Payload: IdentityAuthorityPayload{Version: "v1", AccountID: preparedAccount, IdentityKey: preparedIdentity}})
	})
	select {
	case got := <-sink:
		if got.AccountID != preparedAccount || got.Revision != 2 {
			t.Fatalf("invalidation=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing security invalidation")
	}
}

func TestSecurityInvalidationUnsubscribeIsIdempotent(t *testing.T) {
	h := newAuthority(t)
	sink := make(channelInvalidationSink, 1)
	unsubscribe := h.Service.Subscribe(sink)
	unsubscribe()
	unsubscribe()
	if count := h.Service.invalidationSubscriberCount(); count != 0 {
		t.Fatalf("subscriber count=%d", count)
	}
}
