# Swiss PEZZ 2025 reference — attribution

Derived aggregate reference data from Eiholzer U, Stephan A, Dubinski I,
Fritz C, and Noordam C, *Updated Swiss Growth References 2025:
No Height Differences, but BMI Variations Associated with Migration*,
*Journal of Clinical Medicine* 14 (2025), 5912. Publisher: MDPI.

Article: https://www.mdpi.com/2077-0383/14/16/5912
DOI: https://doi.org/10.3390/jcm14165912
Official supplementary material: https://www.mdpi.com/article/10.3390/jcm14165912/s1

© 2025 by the authors; licensed to MDPI. The published tables and this extraction
are available under Creative Commons Attribution 4.0 International (CC BY 4.0):
https://creativecommons.org/licenses/by/4.0/
Publisher supplementary-material reuse policy: https://www.mdpi.com/openaccess
No endorsement by the authors, PEZZ, or MDPI is implied. The app's original code
has separate licensing; this notice applies to the reference data.

## Extraction and application changes

Extracted only Tables S1, S2, S5, and S6 at printed ages 0 through 5 years; preserved published decimal precision and renamed P50 as M. Mapped rounded printed ages to exact calendar month-anniversary nodes 0–24, 30, 36, 42, 48, 54, and 60; this mapping is an app-authored inference. The app uses calendar month anniversaries and linear interpolation of L/M/S between these nodes, including across published model joins; this is an app-authored approximation, not the original continuous GAMLSS model. No daily rows, further rounding, smoothing, or extrapolation were introduced.

S1 = girls' length/height in cm; S2 = boys' length/height in cm;
S5 = girls' weight in kg; S6 = boys' weight in kg. P3/P10/P25/P50/P75/P90/P97
are retained; P50 is the model median M. L is the signed Box–Cox power and S is
the coefficient of variation. Source-node LMS reconstruction is verified to
0.01 cm / 0.003 kg at all seven centiles, allowing for printed model rounding.
Only these aggregate birth-through-five rows are bundled; source ZIP/PDF files
and the separate pädiatrie schweiz combined charts are not included.

## Reproducible provenance (SHA-256)

Original ZIP: `jcm-14-05912-s001.zip`
`0def88b3a5e0096b54064b7d9459e683081364943cab341e179f947a46d61336`

Selected PDF member: `jcm-3796415 Supplemental material to Manuscript Cohort 2019+2025 Supp. Table S1-S8 Figure S1-S3. (Edited).pdf`
`fb05a53a8ffbbac45201ad8407ff8c0810abd1fa15e38e4d964681f6043f141a`

Derived CSV:
`5f01d540dcab723a8a8b017bdbc639f53186f333d5c78a56cd02e47fb61de926`

The accompanying deterministic manifest records the source, table mapping, age
grid, CSV hash, and this attribution's hash. It contains no generation timestamp
or machine-specific path.
