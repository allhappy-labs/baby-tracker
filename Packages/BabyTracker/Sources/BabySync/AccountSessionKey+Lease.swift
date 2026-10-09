import BabyStore
import Foundation

/// The one-way projection from a Keychain session identity to the neutral,
/// non-secret lease identity `BabyStore` owns.
///
/// It drops the recovery epoch deliberately: the account-scoped refresh lease
/// must serialise *across* an epoch rekey, so two sessions of the same account
/// on the same device contend for one lease rather than each taking their own.
///
/// The direction matters. `BabySync` knows about `BabyStore`; nothing in
/// `BabyStore` — schema, write boundary, or the lease probe executable — may
/// import a Keychain session type.
public extension AccountSessionKey {
    var leaseKey: AccountLeaseKey {
        AccountLeaseKey(
            canonicalOrigin: canonicalOrigin,
            serverInstanceID: serverInstanceID,
            accountID: accountID,
            deviceID: deviceID
        )
    }

    var continuityKey: ContinuityKey {
        ContinuityKey(
            canonicalOrigin: canonicalOrigin,
            serverInstanceID: serverInstanceID,
            accountID: accountID,
            deviceID: deviceID
        )
    }
}

public extension FamilySessionKey {
    var accountSessionKey: AccountSessionKey {
        AccountSessionKey(
            canonicalOrigin: canonicalOrigin,
            serverInstanceID: serverInstanceID,
            accountID: accountID,
            deviceID: deviceID,
            recoveryEpoch: recoveryEpoch
        )
    }

    var leaseKey: AccountLeaseKey { accountSessionKey.leaseKey }

    var realmID: RealmID {
        .shared(canonicalOrigin: canonicalOrigin, serverInstanceID: serverInstanceID, familyID: familyID)
    }
}
