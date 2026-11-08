# Insights redesign — design

Date: 2026-10-04. Third redesign sub-project, after Home & tracking ([spec](2026-10-01-home-tracking-redesign-design.md)) and History ([spec](2026-10-03-history-redesign-design.md)). Mockups: `mockups/2026-10-04-insights-redesign/` (README lists the chosen options). The spec wins if a mockup disagrees.

## Intent

The current screen has four problems:
- It leads with "today" totals that Home already shows.
- It compares a partial day with full-day averages. At 9:00 it reports "5 fewer feeds than usual".
- Its bar charts have no scale and no way to read a value.
- It ignores most of what is recorded.

Its range picker only changes how many bars are drawn, while the average stays on seven days.

The user opens Insights for three reasons:
- **Is the baby on track?** Reassurance from the baby's recent days.
- **Trends over weeks.** Is night sleep getting longer, are feeds spacing out?
- **Details for the doctor.** Numbers to read out at a check-up from the screen, without any export.

Daily rhythm planning is not a goal. The 24-hour strip is removed.

Norms come only from the bundled **Swiss PEZZ 2025** growth reference (weight and length/height percentiles). Feeds, sleep and diapers are compared only with the baby's own history. The app never shows generic age ranges for them, and never colours a value as good or bad.

Insights and Growth merge. The separate Growth details screen goes away. Growth charts live on Insights, and editing check-ins happens in History.

Success means four things:
- The averages and comparisons are true at any hour of the day.
- Every number the user listed for a doctor is visible on one scroll without taps.
- The screen shows feeding and sleep detail, not only totals.
- Everything fits in Ukrainian at the largest accessibility text size.

## 1. Screen

One scroll (mockup layout **A**) of four sections, in this order: Feeding, Sleep, Diapers, Growth.

- **Header.** Pinned while scrolling. It shows:
  - the baby's name, with the same baby switcher as Home and History;
  - a segmented **Week / Month / All** picker.

  The "INSIGHTS" eyebrow is removed.
- **Sections** are cards marked by their tracker icon and colour only, with no title word. The unit after the headline number says what it is ("feeds / day", "diapers / day").
- **Headline.** The per-day average for the selected period, with a neutral comparison to its right (section 2).
- **Charts.**
  - Bars with two faint y-gridlines labelled on the right, and a dashed line at the period average.
  - The x axis is labelled at its two ends only.
  - Today is a hatched "so far" bar at the right end of Week and Month. It is never counted (section 2).
  - Tapping or dragging across a chart shows a callout with the bar's date (or week) and exact value. Sleep callouts add the night and naps split.
- **Feeding.**
  - Headline: feeds / day.
  - Chart: feeds per day.
  - Rows: Between feeds, Longest gap, Breast · bottle (with a thin split bar), Left · right, Bottle / day.
- **Sleep.**
  - Headline: asleep time / day.
  - Chart: stacked bars, night solid and naps lighter, with a small night/naps key.
  - Rows: Longest stretch, Night · naps, Naps / day, Awake between sleeps.
- **Diapers.**
  - Headline: diapers / day.
  - Chart: a short bar chart.
  - No detail rows. Wet/dirty detail is out of scope.
- **Growth.** Layout **G1**. Growth ignores the range picker, and its chart always runs from birth.
  - **Header line:** the date of the latest check-in and the baby's age then.
  - **Chips:** three side by side, for Weight, Length and Head.
    - Each shows its latest value and Swiss percentile.
    - Head has no reference, so it shows "—" in place of a percentile.
    - A metric never measured shows "—" as its value.
    - The selected chip is outlined. Weight is selected by default.
  - **Change line** below the chips: the change since the previous check-in of that metric, plus the average change per day between them (weight in g/day, length in cm). Hidden with fewer than two readings.
  - **Chart:**
    - The baby's readings as a line with points, over the Swiss 3rd–97th percentile band and a dashed median, following the existing band rules (no band across an unavailable comparison, no one-point band). Head shows the raw line only.
    - Tapping a point shows its date, value and percentile.
    - With fewer than two readings of the selected metric, the chart is replaced by "The chart appears after a second check-in."
  - **Swiss PEZZ 2025 reference** link: pushes the existing `GrowthReferenceInfoView`.
  - **All check-ins** row with a count: switches to History with its filter set to Growth only (section 4).
  - **Without a reference sex:** the chips show raw values, and **Girls / Boys** buttons appear in place of percentiles. This is today's `referenceChoice`, moved here.
  - **With no measurements at all:** one line, "No check-ins yet. Add one from Home → Measure.", with no chips or chart.
