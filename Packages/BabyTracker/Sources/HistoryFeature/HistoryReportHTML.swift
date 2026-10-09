import Foundation

/// The report as one HTML page for iOS to print. Pure: the same report gives
/// the same text, on any platform. Every string from the user — names,
/// notes, foods, medicines — is escaped; nothing in the data can become
/// markup.
enum HistoryReportHTML {
    static func render(_ report: HistoryReport) -> String {
        var html = """
        <!DOCTYPE html>
        <html lang="\(escape(L10n.language.identifier))"><head><meta charset="utf-8"><style>\(css)</style></head><body>
        <header><h1>\(escape(report.babyName))</h1>
        <p class="range">\(escape(report.rangeText))</p>
        <p class="generated">\(escape(HistoryCopy.generated(report.generatedText)))</p></header>

        """
        for day in report.days {
            html += "<section><h2>\(escape(day.title))</h2>"
            if !day.totalsLine.isEmpty {
                html += "<p class=\"totals\">\(escape(day.totalsLine))</p>"
            }
            html += "<table><colgroup>"
                + columnWidths.map { "<col style=\"width: \(Int(($0 * 100).rounded()))%\">" }.joined()
                + "</colgroup><thead><tr>"
                + columns.map { "<th>\(escape($0))</th>" }.joined()
                + "</tr></thead><tbody>"
            for entry in day.entries {
                html += "<tr class=\"entry\"><td class=\"time\">\(escape(entry.time))</td>"
                    + "<td class=\"type\">\(escape(entry.type))</td>"
                    + "<td>\(escape(entry.details))</td>"
                    + "<td class=\"note\">\(entry.note.map(multiline) ?? "")</td></tr>"
            }
            html += "</tbody></table></section>\n"
        }
        return html + "</body></html>"
    }

    /// The table's columns and their shares of the page width. The printer
    /// draws the same headings at the same places at the top of every later
    /// page, so both read from here.
    static var columns: [String] {
        [HistoryCopy.columnTime, HistoryCopy.columnType, HistoryCopy.columnDetails, HistoryCopy.columnNote]
    }
    static let columnWidths: [CGFloat] = [0.17, 0.12, 0.36, 0.35]
    /// A cell's left and right padding, in points.
    static let cellPadding: CGFloat = 4

    static func escape(_ text: String) -> String {
        var escaped = ""
        escaped.reserveCapacity(text.count)
        for character in text {
            switch character {
            case "&": escaped += "&amp;"
            case "<": escaped += "&lt;"
            case ">": escaped += "&gt;"
            case "\"": escaped += "&quot;"
            case "'": escaped += "&#39;"
            default: escaped.append(character)
            }
        }
        return escaped
    }

    /// A note keeps its line breaks.
    private static func multiline(_ text: String) -> String {
        escape(text).replacingOccurrences(of: "\n", with: "<br>")
    }

    /// Print styles: a day's heading and totals stay with its first rows,
    /// and the columns keep fixed widths so the printer's running headings
    /// line up with them. The print WebKit ignores `thead` repetition and
    /// `page-break-inside`; the rules stay for any engine that honours them.
    private static let css = """
    body { font: 10.5pt -apple-system, "Helvetica Neue", Helvetica, sans-serif; color: #111; margin: 0; }
    h1 { font-size: 20pt; margin: 0 0 2pt; }
    .range { font-size: 12pt; margin: 0; }
    .generated { font-size: 8.5pt; color: #666; margin: 2pt 0 10pt; }
    section { margin-top: 14pt; }
    h2 { font-size: 12.5pt; margin: 0 0 2pt; page-break-after: avoid; }
    .totals { font-weight: 600; margin: 0 0 4pt; page-break-after: avoid; }
    table { width: 100%; border-collapse: collapse; table-layout: fixed; }
    thead { display: table-header-group; }
    tr { page-break-inside: avoid; }
    th { text-align: left; font-weight: 600; color: #444; border-bottom: 1px solid #999; padding: 2pt 4pt; }
    td { border-bottom: 1px solid #ddd; padding: 3pt 4pt; vertical-align: top; overflow-wrap: break-word; }
    .time { white-space: nowrap; }
    .note { color: #444; }
    """
}
