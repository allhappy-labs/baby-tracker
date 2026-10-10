import BabyProtocol

/// Which APNs gateway this signed build's tokens belong to: the push-token
/// resolver's protocol. Live Activity and wake pushes share one key, one team
/// and one gateway per build, so the app reads its environment once, one way.
public typealias LiveActivityAPNsEnvironmentResolving = PushEnvironmentResolving

/// `main`'s Live Activity resolver is the push resolver
/// (`PushEnvironmentResolver.swift`), which reads the environment from the
/// provisioning profile the build was signed with, never from DEBUG or a
/// configuration name, and fails closed on anything it cannot read.
public typealias ProvisioningProfileEnvironmentResolver = PushEnvironmentResolver