- **Rows appear only when they apply.**
  - No bottle feeds in the period: no Bottle / day row and no bottle segment.
  - No breast feeds: no Left · right row.
  - Fewer than two sleeps: no Awake between sleeps row.
  - A section with no logged days in the period shows its icon and "Nothing logged in this period."
- **Removed:**
  - the four "today" figure cards, the 24-hour strip (`RhythmStrip`, `RhythmAxis`, `DayRhythm`) and the 7/14/30 picker;
  - the weight-only `GrowthCardView`, `GrowthView` (the Growth details screen) and `GrowthMetricCard` with its history list and Edit/Delete buttons.

## 2. Periods, averages and comparisons

- **Days.** A day runs from local midnight to local midnight in the device's current time zone, as now (`DayBucket`).
- **Periods.** Today is never part of a period.
  - **Week:** the 7 days ending yesterday.
  - **Month:** the 30 days ending yesterday.
  - **All:** from the first day with any feed, sleep or diaper logged, to yesterday. When yesterday is earlier than that first day (tracking began today), every section shows "Nothing logged in this period."
- **Counted days are per section.** A day counts toward a section's averages only if that section's kind of entry was logged that day: feeds for Feeding, sleeps for Sleep, diapers for Diapers. A day with feeds but no sleep logged is a day of missing sleep data, not a day of zero sleep.
- **Averages** are the per-day mean over counted days.
- **Comparison** applies to Week and Month only.
  - It compares with the immediately preceding period of equal length, averaged the same way.
  - It shows ▲ or ▼ and the difference, in secondary grey. "vs prev. week" / "vs prev. month" sits beneath it.
  - It shows "≈" when the difference rounds to zero at the displayed precision.
  - When the preceding period has no counted days, the comparison is replaced by "%lld days", the number of days the average covers.
  - All shows "since %@", the first counted date, with no comparison.
- **Precision.**
  - Counts per day: one decimal.
  - Durations: whole minutes, shown as `14h 20m` via `ElapsedFormatter`.
  - Bottle volume: whole ml, or one decimal for fl oz.
  - Percent shares: whole numbers that sum to 100.
- **Charts.**
  - Week and Month draw one bar per day of the period, plus today's hatched bar.
  - A day with nothing logged for that section is an empty slot, not a missing one.
  - All draws one bar per 7-day block, counting back from yesterday, so the last bar is always a full week. The first block may be shorter. Each bar is that block's per-day average over its counted days.
- **Running sessions** are excluded from every calculation. Today is excluded anyway. A session still running from yesterday is excluded until it ends.

## 3. Metric definitions

Sessions come from the existing projections (`store.feedingSessions`, `store.sleepSessions`), so a joined session counts once, on the day its first part started. That matches Home and History.

**Feeding**

| Metric | Definition |
|---|---|
| Feeds / day | Feeding sessions started that day. |
| Between feeds | Median start-to-start interval between consecutive sessions, over intervals whose later session started in the period. |
| Longest gap | Longest such start-to-start interval. |
| Breast · bottle | Sessions classified as breast only, bottle only, or both; shares of those three. Zero shares are omitted, and the row label lists the kinds shown: "Breast · bottle  72% · 28%", or "Breast · both · bottle  64% · 8% · 28%" when some sessions had both. Sessions with only solids are left out of the split but still count in Feeds / day. |
| Left · right | Share of total breast seconds on each side. |
| Bottle / day | Mean of each counted day's measured bottle volume (`measuredBottleSteps`), in the user's volume unit. Days with feeds but no bottle count as 0. |

