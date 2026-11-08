package family_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The property's size. The defaults keep a run to a few minutes (one
// disposable database per seed) so it can share the machine with other
// database tests; the environment raises them for a longer search and pins
// one seed to reproduce a failure:
//
//	BT_DEADLOCK_PROPERTY_SEED=<first seed> BT_DEADLOCK_PROPERTY_SEEDS=<count>
//	BT_DEADLOCK_PROPERTY_COMMANDS=<commands per seed>
const (
	defaultPropertySeeds    = 8
	defaultPropertyCommands = 160
	propertyConcurrency     = 16
	// propertyCommandTimeout bounds one command: a lock wait that never ends
	// (PostgreSQL detects cycles after deadlock_timeout, 1s) is a stuck
	// command, reported with the others.
	propertyCommandTimeout = 2 * time.Minute
	propertyOperatorID     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// Property: 8 seeds × 160 commands, run 16 at a time over one family with 6
// members (owner, 3 caregivers, 2 viewers), a second active family, and a
// third family pending deletion whose former owner holds a recovery claim
// and two of whose former members are now members of the first family (so
// deleting either account locks two families). One caregiver has a second
// device. The commands are chosen at random from invite, redeem, change
// role, remove, leave, transfer ownership, profile update, revoke invitation,
// schedule deletion, recover, refresh, logout, entitlement grant and account
// deletion, three in four aimed at the party and target they are meant for
// (see plan), the rest by a random device on a random subject. No command
// deadlocks (40P01), waits out a lock timeout (55P03) or hangs. Every other
// failure is one of the commands' documented refusals. Afterwards every
// audit chain verifies and the node invariants hold: one owner per active
// family, at most one active family per account, slots agree with
// memberships, and session rows agree with memberships.
//
// The plan is a function of the seed; the interleaving is the scheduler's.
// A failing seed is printed with the command line that reruns it.
func TestRandomFamilyAndAccountCommandsNeverDeadlock(t *testing.T) {
	first := propertyKnob(t, "BT_DEADLOCK_PROPERTY_SEED", 0)
	seeds := propertyKnob(t, "BT_DEADLOCK_PROPERTY_SEEDS", defaultPropertySeeds)
	commands := propertyKnob(t, "BT_DEADLOCK_PROPERTY_COMMANDS", defaultPropertyCommands)
	for seed := uint64(first); seed < uint64(first+seeds); seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			h := testkit.New(t)
			world := newCommandWorld(t, h)
			rng := rand.New(rand.NewPCG(seed, 0x5eed))
			plan := world.plan(rng, commands)
			var deadlocks, timeouts, stuck, unexpected atomic.Int32
			var outcomes sync.Map // kind → *[2]atomic.Int32{committed, refused}
			sem := make(chan struct{}, propertyConcurrency)
			var wg sync.WaitGroup
			for _, cmd := range plan {
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					ctx, cancel := context.WithTimeout(t.Context(), propertyCommandTimeout)
					defer cancel()
					err := cmd.run(ctx)
					counts, _ := outcomes.LoadOrStore(cmd.kind, new([2]atomic.Int32))
					var pgErr *pgconn.PgError
					switch {
					case err == nil:
						counts.(*[2]atomic.Int32)[0].Add(1)
					case errors.As(err, &pgErr) && pgErr.Code == "40P01":
						deadlocks.Add(1)
						t.Logf("DEADLOCK %s: %v", cmd.name, err)
					// No request transaction sets lock_timeout today (only the
					// wake and maintenance workers do), so this bucket stays
					// empty unless one starts to; a lock wait that never ends
					// is caught as stuck by propertyCommandTimeout below.
					case errors.As(err, &pgErr) && pgErr.Code == "55P03":
						timeouts.Add(1)
						t.Logf("LOCK TIMEOUT %s: %v", cmd.name, err)
					case errors.Is(err, context.DeadlineExceeded):
						stuck.Add(1)
						t.Logf("STUCK %s: %v", cmd.name, err)
					case world.documentedRefusal(cmd, err):
						counts.(*[2]atomic.Int32)[1].Add(1)
					default:
						unexpected.Add(1)
						t.Logf("UNEXPECTED %s: code %q: %v", cmd.name, cmd.code(err), err)
					}
				}()
			}
			wg.Wait()
			var summary []string
			for _, kind := range commandKinds {
				if counts, ok := outcomes.Load(kind.name); ok {
					c := counts.(*[2]atomic.Int32)
					summary = append(summary, fmt.Sprintf("%s %d/%d", kind.name, c[0].Load(), c[0].Load()+c[1].Load()))
				}
			}
			t.Logf("seed %d committed/run: %s", seed, strings.Join(summary, ", "))
			if deadlocks.Load()+timeouts.Load()+stuck.Load()+unexpected.Load() > 0 {
				t.Fatalf("seed %d: deadlocks %d, lock timeouts %d, stuck %d, unexpected %d; rerun from the repository root with "+
					"BT_DEADLOCK_PROPERTY_SEED=%d BT_DEADLOCK_PROPERTY_SEEDS=1 BT_DEADLOCK_PROPERTY_COMMANDS=%d "+
					"BT_TEST_PACKAGES=./internal/family BT_TEST_RUN=TestRandomFamilyAndAccountCommandsNeverDeadlock bash Server/scripts/test-db.sh",
					seed, deadlocks.Load(), timeouts.Load(), stuck.Load(), unexpected.Load(), seed, commands)
			}
			world.assertInvariants(t)
		})
	}
}

