# History PDF export and daily totals — design

Date: 2026-10-08.

## Intent

Parents want to hand a pediatrician, a grandparent or a printer a record of a stretch of days. They also want each day in History to say at a glance how it went.

- **Export.** An export button on History asks for a date range. It builds a PDF of that range and opens the share sheet. The share sheet already offers Print, AirDrop, Files, Mail and Messages.
- **Daily summary.** Every day shows four totals:
  - feeds (count)
  - total sleep
  - total bottle volume
  - diapers (count)

  History's day header already shows feeds, sleep and diapers. Bottle volume is new. The PDF opens every day with the same four totals.

Decisions made with the user:
- **"Feed times" means the number of feeds.** It is not breastfeeding minutes or a list of clock times.
- **The PDF has a summary and a full log.** Each day gives its four totals, then every entry with its time and details.
- **The range is a preset or custom dates.** The presets are Last 7 days, Last 14 days and Last 30 days. Custom has a start date and an end date.
- **The PDF always includes every tracker.** History's tracker filter does not change what is exported.
- **The PDF is HTML printed to PDF by iOS (approach A).** It is not SwiftUI rendered to PDF or text drawn by hand.

Assumptions not raised with the user:
- The PDF uses the app's language and the user's ml/oz preference.
- It covers the currently selected baby.
- Running timers are left out. The PDF lists finished entries only.
- Paper is US Letter when the locale's measurement system is US, and A4 otherwise.

Success means five things:
- History's day header shows bottle volume whenever that day had a bottle.
- The header's totals and the PDF's totals for the same day are always equal, because one builder makes both.
- From History, the PDF for a chosen range reaches the share sheet in at most four taps: Export, a preset, Create PDF, then a share target.
- The PDF prints cleanly across several pages, and its text can be selected.
- The PDF is fully localised in en, de, es, fr, it and uk.

## 1. One day builder for the list and the export

`HistoryViewModel.load` builds the days today, using the paging window and the filter. The work that turns store data into days moves into an internal `HistoryDayBuilder`.

The builder's input:
- store
- baby ID
- range start and end
- filter
- volume unit
- time zone
- today and yesterday keys, used for titles

The builder's output is `[HistoryDay]` plus the raw facts the export needs (§3).

- **The History list** calls the builder with its window and the user's filter. It then prepends running timers to today, as it does now. Paging, empty states and focus stay in the view model.
- **The export** calls the builder with the chosen range and every tracker.

### Bottle total

`HistoryDayTotals` gains `bottleML: Double`. For every feeding session that starts on a day, the builder adds each member's `totalBottleVolumeML` to that day. This is the same day the feed count uses, so a feed that crosses midnight counts on the day it started.

Filter rules for the new total:
- It is 0 when the feed tracker is filtered out, the same as `feeds`.
- It is formatted in the view with `Volume.formatted(as: volumeUnit)`.
- It stays a number in the model, so the PDF and tests can use the raw value.

## 2. Day header

The header shows its totals in this order, each with its tracker's icon and colour:
1. feeds
2. sleep
3. **bottle volume**, e.g. "540 ml", with the bottle icon in the feed colour
4. diapers

As today, a total only appears when it is above zero. The existing layout for accessibility text sizes, a stacked `VStack`, already handles four items. The VoiceOver label adds "540 ml bottle".

## 3. Report model

`HistoryReport` is a plain value type that the HTML renderer reads.

The report has:
- `babyName`
- `range` (first day key and last day key)
- `generatedAt`
- `days`, oldest first so the PDF reads like a diary

Each day has:
- `title`: the full date, such as "Wednesday, 7 October 2026". It is never "Today", so the printout still makes sense later.
- `totals`: a `HistoryDayTotals`
- `entries`, oldest first

Each entry has:
- `time`: "10:23" or "10:23–11:05"
- `type`: the tracker's existing localised name from `BabyUI` ("Feed", "Sleep", "Diaper", "Meds", "Growth")
- `details`: the same words History shows, with the row's chips joined by " · ", e.g. "L 12m · R 8m · 60 ml"
- `note`

Days with no entries are left out. If every day is empty, the export reports "Nothing logged in these dates" and no PDF is made.

