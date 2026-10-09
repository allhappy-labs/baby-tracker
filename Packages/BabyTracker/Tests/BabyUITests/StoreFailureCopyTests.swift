import Testing
import BabyUI

/// The failure screen is the only thing a user sees when the store will not
/// open, so its copy carries the whole recovery story. These pin the two
/// properties that matter: it never blames the user, and it never offers to
/// throw their entries away.
@Test func storeFailureCopyOffersNoDestructiveAction() {
    let everything = [
        StoreFailureCopy.title,
        StoreFailureCopy.reassurance,
        StoreFailureCopy.retry,
    ].joined(separator: " ").lowercased()

    for forbidden in ["delete", "erase", "reset", "clear", "start over", "reinstall"] {
        #expect(!everything.contains(forbidden), "copy must not offer \(forbidden)")
    }
}

@Test func storeFailureCopyDoesNotBlameTheUser() {
    let everything = [
        StoreFailureCopy.title,
        StoreFailureCopy.reassurance,
        StoreFailureCopy.retry,
    ].joined(separator: " ").lowercased()

    for blaming in ["you ", "your fault", "invalid", "corrupt", "failed"] {
        #expect(!everything.contains(blaming), "copy must not say \(blaming)")
    }
}

@Test func storeFailureCopySaysTheEntriesAreStillThere() {
    #expect(StoreFailureCopy.reassurance.lowercased().contains("still"))
    #expect(!StoreFailureCopy.title.isEmpty)
}