func propertyKnob(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || (value == 0 && name != "BT_DEADLOCK_PROPERTY_SEED") {
		t.Fatalf("%s=%q: want a positive integer", name, raw)
	}
	return value
}

// device is one installation of a subject: its current principal and the
// refresh token that rotates it. Commands read the principal; refresh
// replaces both, one rotation of the device at a time (a client never
// spends its refresh token twice).
type device struct {
	subject      string
	turn         sync.Mutex
	mu           sync.Mutex
	principal    access.Principal
	refreshToken string
}

func (d *device) current() access.Principal {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.principal
}

func (d *device) token() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refreshToken
}

type invitation struct{ id, token string }

// commandWorld is the families, devices and invitations the plan's
// commands act on.
type commandWorld struct {
	t        *testing.T
	h        *testkit.Harness
	accounts *account.Service
	devices  []*device
	// primary is each subject's first device: the account it acts as is
	// the subject's account a command targets.
	primary  map[string]*device
	subjects []string
	// families are the first (active), second (active) and third
	// (deletion pending) family.
	families    [3]string
	mu          sync.Mutex
	invitations []invitation
}

func newCommandWorld(t *testing.T, h *testkit.Harness) *commandWorld {
	t.Helper()
	w := &commandWorld{t: t, h: h, accounts: &account.Service{Authority: h.Authority}, primary: map[string]*device{}}
	ctx := t.Context()
	owner := w.adopt("owner", h.Owner, h.RefreshToken("owner"))
	w.adopt("caregiver", h.Caregiver, h.RefreshToken("caregiver"))
	w.adopt("outsider", h.Outsider, h.RefreshToken("outsider"))
	w.families[0] = h.FamilyID

	// The third family: its owner deletes it after two members joined, so
	// they are its former members and its owner holds the recovery claim.
	thirdOwner := w.newDevice("third-owner")
	third, err := h.Family.Create(ctx, thirdOwner.current(), h.CommandID(), "Third family")
	if err != nil {
		t.Fatal(err)
	}
	w.families[2] = third.ID
	w.mustRefresh(thirdOwner)
	for _, subject := range []string{"cg3", "v2"} {
		w.join(w.newDevice(subject), thirdOwner, "caregiver")
	}
	if _, err := h.Family.ScheduleDeletion(ctx, thirdOwner.current(), h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	// Deletion revoked their sessions; they sign in again (the owner's new
	// session is recovery scoped).
	for _, subject := range []string{"third-owner", "cg3", "v2"} {
		w.mustSignIn(w.primary[subject])
	}
	if p := thirdOwner.current(); p.SessionScope != "family_recovery" {
		t.Fatalf("third owner's session scope %q, want family_recovery", p.SessionScope)
	}

	// The first family: owner, 3 caregivers, 2 viewers.
	w.join(w.newDevice("cg2"), owner, "caregiver")
	w.join(w.primary["cg3"], owner, "caregiver")
	w.join(w.newDevice("v1"), owner, "viewer")
	w.join(w.primary["v2"], owner, "viewer")
	// The caregiver's second device.
	second := &device{subject: "caregiver"}
	w.mustSignIn(second)
	w.devices = append(w.devices, second)

	// The second family.
	secondOwner := w.newDevice("second-owner")
	created, err := h.Family.Create(ctx, secondOwner.current(), h.CommandID(), "Second family")
	if err != nil {
		t.Fatal(err)
	}
	w.families[1] = created.ID
	w.mustRefresh(secondOwner)
	w.join(w.newDevice("second-cg"), secondOwner, "caregiver")

	// Pending invitations of both active families.
	for _, inviter := range []*device{owner, owner, owner, secondOwner, secondOwner} {
		w.mustInvite(inviter, "caregiver")
	}
	w.mustInvite(owner, "viewer")
	w.checkSetup()
	return w
}

// adopt records a harness subject's device.
func (w *commandWorld) adopt(subject string, p access.Principal, refreshToken string) *device {
	d := &device{subject: subject, principal: p, refreshToken: refreshToken}
	w.devices = append(w.devices, d)
	w.primary[subject] = d
	w.subjects = append(w.subjects, subject)
	return d
}

// newDevice signs a new subject in on its first device.
func (w *commandWorld) newDevice(subject string) *device {
	d := w.adopt(subject, access.Principal{}, "")
	w.mustSignIn(d)
	return d
}

func (w *commandWorld) mustSignIn(d *device) {
	w.t.Helper()
	bundle, err := w.signIn(w.t.Context(), d.subject)
	if err != nil {
		w.t.Fatalf("sign in %s: %v", d.subject, err)
	}
	if err := w.install(w.t.Context(), d, bundle); err != nil {
		w.t.Fatalf("sign in %s: %v", d.subject, err)
	}
}

func (w *commandWorld) mustRefresh(d *device) {
	w.t.Helper()
	if err := w.refresh(w.t.Context(), d); err != nil {
		w.t.Fatalf("refresh %s: %v", d.subject, err)
	}
}

func (w *commandWorld) mustInvite(inviter *device, role string) invitation {
	w.t.Helper()
	secret, err := w.h.Family.Invite(w.t.Context(), inviter.current(), w.h.CommandID(), role)
	if err != nil {
		w.t.Fatalf("invite by %s: %v", inviter.subject, err)
	}
	return w.remember(secret)
}

// join walks joiner through a real invitation of inviter's family and
// refreshes its session into the family's scope.
func (w *commandWorld) join(joiner, inviter *device, role string) {
	w.t.Helper()
	invite := w.mustInvite(inviter, role)
	if _, err := w.h.Family.Redeem(w.t.Context(), joiner.current(), w.h.CommandID(), invite.token, true); err != nil {
		w.t.Fatalf("%s redeems %s's invitation: %v", joiner.subject, inviter.subject, err)
	}
	w.mustRefresh(joiner)
}

func (w *commandWorld) remember(secret family.InvitationSecret) invitation {
	w.mu.Lock()
	defer w.mu.Unlock()
	invite := invitation{id: secret.Invitation.ID, token: secret.Token}
	w.invitations = append(w.invitations, invite)
	return invite
}

// invitation is one of the invitations minted so far (redeemed, revoked
// and expired ones included), chosen by pick.
func (w *commandWorld) invitation(pick uint64) invitation {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.invitations[pick%uint64(len(w.invitations))]
}

// signIn is a client's sign-in on a new installation, refreshed once into
// the scope its account's slot gives it.
func (w *commandWorld) signIn(ctx context.Context, subject string) (auth.SessionBundle, error) {
	first, err := w.h.SignInDevice(ctx, subject)
	if err != nil {
		return auth.SessionBundle{}, err
	}
	return w.h.Auth.Refresh(ctx, first.RefreshToken)
}

// install makes bundle the device's session. The rotation that issued it
// has committed, so its refresh token is the device's next one even when
// authenticating its access token fails (the principal then stays the old
// one): the device never presents a consumed token, and refresh_reused is
// never the test's own doing.
func (w *commandWorld) install(ctx context.Context, d *device, bundle auth.SessionBundle) error {
	p, err := w.h.Auth.Authenticate(ctx, bundle.AccessToken)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshToken = bundle.RefreshToken
	if err != nil {
		return err
	}
	d.principal = p
	return nil
}

// refresh rotates the device's session. A session that is gone (revoked by
// a removal, a deletion or a logout) is what makes the app sign in again:
// the device then signs in on a new installation. refresh_reused is not
// such a refusal here: the device rotates one at a time and keeps every
// committed rotation's token (install), so a reuse would be the server's
// error and is reported as unexpected.
func (w *commandWorld) refresh(ctx context.Context, d *device) error {
	d.turn.Lock()
	defer d.turn.Unlock()
	bundle, err := w.h.Auth.Refresh(ctx, d.token())
	switch auth.Code(err) {
	case "":
	case "session_revoked", "reauthentication_required":
		if bundle, err = w.signIn(ctx, d.subject); err != nil {
			return fmt.Errorf("%w: %w", errSignIn, err)
		}
	}
	if err != nil {
		return err
	}
	return w.install(ctx, d, bundle)
}

// errSignIn marks a refresh's sign-in again on a new installation.
var errSignIn = errors.New("sign in again")

// accountOf is the account a subject's first device acts as.
func (w *commandWorld) accountOf(subject string) string {
	return w.primary[subject].current().AccountID
}

// grant is an operator's entitlement grant on familyID at the family's
// current revision (stale_security_revision when another command moved it
// in between).
func (w *commandWorld) grant(ctx context.Context, familyID string, index int) error {
	var revision uint64
	if err := w.query(ctx, `SELECT audit_seq FROM families WHERE id=$1`, []any{familyID}, &revision); err != nil {
		return err
	}
	now := w.h.Now.UTC().Truncate(time.Second)
	_, err := w.h.Authority.ExecuteOperatorEntitlement(ctx, authority.OperatorEntitlement{CommandID: w.h.CommandID(), OperatorID: propertyOperatorID,
		FamilyID: familyID, Operation: "grant", Key: "family_sync", Source: "hosted_beta",
		ExpiresAt: now.Add(400*24*time.Hour + time.Duration(index)*time.Second), PolicyID: "Deadlock.Property_v1",
		EvidenceDigest: strings.Repeat("e", 64), ReasonCode: "beta_extension", ExpectedRevision: revision,
		ConfirmationExpiresAt: now.Add(5 * time.Minute)})
	return err
}

// query reads authority rows as their owner, the authority flag raised.
func (w *commandWorld) query(ctx context.Context, statement string, args []any, dest ...any) error {
	tx, err := w.h.Migrator.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		return err
	}
	return tx.QueryRow(ctx, statement, args...).Scan(dest...)
}

