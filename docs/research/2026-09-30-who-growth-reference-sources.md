# WHO growth reference source and rights investigation

Date checked: 2026-09-30. Status: **blocked for bundling and distribution**.

## Exact official candidate files

The official WHO indicator pages link these six expanded z-score workbooks, covering girls and boys for each required age-based series:

- [Weight-for-age](https://www.who.int/toolkits/child-growth-standards/standards/weight-for-age)
- [Length/height-for-age](https://www.who.int/toolkits/child-growth-standards/standards/length-height-for-age)
- [Head circumference-for-age](https://www.who.int/toolkits/child-growth-standards/standards/head-circumference-for-age)

The following hashes identify the exact bytes retrieved from those official links. CDN `sfvrsn` values and workbook modification metadata are provenance, not a claimed scientific edition number. No independent semantic version was established. The tables were inspected in memory; no reference data is bundled or committed.

| File | Bytes | Workbook modified (UTC) | SHA-256 |
| --- | ---: | --- | --- |
| [wfa-girls-zscore-expanded-tables.xlsx](https://cdn.who.int/media/docs/default-source/child-growth/child-growth-standards/indicators/weight-for-age/expanded-tables/wfa-girls-zscore-expanded-tables.xlsx?sfvrsn=f01bc813_10) | 197671 | 2022-08-24T08:46:46Z | `ee3ae12cb96c6c5541cdf43665c03ce6c984f877859a183a5f6104eb06a49a6e` |
| [wfa-boys-zscore-expanded-tables.xlsx](https://cdn.who.int/media/docs/default-source/child-growth/child-growth-standards/indicators/weight-for-age/expanded-tables/wfa-boys-zscore-expanded-tables.xlsx?sfvrsn=65cce121_10) | 198984 | 2022-08-24T09:10:44Z | `b5b4748c6bfa5230e2eddafa1767629c349178b08d457f400b59422b8bfef86c` |
| [lhfa-girls-zscore-expanded-tables.xlsx](https://cdn.who.int/media/docs/default-source/child-growth/child-growth-standards/indicators/length-height-for-age/expandable-tables/lhfa-girls-zscore-expanded-tables.xlsx?sfvrsn=27f1e2cb_10) | 199890 | 2022-08-24T11:56:43Z | `6aa2876319449a6b1f4d825848128902114ff53c67b92b86a0c5140846013059` |
| [lhfa-boys-zscore-expanded-tables.xlsx](https://cdn.who.int/media/docs/default-source/child-growth/child-growth-standards/indicators/length-height-for-age/expandable-tables/lhfa-boys-zscore-expanded-tables.xlsx?sfvrsn=7b4a3428_12) | 200151 | 2022-08-24T09:09:30Z | `c4b1c9029ab9751a5f0888e32f35c7c0287a16d361885cf911ecf23b3f7f6b4f` |
| [hcfa-girls-zscore-expanded-tables.xlsx](https://cdn.who.int/media/docs/default-source/child-growth/child-growth-standards/indicators/head-circumference-for-age/expanded-tables/hcfa-girls-zscore-expanded-tables.xlsx?sfvrsn=3a34b8b0_8) | 186723 | 2022-08-24T11:57:39Z | `8eec3770d1027ce1b3b96a7b89fd1e77070558a7791b17b4462cda8a813324a3` |
| [hcfa-boys-zscore-expanded-tables.xlsx](https://cdn.who.int/media/docs/default-source/child-growth/child-growth-standards/indicators/head-circumference-for-age/expanded-tables/hcfa-boys-zscore-expanded-tables.xlsx?sfvrsn=2ab1bec8_8) | 185962 | 2022-08-24T11:58:20Z | `89a657bc466e85f6c8f2e5e7f4635e969bdcf982bb71e519273e43896a1c3314` |

## Rights evidence and release gate

The [WHO copyright policy](https://www.who.int/about/policies/publishing/copyright) permits licensed noncommercial reuse under CC BY-NC-SA 3.0 IGO, while requiring permission for commercial uses and for materials not issued under that license. This policy does not establish that the six workbooks carry that grant. A scan of every XML member in all six XLSX archives found no copyright, license, permission, or commercial-use notice. Absence of such a notice does not establish unrestricted reuse.

The [official expanded-table instructions](https://cdn.who.int/media/docs/default-source/child-growth/child-growth-standards/indicators/instructions-en.pdf?sfvrsn=5cec8c61_23) explain national health-card chart reproduction and contain a warranty disclaimer. They do not explicitly authorize this app's intended distribution. No permission from WHO was supplied or obtained, and no distribution model has been established that resolves the gate. The calculator and table bundle remain blocked under the approved design.

To unblock: document the actual app distribution and applicable grant for these exact files, or obtain written WHO permission covering redistribution/adaptation of the tables in the app, with any required attribution, notices, and restrictions. The [WHO permissions form](https://www.who.int/about/policies/publishing/permissions) is the policy's permission route; no request was submitted. A prospective attribution should name World Health Organization, WHO Child Growth Standards, the three indicators, the exact source links, and retrieval date; final license/disclaimer wording must follow the grant actually obtained. Do not claim WHO endorsement or include its logo without authorization.

## Alternate official repository

[WHO's anthro repository](https://github.com/WorldHealthOrganization/anthro/tree/master/data-raw/growthstandards) contains growth standards. Its [package DESCRIPTION](https://raw.githubusercontent.com/WorldHealthOrganization/anthro/master/DESCRIPTION) identifies WHO as a copyright holder and declares GPL-3; the root [LICENSE.md](https://github.com/WorldHealthOrganization/anthro/blob/master/LICENSE.md) contains GPLv3. This is evidence of a package license, not a confirmed permissive grant for the six separate CDN workbooks. The specific data licensing scope and app distribution obligations need review. No anthro data or code was copied, and this investigation does not select or apply a license to Baby Tracker.

## Technical constraints discovered for later calculator work

WHO's expanded-table instructions use ages in days, convert months using 30.4375 days/month, place the length/height discontinuity between days 730 and 731, and extend the tables through day 1856 to accommodate 60 completed months. This needs an explicit acceptance decision against the design's calendar 24-month and 60-month wording before implementing boundary tests; do not silently interpolate across the discontinuity or assume table extent equals the product limit. Numeric table contents and LMS/golden examples have not yet been validated.

Implementation direction confirmed during this task: follow WHO's published day-based cutoffs and position rules where explicitly defined; the corrected-age switch remains the third chronological calendar birthday. A second calendar birthday can fall on day 730 or 731 depending on leap days, so it cannot always be substituted for the WHO transition. Day 1856 also extends beyond the fifth calendar birthday. Confirm the accepted upper-limit behavior before calculator implementation.

## Rights-independent stable profile identity

UUIDv5 uses the baby UUID's 16 network-order bytes as namespace and exactly the UTF-8 name `growth-profile-v1`. It uses SHA-1 as required by UUIDv5, sets version 5 and RFC variant bits, and takes the first 16 hash bytes. This is a stable identifier, not authentication.

Independent Python `uuid.uuid5(UUID(baby), "growth-profile-v1")` vectors for Swift and Go:

| Baby namespace | Profile ID |
| --- | --- |
| `00000000-0000-0000-0000-000000000000` | `052ae0b7-e763-5d48-bf4c-b283a15c071f` |
| `123e4567-e89b-12d3-a456-426614174000` | `101dea4a-277e-597f-a0a7-aedaadde0cde` |
| `ffffffff-ffff-ffff-ffff-ffffffffffff` | `5793676f-fb9d-5024-b24d-6a3748cd8085` |

Storage enum values: `GrowthMetric` = `weight`, `lengthHeight`, `headCircumference`; `GrowthReferenceSex` = `girls`, `boys`; `LengthPosition` = `lying`, `standing`. Display labels will be localized separately.
