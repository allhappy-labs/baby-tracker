import Foundation

public enum SupplementName {
    public enum ValidationError: Error, Sendable { case invalidName }

    public static func normalized(_ raw: String) throws -> String {
        let nfc = raw.precomposedStringWithCanonicalMapping
        let scalars = nfc.unicodeScalars.drop(while: ProtocolCanonicalizer.isUnicode15WhiteSpace)
        let trimmed = scalars.reversed().drop(while: ProtocolCanonicalizer.isUnicode15WhiteSpace).reversed()
        let name = String(String.UnicodeScalarView(trimmed))
        guard !name.isEmpty, name.unicodeScalars.count <= 64, name.utf8.count <= 256,
              !name.unicodeScalars.contains(where: { $0.properties.generalCategory == .control }) else {
            throw ValidationError.invalidName
        }
        return name
    }
}