// list reads one text column of every row statement returns, as the
// authority rows' owner.
func (w *commandWorld) list(ctx context.Context, statement string, args ...any) ([]string, error) {
	tx, err := w.h.Migrator.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// rows is list in the test's context; an error fails the test.
func (w *commandWorld) rows(statement string) []string {
	w.t.Helper()
	out, err := w.list(w.t.Context(), statement)
	if err != nil {
		w.t.Fatalf("%s: %v", statement, err)
	}
	return out
}

// checkSetup pins the world the plan starts from.
func (w *commandWorld) checkSetup() {
	w.t.Helper()
	var members, caregivers, viewers int
	if err := w.query(w.t.Context(), `SELECT count(*), count(*) FILTER (WHERE role='caregiver'), count(*) FILTER (WHERE role='viewer')
		FROM memberships WHERE family_id=$1 AND status='active'`, []any{w.families[0]}, &members, &caregivers, &viewers); err != nil {
		w.t.Fatal(err)
	}
	var formerInBoth int
	if err := w.query(w.t.Context(), `SELECT count(*) FROM memberships a JOIN memberships c ON c.account_id=a.account_id
		WHERE a.family_id=$1 AND a.status='active' AND c.family_id=$2 AND c.status='former'`, []any{w.families[0], w.families[2]}, &formerInBoth); err != nil {
		w.t.Fatal(err)
	}
	if members != 6 || caregivers != 3 || viewers != 2 || formerInBoth != 2 {
		w.t.Fatalf("setup: first family members %d (caregivers %d, viewers %d), former third-family members in it %d; want 6 (3, 2), 2",
			members, caregivers, viewers, formerInBoth)
	}
}

// command is one step of the plan.
type command struct {
	name, kind string
	run        func(context.Context) error
	// code is the stable code of the command's error, read with the
	// package that returned it.
	code func(error) string
}

var commandKinds = []struct {
	name   string
	weight int
}{
	{"invite", 10}, {"redeem", 10}, {"change_role", 10}, {"remove", 8}, {"leave", 4}, {"transfer", 8}, {"profile", 8},
	{"revoke_invitation", 6}, {"schedule_deletion", 3}, {"recover", 5}, {"refresh", 14}, {"logout", 3}, {"entitlement_grant", 5},
	{"delete_account", 2},
}

// plan draws n commands from rng: a kind by weight, the family it is about,
// an acting device, a target subject and the role or invitation it names.
// Three in four commands are aimed: when they run they act as the party the
// command is meant for (the family's current owner, one of its members, an
// account without a family, a recovering owner) on a live target (a member
// of that family, an active invitation), as far as one exists; the rest act
// as the random device on the random subject, which the commands mostly
// refuse after taking their locks.
func (w *commandWorld) plan(rng *rand.Rand, n int) []command {
	total := 0
	for _, kind := range commandKinds {
		total += kind.weight
	}
	plan := make([]command, 0, n)
	for index := range n {
		draw, kind := rng.IntN(total), ""
		for _, candidate := range commandKinds {
			if draw < candidate.weight {
				kind = candidate.name
				break
			}
			draw -= candidate.weight
		}
		which := [4]int{0, 0, 1, 2}[rng.IntN(4)]
		familyID := w.families[which]
		random := w.devices[rng.IntN(len(w.devices))]
		target := w.subjects[rng.IntN(len(w.subjects))]
		role := [2]string{"caregiver", "viewer"}[rng.IntN(2)]
		pick := rng.Uint64()
		aimed := rng.IntN(4) != 0
		h := w.h
		cmd := command{kind: kind, code: family.Code}
		if aimed {
			cmd.name = fmt.Sprintf("#%d %s aimed at family %d", index, kind, which)
		} else {
			cmd.name = fmt.Sprintf("#%d %s by %s on %s", index, kind, random.subject, target)
		}
		// owner and member are the aimed actor and target, or the random
		// ones; invite is the aimed or random invitation.
		owner := func(ctx context.Context) *device {
			if !aimed {
				return random
			}
			return w.as(w.ownerOf(ctx, familyID), random)
		}
		member := func(ctx context.Context) string {
			if aimed {
				if id := w.memberOf(ctx, familyID, pick); id != "" {
					return id
				}
			}
			return w.accountOf(target)
		}
		invite := func(ctx context.Context) invitation {
			if !aimed {
				return w.invitation(pick)
			}
			return w.liveInvitation(ctx, pick)
		}
		switch kind {
		case "invite":
			cmd.run = func(ctx context.Context) error {
				secret, err := h.Family.Invite(ctx, owner(ctx).current(), h.CommandID(), role)
				if err == nil {
					w.remember(secret)
				}
				return err
			}
		case "redeem":
			cmd.run = func(ctx context.Context) error {
				by := random
				if aimed {
					by = w.as(w.slotless(ctx, pick), random)
				}
				_, err := h.Family.Redeem(ctx, by.current(), h.CommandID(), invite(ctx).token, true)
				return err
			}
		case "change_role":
			cmd.run = func(ctx context.Context) error {
				_, err := h.Family.ChangeRole(ctx, owner(ctx).current(), h.CommandID(), member(ctx), role)
				return err
			}
		case "remove":
			cmd.run = func(ctx context.Context) error {
				_, err := h.Family.Remove(ctx, owner(ctx).current(), h.CommandID(), member(ctx))
				return err
			}
		case "leave":
			cmd.run = func(ctx context.Context) error {
				by := random
				if aimed {
					by = w.as(member(ctx), random)
				}
				_, err := h.Family.Leave(ctx, by.current(), h.CommandID())
				return err
			}
		case "transfer":
			cmd.run = func(ctx context.Context) error {
				_, err := h.Family.Transfer(ctx, owner(ctx).current(), h.CommandID(), member(ctx))
				return err
			}
		case "profile":
			cmd.run = func(ctx context.Context) error {
				_, err := h.Family.UpdateProfile(ctx, random.current(), h.CommandID(), fmt.Sprintf("Name %d", index), true)
				return err
			}
		case "revoke_invitation":
			cmd.run = func(ctx context.Context) error {
				_, err := h.Family.RevokeInvitation(ctx, owner(ctx).current(), h.CommandID(), invite(ctx).id)
				return err
			}
		case "schedule_deletion":
			cmd.run = func(ctx context.Context) error {
				_, err := h.Family.ScheduleDeletion(ctx, owner(ctx).current(), h.CommandID(), true)
				return err
			}
		case "recover":
			cmd.run = func(ctx context.Context) error {
				by := random
				if aimed {
					by = w.recovering(random)
				}
				_, err := h.Family.Recover(ctx, by.current(), h.CommandID(), true)
				return err
			}
		case "refresh":
			cmd.code = auth.Code
			cmd.run = func(ctx context.Context) error { return w.refresh(ctx, random) }
		case "logout":
			cmd.code = auth.Code
			cmd.run = func(ctx context.Context) error {
				_, err := h.Auth.Logout(ctx, random.current(), h.CommandID())
				return err
			}
		case "entitlement_grant":
			cmd.name = fmt.Sprintf("#%d %s on family %d", index, kind, which)
			cmd.code = authority.CodeOf
			cmd.run = func(ctx context.Context) error { return w.grant(ctx, familyID, index) }
		case "delete_account":
			cmd.code = account.Code
			cmd.run = func(ctx context.Context) error {
				_, err := w.accounts.Delete(ctx, random.current(), h.CommandID(), true)
				return err
			}
		}
		plan = append(plan, cmd)
	}
	return plan
}

// as is the device acting as accountID, or fallback when there is none.
func (w *commandWorld) as(accountID string, fallback *device) *device {
	if accountID != "" {
		for _, d := range w.devices {
			if d.current().AccountID == accountID {
				return d
			}
		}
	}
	return fallback
}

// ownerOf is familyID's owner now ("" if it cannot be read).
func (w *commandWorld) ownerOf(ctx context.Context, familyID string) string {
	var owner string
	if w.query(ctx, `SELECT coalesce(owner_account_id::text,'') FROM families WHERE id=$1`, []any{familyID}, &owner) != nil {
		return ""
	}
	return owner
}

// memberOf is one of familyID's active members other than its owner now
// ("" if there is none).
func (w *commandWorld) memberOf(ctx context.Context, familyID string, pick uint64) string {
	members, _ := w.list(ctx, `SELECT account_id::text FROM memberships WHERE family_id=$1 AND status='active' AND role<>'owner' ORDER BY account_id`,
		familyID)
	return choose(members, pick)
}

// slotless is one of the active accounts a device acts as that has no
// family slot now ("" if there is none).
func (w *commandWorld) slotless(ctx context.Context, pick uint64) string {
	ids, _ := w.list(ctx, `SELECT id::text FROM accounts a WHERE status='active' AND NOT EXISTS (SELECT 1 FROM family_slots s WHERE s.account_id=a.id)`)
	free := map[string]bool{}
	for _, id := range ids {
		free[id] = true
	}
	var candidates []string
	for _, d := range w.devices {
		if id := d.current().AccountID; free[id] {
			candidates = append(candidates, id)
		}
	}
	return choose(candidates, pick)
}

// liveInvitation is one of the invitations still active now, or any
// invitation minted when none is.
func (w *commandWorld) liveInvitation(ctx context.Context, pick uint64) invitation {
	ids, _ := w.list(ctx, `SELECT id::text FROM invitations WHERE status='active'`)
	active := map[string]bool{}
	for _, id := range ids {
		active[id] = true
	}
	w.mu.Lock()
	var live []invitation
	for _, invite := range w.invitations {
		if active[invite.id] {
			live = append(live, invite)
		}
	}
	w.mu.Unlock()
	if len(live) == 0 {
		return w.invitation(pick)
	}
	return live[pick%uint64(len(live))]
}

// recovering is a device whose session is recovery scoped, or fallback.
func (w *commandWorld) recovering(fallback *device) *device {
	for _, d := range w.devices {
		if d.current().SessionScope == "family_recovery" {
			return d
		}
	}
	return fallback
}

func choose(values []string, pick uint64) string {
	if len(values) == 0 {
		return ""
	}
	return values[pick%uint64(len(values))]
}

// codes is the set of the given exported authority codes.
func codes(errs ...error) map[string]bool {
	out := make(map[string]bool, len(errs))
	for _, err := range errs {
		out[err.Error()] = true
	}
	return out
}

// staleCaller is how a command refuses a principal an earlier command
// revoked or changed (a removal, deletion, logout or rotation of its
// session, or its account deleted): the command's session re-check, or the
// guarded read a family service command makes before or after it.
var staleCaller = []error{authority.ErrSessionRevoked, authority.ErrMembershipRevoked, authority.ErrFamilyUnavailable}

// documentedRefusals are, per command, the refusals its doc comment lists
// (internal/authority/family.go, lifecycle.go, operator.go, sessions.go and
// the family, auth and account services over them), plus staleCaller.
var documentedRefusals = map[string]map[string]bool{
	// CreateInvitation; Invite reads the caller's slot first.
	"invite": codes(append(staleCaller, authority.ErrOwnerRequired, authority.ErrEntitlementRequired, authority.ErrInvitationLimit)...),
	// RedeemInvitation, in legacy order.
	"redeem": codes(append(staleCaller, authority.ErrFamilySlotOccupied, authority.ErrInvitationInvalid, authority.ErrInvitationConsumed,
		authority.ErrInvitationRevoked, authority.ErrInvitationExpired, authority.ErrCaregiverLimit, authority.ErrEntitlementRequired)...),
	"change_role": codes(append(staleCaller, authority.ErrOwnerRequired, authority.ErrEntitlementRequired)...),
	"remove":      codes(append(staleCaller, authority.ErrOwnerRequired, authority.ErrEntitlementRequired)...),
	"leave":       codes(append(staleCaller, authority.ErrOwnerRequired)...),
	// TransferOwner: a transfer to oneself is invalid_command.
	"transfer": codes(append(staleCaller, authority.ErrInvalidRequest, authority.ErrOwnerRequired, authority.ErrEntitlementRequired)...),
	// UpdateProfile runs in the caller's account transaction.
	"profile": codes(staleCaller...),
	"revoke_invitation": codes(append(staleCaller, authority.ErrInvitationInvalid, authority.ErrOwnerRequired, authority.ErrEntitlementRequired,
		authority.ErrInvitationConsumed, authority.ErrInvitationRevoked)...),
	// ScheduleDeletion: membership_revoked is a member set that changed
	// between the snapshot and the locks.
	"schedule_deletion": codes(append(staleCaller, authority.ErrOwnerRequired)...),
	// RecoverFamily, after RecoveryStatus.
	"recover": codes(append(staleCaller, authority.ErrRecoveryOnly, authority.ErrRecoveryForfeited, authority.ErrRecoveryExpired)...),
	// Refresh signs in again when the session is gone; its own refusals
	// are a stale caller's (signInRefusals are the sign-in's).
	"refresh": codes(staleCaller...),
	// Logout of a session that is no longer active is invalid_command.
	"logout": codes(append(staleCaller, authority.ErrInvalidCommand)...),
	// ExecuteOperatorEntitlement: a family no longer active, a grant that
	// does not extend the current one, a revision another command moved.
	"entitlement_grant": codes(authority.ErrFamilyUnavailable, authority.ErrInvalidCommand, authority.ErrStaleSecurityRevision),
	// DeleteAccount: the owner of an active family must transfer it first.
	"delete_account": codes(append(staleCaller, authority.ErrOwnershipTransferRequired)...),
}

// documentedRefusal reports whether err is one of cmd's documented
// refusals. service_unavailable never is.
func (w *commandWorld) documentedRefusal(cmd command, err error) bool {
	if errors.Is(err, errSignIn) {
		return signInRefusals[cmd.code(err)]
	}
	return documentedRefusals[cmd.kind][cmd.code(err)]
}

// signInRefusals are the refusals of a refresh's sign-in again (SignIn,
// internal/authority/identity.go), plus staleCaller for the rotation into
// the account's scope: security_busy, a lost race the client retries, for a
// registration's concurrent twin (the subject's other device signing in
// after the account's deletion) and for a sign-in whose resolved account
// the subject's other device deleted before the sign-in's transaction.
var signInRefusals = codes(append(staleCaller, authority.ErrSecurityBusy)...)

// assertInvariants checks every audit chain and the node invariants after
// the plan ran.
func (w *commandWorld) assertInvariants(t *testing.T) {
	t.Helper()
	report, err := authority.NewVerifier(w.h.Authority, nil).VerifyAll(t.Context())
	if err != nil || len(report.Failed) > 0 || report.Scopes == 0 {
		t.Fatalf("audit chains: %d verified, failed %v, err %v", report.Scopes, report.Failed, err)
	}
	checks := []struct{ what, query string }{
		{"active families without exactly one active owner who is the family's owner", `
			SELECT f.id::text FROM families f WHERE f.status='active' AND (
				(SELECT count(*) FROM memberships m WHERE m.family_id=f.id AND m.role='owner' AND m.status='active') <> 1
				OR NOT EXISTS (SELECT 1 FROM memberships m WHERE m.family_id=f.id AND m.account_id=f.owner_account_id AND m.role='owner' AND m.status='active'))`},
		{"active memberships of families that are not active", `
			SELECT m.family_id::text||'/'||m.account_id::text FROM memberships m JOIN families f ON f.id=m.family_id
			WHERE m.status='active' AND f.status<>'active'`},
		{"accounts with more than one active family", `
			SELECT m.account_id::text FROM memberships m WHERE m.status='active' GROUP BY m.account_id HAVING count(*) > 1`},
		{"active slots without the account's active membership", `
			SELECT s.account_id::text FROM family_slots s WHERE s.state='active' AND NOT EXISTS (
				SELECT 1 FROM memberships m WHERE m.family_id=s.family_id AND m.account_id=s.account_id AND m.status='active')`},
		{"active memberships without the account's active slot", `
			SELECT m.account_id::text FROM memberships m WHERE m.status='active' AND NOT EXISTS (
				SELECT 1 FROM family_slots s WHERE s.account_id=m.account_id AND s.family_id=m.family_id AND s.state='active')`},
		{"recovery slots without a deletion-pending family and the account's active claim", `
			SELECT s.account_id::text FROM family_slots s WHERE s.state='family_recovery' AND NOT EXISTS (
				SELECT 1 FROM families f JOIN family_recovery_claims c ON c.family_id=f.id
				WHERE f.id=s.family_id AND f.status='deletion_pending' AND c.account_id=s.account_id AND c.status='active')`},
		{"slots or active memberships of accounts that are not active", `
			SELECT a.id::text FROM accounts a WHERE a.status<>'active' AND (
				EXISTS (SELECT 1 FROM family_slots s WHERE s.account_id=a.id)
				OR EXISTS (SELECT 1 FROM memberships m WHERE m.account_id=a.id AND m.status='active'))`},
		{"active family sessions whose family, slot or role disagree with the membership", `
			SELECT s.id::text FROM sessions s WHERE s.status='active' AND s.scope='family' AND NOT EXISTS (
				SELECT 1 FROM family_slots f JOIN memberships m ON m.family_id=f.family_id AND m.account_id=f.account_id
				WHERE f.account_id=s.account_id AND f.family_id=s.family_id AND f.state='active' AND m.status='active' AND m.role=s.role)`},
		{"active recovery sessions without the account's recovery slot", `
			SELECT s.id::text FROM sessions s WHERE s.status='active' AND s.scope='family_recovery' AND NOT EXISTS (
				SELECT 1 FROM family_slots f WHERE f.account_id=s.account_id AND f.family_id=s.family_id AND f.state='family_recovery')`},
		{"active account sessions naming a family", `
			SELECT s.id::text FROM sessions s WHERE s.status='active' AND s.scope='account' AND (s.family_id IS NOT NULL OR s.role IS NOT NULL)`},
		{"active sessions of accounts that are not active or of revoked devices", `
			SELECT s.id::text FROM sessions s JOIN accounts a ON a.id=s.account_id JOIN devices d ON d.id=s.device_id
			WHERE s.status='active' AND (a.status<>'active' OR d.status<>'active')`},
	}
	for _, check := range checks {
		if bad := w.rows(check.query); len(bad) > 0 {
			t.Errorf("%s: %v", check.what, bad)
		}
	}
}
