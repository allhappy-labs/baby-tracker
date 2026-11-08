import SwiftUI
import BabyUI

/// Reachable credit and limitations for the bundled derivative. Its notice applies
/// to reference data, independently of the app's original code license.
struct GrowthReferenceInfoView: View {
    private let provenance = GrowthReferenceProvenance.load()

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                Text(GrowthCopy.text("Swiss PEZZ 2025 reference")).textRole(.sheetTitle)
                Text(GrowthCopy.text("Weight and length/height comparisons cover birth through the fifth calendar birthday. Head circumference keeps its recorded values and history without a reference percentile."))
                Text(GrowthCopy.text("These comparisons describe a population reference; they do not diagnose growth concerns."))
                Text(GrowthCopy.text("Ranks are rounded estimates. Values below P3 or above P97 show <3rd or >97th rather than an extrapolated rank."))
                Text(GrowthCopy.text("The app extracts only supplementary Tables S1, S2, S5 and S6, preserving published precision. Printed ages are mapped to calendar month anniversaries. Linear interpolation of L/M/S between nodes, including model joins, is an app approximation of the published model; no extrapolation is used."))
                Text(GrowthCopy.text("A later due date supplies corrected age until the third chronological birthday. This is the app’s application of AAP guidance; the Swiss study did not validate preterm growth."))
                Link(GrowthCopy.text("AAP corrected-age guidance"), destination: URL(string: "https://www.aap.org/en/patient-care/newborn-infant-and-early-childhood-nutrition/newborn-and-infant-nutrition-assessment-tools/term-infant-growth-tools/")!)
                Text(GrowthCopy.text("For standing measurements before the second reference-age birthday, only the comparison value receives +1 cm. Lying measurements from that birthday have no reference comparison. Recorded values stay unchanged."))
                Text(GrowthCopy.text("Source and license")).textRole(.cardTitle)
                // Author names and the published article title remain in their original form.
                Text("Eiholzer U, Stephan A, Dubinski I, Fritz C, Noordam C. Updated Swiss Growth References 2025: No Height Differences, but BMI Variations Associated with Migration. Journal of Clinical Medicine 14 (2025), 5912.")
                Link("DOI: 10.3390/jcm14165912", destination: URL(string: "https://doi.org/10.3390/jcm14165912")!)
                Link(GrowthCopy.text("Official supplementary tables"), destination: URL(string: "https://www.mdpi.com/article/10.3390/jcm14165912/s1")!)
                Text(GrowthCopy.text("© 2025 by the authors; licensed to MDPI. Reference data and this extraction use CC BY 4.0. The app’s original code has separate licensing. No endorsement by the authors, PEZZ or MDPI is implied."))
                Link(GrowthCopy.text("Creative Commons Attribution 4.0 (CC BY 4.0)"), destination: URL(string: "https://creativecommons.org/licenses/by/4.0/")!)
                if let provenance {
                    Text(GrowthCopy.text("Original ZIP SHA-256")).textRole(.cardTitle)
                    Text(provenance.source.sha256).font(.caption.monospaced())
                    Text(GrowthCopy.text("Extracted CSV SHA-256")).textRole(.cardTitle)
                    Text(provenance.csv.sha256).font(.caption.monospaced())
                } else {
                    Text(GrowthCopy.text("Reference data is unavailable."))
                }
            }
            .fixedSize(horizontal: false, vertical: true)
            .textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(20)
        }
        .background(Palette.ground.ignoresSafeArea())
        .foregroundStyle(Palette.primaryText)
        .tint(Palette.feed)
        .navigationTitle(GrowthCopy.text("Growth reference information"))
        #if os(iOS)
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .accessibilityIdentifier("growth.reference.information")
    }
}

private struct GrowthReferenceProvenance: Decodable {
    struct Artifact: Decodable { let sha256: String }
    let source: Artifact
    let csv: Artifact

    static func load() -> Self? {
        guard let url = Bundle.module.url(forResource: "SwissPEZZ2025-manifest", withExtension: "json"),
              let data = try? Data(contentsOf: url) else { return nil }
        return try? JSONDecoder().decode(Self.self, from: data)
    }
}