**Sleep**

| Metric | Definition |
|---|---|
| Asleep / day | Sum of `asleepSeconds` of sleep sessions started that day. |
| Night · naps | Each session's kind is the effective `SleepEntry.kind` of its first part (classifier, overridable). Mean per day of each kind's asleep time. |
| Longest stretch | Longest single completed `SleepEntry` (one continuous sleep). A joined session with a wake counts as separate stretches. For All this is the record. |
| Naps / day | Nap sessions per counted day. |
| Awake between sleeps | Median gap from one session's end to the next session's start. A gap counts only if it starts in daytime (`SleepClassifier` hours 06:00–19:00) and is at most 12 hours long, because longer gaps almost always mean a sleep wasn't logged. |

**Diapers.** Diaper entries per day.

**Growth.** Unchanged calculation:
- `GrowthViewModel` summaries and `GrowthReferenceCalculator`, including corrected-age labelling and the "<3rd" / ">97th" tails.
- The chip shows the percentile compactly ("P48"). The tapped-point callout carries the full wording, including "corrected age".

## 4. Integration

- **InsightsSummary.** One pure calculator in InsightsFeature computes every value in sections 2–3.
  - Inputs: the sessions, entries and bottle steps for the needed window, plus `now`, the time zone and the range.
  - Output: an `InsightsSummary` value.
  - `InsightsViewModel` fetches and calls it. The view only formats and draws.
  - The fetch window covers the previous period too, or from the first logged day for All.
- **Reload triggers.** The view reloads on `store.revision`, on a volume unit change, on a range change, and when the scene becomes active, as now.
- **History focus.** `HistoryFocus` gains an optional tracker filter.
  - `RootView` handles Insights' **All check-ins** by setting `historyFocus` with `filter: [.growth]` and selecting the History tab.
  - HistoryView applies it the way it applies `entryID` today.
  - The pinned "Showing: Growth ✕" chip clears it.
- **Growth reference.** `RootView`/Settings handling of the profile is unchanged. The Girls / Boys buttons on Insights call the same `setGrowthProfile` path as today's Growth details screen.
- **Ask.** `AskFactBuilder` for `.insights(range)` builds its facts from `InsightsSummary`:
  - per-day averages and comparisons for feeds, bottle volume, sleep and diapers;
  - the feeding and sleep detail rows.

  The `today` facts are dropped, because Ask's handoff query already covers today.
  - Fact keys become `insight.feeding.*`, `insight.sleep.*` and `insight.diapers.*`.
  - Sources become `.insightRange(key:days:)` with keys `feeding`, `sleep`, `diapers` and `growth`. `days` encodes the range: 7 = Week, 30 = Month, 0 = All.
- **Old Ask links.** *Superseded by the 2026-10-06 amendment: old Insights links are no longer shown.* Saved Ask conversations keep their old sources and still open the right place:
  - key `feeds` or `volume` → Feeding;
  - `sleep` → Sleep;
  - `diapers` → Diapers;
  - `days` 7 → Week; 14 or 30 → Month; anything else → Week.
- **InsightFocus** *(superseded by the 2026-10-06 amendment: removed)* keeps its shape. `InsightRange` becomes `week`, `month` and `all`, with an initializer from the Ask `days` value as above.
- **Module boundaries.**
  - GrowthFeature keeps the reference data, the calculator, `GrowthViewModel` and `GrowthReferenceInfoView`.
  - It loses `GrowthView`, `GrowthCardView` and `GrowthMetricCard`.
  - The Growth section view, including its chart (moved from `GrowthMetricCard.chart` and `GrowthMetricPresentation`), lives in GrowthFeature as a public `GrowthSectionView`. Insights embeds it.
  - `GrowthRecordActions` keeps only the reference choice.
