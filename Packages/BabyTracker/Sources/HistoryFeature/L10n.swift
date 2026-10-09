import Foundation
import BabyCore

enum L10n {
    static func tr(_ key: String) -> String {
        NSLocalizedString(key, bundle: .module, comment: "")
    }

    static func fmt(_ key: String, _ arguments: CVarArg...) -> String {
        String(format: tr(key), locale: .current, arguments: arguments)
    }

    /// A key whose translation varies with a count ("1 day", "2 дні",
    /// "5 днів"). The plural form follows the language the string is in —
    /// the one this bundle resolved — not the device's region, which may
    /// belong to another language.
    static func plural(_ key: String, _ count: Int) -> String {
        String(format: tr(key), locale: language, count)
    }

    static let language = Locale(identifier: Bundle.module.preferredLocalizations.first ?? "en")
}
