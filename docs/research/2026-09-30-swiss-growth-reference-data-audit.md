# Swiss PEZZ 2025 weight and height reference: source audit

Date: 30 September 2026. Scope: a no-fee, on-device, sex-specific weight-for-age and length/height-for-age reference for Baby Tracker, birth through age five. **Decision: official supplement verified; a rounded LMS derivative is technically feasible, subject to explicit age/interpolation and clinical handling decisions.** The user supplied the official MDPI ZIP. Its bytes, table structure, coverage, units, parameter orientation, and selected centiles were audited. No Swiss data was added to the app.

## Source and rights

- The primary publication is Eiholzer U, Stephan A, Dubinski I, Fritz C, Noordam C, *Updated Swiss Growth References 2025: No Height Differences, but BMI Variations Associated with Migration*, *Journal of Clinical Medicine* **14** (2025), 5912, DOI [10.3390/jcm14165912](https://doi.org/10.3390/jcm14165912). The paper reports reference curves for children living in Switzerland from a combined cohort of 43,290, including all three main language regions. It explicitly names supplementary Table S1 (girls' height), S2 (boys' height), S5 (girls' weight), and S6 (boys' weight). Height units are cm and weight units kg; its described age ranges are birth–18 years for girls and birth–20 years for boys. The study's birth sample excluded gestational ages below 39 or above 41 weeks. [Article: methods and supplementary materials](https://www.mdpi.com/2077-0383/14/16/5912).
- The article declares **© 2025 by the authors, licensed to MDPI, CC BY 4.0**. [MDPI's open-access policy](https://www.mdpi.com/openaccess) expressly includes an article's data, graphics, and supplementary material in its reuse statement, with source and publisher credit; it warns that third-party material can be excluded. The paper identifies no third-party rights exception for S1/S2/S5/S6 in the visible article. Both PDFs in the supplied ZIP were inspected: neither contains a separate copyright/license notice or a third-party restriction. The ZIP contains no README or license file. Reuse therefore relies on the article and publisher supplementary-material policy, not an additional license printed inside the ZIP. The underlying individual-level study data are separately described as available from the corresponding author on request; that statement does not negate the published aggregate tables. [Article: copyright and data availability](https://www.mdpi.com/2077-0383/14/16/5912).
- [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/legalcode.en) grants royalty-free copying, sharing, and adaptation, including applicable database extraction rights, subject to attribution, source/license link, and change notice. It has no ShareAlike term, so the app's original code can have a different license while the bundled published tables retain CC BY attribution. Do not imply PEZZ, the authors, MDPI, or pädiatrie schweiz endorses the app. Check that app packaging does not impose downstream restrictions on the CC BY data. This is a source-rights reading, not a review of a final binary or legal opinion.
- The [2026 pädiatrie schweiz chart PDF](https://files.paediatrieschweiz.ch/production/uploads/2026/07/PCH_Wachstumskurven_260701_DE.pdf) is **a separate work**. Its page 2 requires article attribution plus naming the society and prohibits altering its curves without consent. Do not bundle, trace, digitize, or re-create that PDF's curves using the article's license. It describes the society's combined charts as using PEZZ for length/weight, WHO for BMI under two, and other sources for head circumference. The proposed dataset is only the PEZZ article's weight and height supplement.

## Exact artifact: verified user-supplied supplement

The user supplied `jcm-14-05912-s001.zip` from the [publisher supplement endpoint](https://www.mdpi.com/article/10.3390/jcm14165912/s1). The source is retained locally under `.superpowers/sdd/2026-09-30-growth-measurements/source/`; it is not bundled into the app. SHA-256 was computed from the original bytes. The earlier network/browser acquisition block is superseded by this supplied artifact; no alternate download was attempted. A fresh article fetch during this audit still returned HTTP 429, so the main-article comparison below uses the previously captured Table 3 values in this document.

| Artifact | Size / pages | SHA-256 |
| --- | --- | --- |
| `jcm-14-05912-s001.zip` | 329,239 bytes; exactly two PDF members | `0def88b3a5e0096b54064b7d9459e683081364943cab341e179f947a46d61336` |
| `jcm-3796415 Supplemental material to Manuscript Cohort 2019+2025 Supp. Table S1-S8 Figure S1-S3. (Edited).pdf` | 397,735 bytes; 11 pages | `fb05a53a8ffbbac45201ad8407ff8c0810abd1fa15e38e4d964681f6043f141a` |
| `jcm-3796415 Supplemental material to Manuscript Cohort 2019+2025 Supp. Text S1-S4. (Edited).pdf` | 103,839 bytes; 2 pages | `a98fd3168a445ee44e1426d612cc99a63574a9c4a55fdb463b1033e652392466` |

Page references below are one-based PDF page indices; the PDF has no printed page numbering. Native macOS PDFKit extracted text from all 13 pages and rendered every page. Full visual inspection covered all pages containing S1/S2/S5/S6 (table PDF pages 1–3 and 6–8), including repeated headers, continuation boundaries, negative signs, and footnotes. Poppler and Python PDF libraries were unavailable locally. Text review of both complete PDFs found no conversion/interpolation instructions or additional rights notices. The text PDF consists of acknowledgments and participating practices/institutions, not model specifications.

| Table | Exact scope / units | Pages | Total numeric rows | Rows from 0 through 5 inclusive |
| --- | --- | --- | ---: | ---: |
| S1 | Girls' smoothed height, cm, 0–18 years | 1–2; birth–5 entirely on 1 | 57 | 31 |
| S2 | Boys' smoothed height, cm, 0–20 years | 2–3; birth–5 spans both | 61 | 31 |
| S5 | Girls' smoothed weight, kg, 0–18 years | 6–7; birth–5 entirely on 6 | 57 | 31 |
| S6 | Boys' smoothed weight, kg, 0–20 years | 7–8; birth–5 spans both | 61 | 31 |

Each row has ten numeric fields in this order: **years, P3, P10, P25, P50, P75, P90, P97, L, S**. Each table ends with the explicit footnote “50th percentile = model parameter M”. Centiles/M have three decimal places; L and S have four; age is printed with up to three decimal places. No separate M column, sex column, daily age, sample-size column, or posture footnote appears in these four tables.

The same ordered grid appears in all four: `0, 0.083, 0.167, 0.25, 0.333, 0.417, 0.5, 0.583, 0.667, 0.75, 0.833, 0.917, 1, 1.083, 1.167, 1.25, 1.333, 1.417, 1.5, 1.583, 1.667, 1.75, 1.833, 1.917, 2`, followed by half-year increments. Interpreting the first 25 nodes as months 0–24 is a strong inference from the pattern, not an explicit statement in the PDF. Retain printed ages in the source extraction and document any replacement with exact month fractions as a derivative change.

## Statistical compatibility and daily conversion risks

The article's §2.2 identifies GAMLSS **BCCGo**, with μ = M (median), σ = S (coefficient of variation), and ν = L (Box–Cox power). The supplement's labels and numeric reconstruction confirm the usual signed LMS orientation. For positive measurement x, `z = ((x/M)^L - 1)/(L*S)`; at L = 0 use `log(x/M)/S`. The inverse is `x = M*(1 + L*S*z)^(1/L)`, requiring a positive base; at L = 0 use `M*exp(S*z)`. Do not negate L or interpret S as a standard deviation in cm/kg. [Article §2.2](https://www.mdpi.com/2077-0383/14/16/5912).

Across all 31 birth–5 nodes and seven listed centiles per table, inverse LMS using standard-normal quantiles gave maximum absolute discrepancies from printed centiles of **0.007261 cm (S1), 0.007930 cm (S2), 0.001518 kg (S5), and 0.002230 kg (S6)**. These are conservative upward-rounded bounds from double-precision calculations and reflect rounded L/S/M. Printed centiles are rounded outputs of higher-precision models; requiring exact equality or only 0.0005-unit tolerance for reconstructed centiles would incorrectly reject this source. A source-node parity gate of 0.01 cm and 0.003 kg covers the observed errors, but is not a clinical accuracy guarantee. This checks central published centiles; extreme-tail behavior and BCCGo support/truncation handling require separate review before extrapolating z-scores.

Height L is positive throughout birth–5: girls 0.8513 through 1.167 then 0.5515 from 1.25; boys 0.5192 then 0.4620 at the same nodes. Girls' weight L crosses from +0.0055 at 1.083 to −0.0205 at 1.167; boys' from +0.0738 at 0.667 to −0.0060 at 0.75. All M/S values are positive. An interpolated derivative must support negative L and the L≈0 limit.

A compact set of 124 birth–5 LMS source rows is possible. A precomputed daily set is also possible, but would be an **app-authored approximation**, not a published daily PEZZ dataset. The source supplies neither a year-to-day constant nor a calendar-age rule, neither interpolation method nor original spline coefficients. Linear interpolation of L/M/S between source nodes preserves node values and is reproducible, but does not recover the original continuous GAMLSS model; linear interpolation of centiles is a different approximation. Choose and record an age convention, endpoint behavior, and interpolation algorithm before generating daily rows. Do not silently equate rounded 0.083 years with an exact 30-day age, or reuse WHO day indexing as if it were supplied here.

The article says height models join at 1.2 years and weight at 4.25 years. Neither join has a source row. Interpolating height L from 1.167 to 1.25 creates an intermediate L not published by either branch; linear interpolation across the weight 4–4.5 interval likewise approximates its join. Boundary tests and an explicit approximation notice are needed. The girls' weight median increases 14.342 → 15.001 → 16.294 kg at 3 → 3.5 → 4 years (S5 p.6); preserve those supplied values rather than smoothing or “correcting” them without authority.

### Independent published height values for golden checks

These values are from the paper's **main-text Table 3**, independent of the supplement extraction. Use them as rounded cross-checks (tolerance must reflect 0.1-cm publication rounding), not as the production table. All 42 selected P3/P50/P97 values below agree with the visually inspected supplement after rounding to 0.1 cm (S1 p.1, S2 pp.2–3). [Article Table 3](https://www.mdpi.com/2077-0383/14/16/5912).

| Age (years) | Girls P3 / P50 / P97 (cm) | Boys P3 / P50 / P97 (cm) |
| ---: | ---: | ---: |
| 0 | 46.3 / 49.7 / 53.2 | 47.0 / 50.5 / 54.2 |
| 0.25 | 55.6 / 59.8 / 64.0 | 57.1 / 61.2 / 65.5 |
| 0.5 | 61.6 / 66.2 / 70.9 | 63.6 / 67.9 / 72.3 |
| 1 | 69.3 / 74.5 / 79.7 | 71.1 / 76.0 / 81.1 |
| 2 | 81.0 / 86.8 / 92.8 | 82.2 / 88.1 / 94.3 |
| 3 | 89.0 / 95.7 / 102.6 | 90.1 / 96.9 / 103.9 |
| 5 | 102.0 / 110.3 / 119.0 | 103.1 / 111.4 / 120.0 |

### Independently transcribed supplement weight goldens

The main article prints no weight centile table. These values were independently read from the rendered S5/S6 pages, rather than generated by the LMS formula. They can catch extraction/column/sign defects, but are not a second external dataset. S5 values are on p.6; S6 ages 0–1 are on p.7 and 2–5 on p.8.

| Age (years) | Girls P3 / M=P50 / P97 (kg) | Girls L / S | Boys P3 / M=P50 / P97 (kg) | Boys L / S |
| ---: | --- | --- | --- | --- |
| 0 | 2.658 / 3.357 / 4.161 | 0.3668 / 0.1189 | 2.779 / 3.495 / 4.310 | 0.4026 / 0.1163 |
| 0.25 | 4.554 / 5.705 / 7.053 | 0.2753 / 0.1161 | 4.916 / 6.225 / 7.669 | 0.5535 / 0.1176 |
| 0.5 | 5.895 / 7.331 / 9.037 | 0.1917 / 0.1135 | 6.320 / 7.839 / 9.613 | 0.2555 / 0.1114 |
| 1 | 7.528 / 9.256 / 11.366 | 0.0316 / 0.1095 | 8.006 / 9.795 / 12.088 | -0.2026 / 0.1095 |
| 2 | 9.786 / 12.011 / 14.922 | -0.2750 / 0.1120 | 10.264 / 12.585 / 15.844 | -0.5635 / 0.1148 |
| 3 | 11.614 / 14.342 / 18.231 | -0.5716 / 0.1192 | 11.912 / 14.700 / 18.785 | -0.6743 / 0.1202 |
| 5 | 14.755 / 18.535 / 24.900 | -0.9910 / 0.1361 | 15.242 / 19.104 / 25.491 | -0.9556 / 0.1340 |

## Length/height posture and corrected age

- The Swiss society's **0–2-year** boys' and girls' length charts say to **add 1 cm to a standing measurement** before plotting (PDF pages 3 and 15, respectively). Its boys' and girls' 50–120 cm weight-for-length charts repeat the instruction. This differs from WHO's 0.7-cm convention. The PEZZ article itself does not state a posture adjustment. The society's 0–5 and 1–18 length charts do not show a corresponding note, and neither the article nor chart gives an explicit correction for a lying measurement after age two. Do not transfer the +1-cm rule beyond the charts that state it or silently retain the WHO 0.7-cm rule. [Society chart PDF](https://files.paediatrieschweiz.ch/production/uploads/2026/07/PCH_Wachstumskurven_260701_DE.pdf); [PEZZ paper](https://www.mdpi.com/2077-0383/14/16/5912).
- Implementing a **measurement normalization rule** in original app code, stated in our own words with a citation, would not reproduce the society's chart curves or artwork. Treat that as a narrow methodological inference from its plotting instruction, not a grant to copy the PDF or a claim that the instruction is validated for every age.
- The paper's newborn sample was term-only (39–41 weeks). It does not specify a corrected-age policy for children born early. Existing product agreement to support corrected age therefore requires an explicit clinical/source decision before showing Swiss percentiles for premature children; simply substituting corrected age into the Swiss formula is not validated by this paper. [Article §2.1](https://www.mdpi.com/2077-0383/14/16/5912).

## Integration gate

Use the source label “Swiss PEZZ 2025 reference” for weight and length/height percentiles. Artifact acquisition, hashing, rights-notice inspection, table schema, complete birth–5 coverage, signed LMS orientation, and rounded source-node parity are now verified. Before bundling, decide and validate the daily age/interpolation convention and join behavior; resolve 0–2 standing +1 cm, >2 posture, and corrected-age behavior; and include attribution, source/license links, and an explicit change notice for extraction/interpolation separately from the code license. No paid-source route is needed for this candidate if these remaining checks pass. This audit changes documentation only; it does not authorize or claim a production dataset import.
