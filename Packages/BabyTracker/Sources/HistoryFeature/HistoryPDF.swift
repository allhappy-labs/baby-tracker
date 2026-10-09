import Foundation
import CoreGraphics
#if canImport(UIKit)
import UIKit
#endif

/// The sheet the PDF is laid out for: Letter where the region measures in
/// US units, A4 everywhere else.
enum PaperSize: Equatable {
    case a4, letter

    var size: CGSize {
        switch self {
        case .a4: CGSize(width: 595.28, height: 841.89)
        case .letter: CGSize(width: 612, height: 792)
        }
    }

    static func `for`(_ locale: Locale) -> PaperSize {
        locale.measurementSystem == .us ? .letter : .a4
    }
}

#if canImport(UIKit)
/// HTML to PDF through iOS's own print system: it lays out the pages, breaks
/// tables between them, and keeps the text selectable.
@MainActor
enum HistoryPDFPrinter {
    static let margin: CGFloat = 36

    static func pdf(report: HistoryReport, paper: PaperSize) -> Data {
        let page = CGRect(origin: .zero, size: paper.size)
        let renderer = ReportPageRenderer(title: report.babyName + " · " + report.rangeText)
        renderer.addPrintFormatter(UIMarkupTextPrintFormatter(markupText: HistoryReportHTML.render(report)), startingAtPageAt: 0)
        // UIPrintPageRenderer reads its page from these two read-only
        // properties; key-value coding is the documented way to set them
        // when rendering without a printer.
        renderer.setValue(NSValue(cgRect: page), forKey: "paperRect")
        renderer.setValue(NSValue(cgRect: page.insetBy(dx: margin, dy: margin)), forKey: "printableRect")

        return UIGraphicsPDFRenderer(bounds: page).pdfData { context in
            let pages = renderer.numberOfPages
            renderer.prepare(forDrawingPages: NSRange(location: 0, length: pages))
            for index in 0..<pages {
                context.beginPage()
                renderer.drawPage(at: index, in: context.pdfContextBounds)
            }
        }
    }
}

/// Every page after the first opens with whose record it is, its page
/// number, and the table's column headings. The print WebKit does not
/// repeat a table's `thead` on the next page, so a page that starts
/// mid-day would otherwise be rows with no headings.
private final class ReportPageRenderer: UIPrintPageRenderer {
    private let title: String

    init(title: String) {
        self.title = title
        super.init()
        headerHeight = 30
    }

    override func drawHeaderForPage(at pageIndex: Int, in pageHeaderRect: CGRect) {
        guard pageIndex > 0 else { return }
        // The rect UIKit hands over spans the whole paper; the headings
        // belong inside the margins, over the table's columns.
        let headerRect = CGRect(x: printableRect.minX, y: printableRect.minY,
                                width: printableRect.width, height: headerHeight)
        let caption: [NSAttributedString.Key: Any] = [
            .font: UIFont.systemFont(ofSize: 8), .foregroundColor: UIColor.darkGray,
        ]
        (title as NSString).draw(at: CGPoint(x: headerRect.minX, y: headerRect.minY), withAttributes: caption)
        let pageText = HistoryCopy.page(pageIndex + 1, of: numberOfPages) as NSString
        let pageWidth = pageText.size(withAttributes: caption).width
        pageText.draw(at: CGPoint(x: headerRect.maxX - pageWidth, y: headerRect.minY), withAttributes: caption)

        // The same columns, widths and padding as the HTML table's own
        // headings (`table-layout: fixed`), so the two line up.
        let heading: [NSAttributedString.Key: Any] = [
            .font: UIFont.systemFont(ofSize: 10.5, weight: .semibold), .foregroundColor: UIColor(white: 0.27, alpha: 1),
        ]
        var x = headerRect.minX
        for (name, share) in zip(HistoryReportHTML.columns, HistoryReportHTML.columnWidths) {
            (name as NSString).draw(at: CGPoint(x: x + HistoryReportHTML.cellPadding, y: headerRect.minY + 13),
                                    withAttributes: heading)
            x += headerRect.width * share
        }
        let rule = UIBezierPath()
        rule.move(to: CGPoint(x: headerRect.minX, y: headerRect.maxY - 1))
        rule.addLine(to: CGPoint(x: headerRect.maxX, y: headerRect.maxY - 1))
        rule.lineWidth = 1
        UIColor(white: 0.6, alpha: 1).setStroke()
        rule.stroke()
    }
}
#endif

/// The app-private folder an export is written to before it is shared. It
/// holds one file at a time: each new export empties it first.
final class HistoryExportFiles {
    let directory: URL
    private let manager = FileManager.default

    init(directory: URL? = nil) throws {
        let base = directory ?? FileManager.default.temporaryDirectory
            .appendingPathComponent("baby-tracker-history-exports", isDirectory: true)
        self.directory = base
        try manager.createDirectory(at: base, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var mutable = base
        try mutable.setResourceValues(values)
    }

    /// "Mira – 1–7 Oct 2026.pdf". Characters a file name cannot hold become
    /// "-"; a blank name falls back to "History".
    static func fileName(babyName: String, rangeText: String) -> String {
        let trimmed = babyName.trimmingCharacters(in: .whitespacesAndNewlines)
        let name = trimmed.isEmpty ? HistoryCopy.title : trimmed
        let unsafe = CharacterSet(charactersIn: "/\\:?%*|\"<>").union(.newlines).union(.controlCharacters)
        let safe = String(String.UnicodeScalarView(name.unicodeScalars.map { unsafe.contains($0) ? "-" : $0 }))
        return "\(safe) – \(rangeText).pdf"
    }

    func write(_ data: Data, named name: String) throws -> URL {
        for file in try manager.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil) {
            try manager.removeItem(at: file)
        }
        let file = directory.appendingPathComponent(name)
        try data.write(to: file, options: [.atomic])
        #if os(iOS)
        try manager.setAttributes([.protectionKey: FileProtectionType.complete], ofItemAtPath: file.path)
        #endif
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var mutable = file
        try mutable.setResourceValues(values)
        return file
    }
}