`HistoryReportBuilder.make(store:babyID:range:volumeUnit:)` calls `HistoryDayBuilder` with every tracker, then maps its days into the report.

## 4. HTML and PDF

- **`HistoryReportHTML.render(_ report:) -> String`** is a pure function, and works on macOS too so it can be tested there.
  - It writes one `<section>` per day: a heading, a totals line, and a four-column table (Time | Type | Details | Note).
  - CSS keeps each table header row on the next page when a table splits (`thead { display: table-header-group }`).
  - Each day heading stays with its first rows (`h2 { page-break-after: avoid }`).
  - Every string that comes from the user is HTML-escaped: names, notes, medicine and food names.
- **`HistoryPDFPrinter.pdf(fromHTML:paper:) -> Data`** is iOS only (`#if canImport(UIKit)`). It runs a `UIMarkupTextPrintFormatter` through a `UIPrintPageRenderer` sized to A4 or Letter with 36 pt margins, then draws the pages into a `UIGraphicsPDFRenderer`. It runs on the main actor, because the print formatter needs it.
- **The file is named** `"<baby name> – <range>.pdf"`. The range is formatted with `Date.IntervalFormatStyle` (date only). Characters that cannot appear in a file name are replaced. The file goes into a new private temp folder, `baby-tracker-history-exports`. That folder follows `ExportTemporaryFiles`: it is excluded from backup, uses complete file protection, and is cleared each time a new export starts.

## 5. Export sheet

The History header gets an export button to the left of the filter button. It uses the `square.and.arrow.up` symbol, matches the filter button's 44 pt circle, has the label "Export PDF" and the accessibility identifier `history.export`.

The button opens `HistoryExportSheet` (a medium/large detent sheet), which `HistoryExportViewModel` drives.

- **Range presets** are a segmented picker: 7 days, 14 days, 30 days, Custom. The default is 7 days, and each preset ends today.
- **Custom** shows two date pickers, From and To.
  - Both are clamped to the baby's birth date and today.
  - Moving From past To moves To with it, and the reverse.
- **Create PDF** builds the report. While it works, the button shows a spinner and is disabled. Then one of three things happens:
  - **Entries found:** the PDF is written and the share sheet opens. It uses the existing `ActivityShareSheet` pattern from `FamilyExportView`, moved into `BabyUI` so both features can use it.
  - **No entries:** the sheet shows "Nothing logged in these dates" and makes no file.
  - **Failure:** a store or file error shows "Couldn't create the PDF. Try again."

The report covers whole local days from the start of From to the end of To. Presets mean the N days ending today, today included.

## 6. Localisation

All new strings go into `HistoryFeature`'s `Localizable.xcstrings`, with en, de, es, fr, it and uk translations. Plural forms ("%lld feeds", "%lld diapers") use the catalog's plural variations, as the existing History strings do. Dates and volumes use the system formatters with the app's locale.

## 7. Tests

**`HistoryFeatureTests`** (unit):
- **Builder:** the bottle total adds bottle parts across a combo session. A session that crosses midnight counts its bottle on the day it started. With feeds filtered out, the bottle total is 0. Tombstoned entries are left out.
- **List:** the History list's totals are unchanged for existing fixtures, so the refactor causes no regression.
- **Report:** days run oldest first and empty days are left out. A filtered History still exports every tracker. The range edges include the first and last local day. An empty range reports nothing to export.
- **HTML:** notes and names are escaped (`<script>`, `&`, quotes). Each day's totals line appears in the output.
- **Export view model:** each preset turns into the right dates. Custom dates are clamped to the birth date and today, and From and To stay in order.
- **PDF smoke test (iOS only):** a two-day report renders to non-empty PDF data with at least one page (checked with `CGPDFDocument`).

**UI test:** one test in `HistoryRedesignTests` (`testExportSheetOpens`) taps `history.export`, checks that the sheet and its Create PDF button appear, and closes the sheet. This keeps the UI suite lean.

## Out of scope

- CSV or other formats, and charts in the PDF
- Exporting several babies in one file
- Respecting History's filter in the export
- Saving export presets