- **Strings.** Strings used only by removed views are deleted from the catalogues. New strings are translated into de, es, fr, it and uk.

## 5. Accessibility

- Each section's headline reads as one sentence, e.g. "Feeds, 8.4 per day this week, 0.6 more than the previous week".
- Charts use Swift Charts' per-mark accessibility, with each bar labelled by date (or week) and value. Today's bar says "so far".
- Each Growth chip reads its metric, value, percentile wording and date. Head says "No reference percentile".
- The tap-to-inspect callout is a pointer convenience. VoiceOver users reach the same values through the marks.
- At accessibility text sizes:
  - growth chips stack vertically;
  - detail rows wrap the value under the label;
  - the comparison moves under the headline.
- Every control stays at least 44 pt.

## 6. Testing

- **Calculator unit tests** for every definition in sections 2–3:
  - day boundaries and a session crossing midnight;
  - joined feed and sleep sessions counted once;
  - running sessions excluded;
  - per-section counted days;
  - empty, new-baby and missing-preceding-period cases;
  - ≈ rounding and percent shares summing to 100;
  - All's 7-day blocks with a short first block;
  - the 12-hour and daytime rules for awake gaps;
  - longest stretch within a joined sleep;
  - solids-only sessions in the split.
- **View model tests:** range switching, volume unit change and reload on revision.
- **Ask tests:** the new facts and keys, and old saved sources mapping to sections and ranges.
- **History focus test:** a growth filter focus applies the filter and shows the clear chip.
- **UI tests** on the shared simulator (`platform=iOS Simulator,id=<simulator-udid>`):
  - `GrowthFlowUITests` is rewritten for the Growth section: chips, chip switch, reference info, and All check-ins landing in History filtered to Growth;
  - Insights range switching.
- **Accessibility audits** on Insights. **Ukrainian at the largest accessibility size** layout smoke.
- **Localization gate:** no new problems against the main baseline.
- **Screenshots** of Week, All and a new baby compared with the mockups.

## Amendments from the code survey (2026-10-06)

Main moved after this spec was approved. Each decision below keeps the approved intent.

- **Ask no longer reads or links Insights.** Ask now answers through its own read-only tools (commits `894ff29f`–`fc67b2f1`), and the only sources it creates are History entries (`AskCareLog.source(of:)`).
  - Section 4's Ask fact work and its saved-link mapping are dropped (user ruling 2026-10-06).
  - `AskSource.insight` / `.insightRange` stay in the enum only so chats saved by the old Ask still decode. Ask no longer shows them as source chips, and `RootView` ignores them.
  - Ask was the only caller of `InsightFocus`. `InsightFocus`, `InsightRange`'s Ask day mapping and Insights' scroll-to-section focus are removed.
  - AskFeature no longer depends on InsightsFeature.
- **Today's bar is a faint fill, not hatched.** Swift Charts has no pattern fill. Today's bar uses the tracker colour at low opacity, and the axis end label says "Today".
- **Growth change line.**
  - Weight shows its change and its average per day in grams.
  - Length and head show their change only, because a per-day length change is too small to read.
- **Growth empty copy reuses existing translated strings:**
  - "No measurements yet" when nothing is recorded;
  - "A trend needs two measurements." when the selected metric has one reading.
- **Running feeds.** A running feed part is a timer, not a stored entry. Every stored feed session therefore counts. Only open sleeps (`endedAt == nil`) are excluded.

## Out of scope

- Diaper wet/dirty detail, solids and meds on Insights.
- Any norm or reference other than Swiss PEZZ growth.
- Export, PDF or share of a summary.
- Ask's own layout, and Settings/Caregivers/Export (later sub-projects).
- Any change to the data model, the sync protocol or the server.
