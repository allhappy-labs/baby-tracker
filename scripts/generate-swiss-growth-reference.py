#!/usr/bin/env python3
"""Verify and extract published Swiss PEZZ 2025 birth-through-five source nodes.

Requires macOS PDFKit via Swift; no downloads or third-party Python packages.
--check validates all three deterministic output artifacts without changing them.
"""

import argparse
import csv
import hashlib
import io
import json
import math
from pathlib import Path
import re
from statistics import NormalDist
import subprocess
import tempfile
import zipfile


ZIP_NAME = "jcm-14-05912-s001.zip"
ZIP_SHA256 = "0def88b3a5e0096b54064b7d9459e683081364943cab341e179f947a46d61336"
PDF_NAME = "jcm-3796415 Supplemental material to Manuscript Cohort 2019+2025 Supp. Table S1-S8 Figure S1-S3. (Edited).pdf"
PDF_SHA256 = "fb05a53a8ffbbac45201ad8407ff8c0810abd1fa15e38e4d964681f6043f141a"
ARTICLE_URL = "https://www.mdpi.com/2077-0383/14/16/5912"
SOURCE_URL = "https://www.mdpi.com/article/10.3390/jcm14165912/s1"
LICENSE_URL = "https://creativecommons.org/licenses/by/4.0/"
COLUMNS = "metric sex month printedYears p3 p10 p25 m p75 p90 p97 l s".split()
CENTILES = (3, 10, 25, 50, 75, 90, 97)
AGES = "0 0.083 0.167 0.25 0.333 0.417 0.5 0.583 0.667 0.75 0.833 0.917 1 1.083 1.167 1.25 1.333 1.417 1.5 1.583 1.667 1.75 1.833 1.917 2 2.5 3 3.5 4 4.5 5".split()
MONTHS = list(range(25)) + [30, 36, 42, 48, 54, 60]
TABLES = {
    "S1": ("lengthHeight", "girls", "height", "cm", 18),
    "S2": ("lengthHeight", "boys", "height", "cm", 20),
    "S5": ("weight", "girls", "weight", "kg", 18),
    "S6": ("weight", "boys", "weight", "kg", 20),
}
CHANGE_NOTICE = (
    "Extracted only Tables S1, S2, S5, and S6 at printed ages 0 through 5 years; "
    "preserved published decimal precision and renamed P50 as M. "
    "Mapped rounded printed ages to exact calendar month-anniversary nodes "
    "0–24, 30, 36, 42, 48, 54, and 60; this mapping is an app-authored inference. "
    "The app uses calendar month anniversaries and linear interpolation of L/M/S "
    "between these nodes, including across published model joins; this is an "
    "app-authored approximation, not the original continuous GAMLSS model. "
    "No daily rows, further rounding, smoothing, or extrapolation were introduced."
)


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def verify_hash(data, expected, label):
    if sha256(data) != expected:
        raise ValueError(f"{label} SHA-256 mismatch: source is not the verified artifact")


def load_source(source):
    data = Path(source).read_bytes()
    verify_hash(data, ZIP_SHA256, "ZIP")
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        if archive.namelist().count(PDF_NAME) != 1:
            raise ValueError("ZIP must contain exactly one verified table PDF member")
        pdf = archive.read(PDF_NAME)
    verify_hash(pdf, PDF_SHA256, "PDF")
    return pdf


def extract_text(pdf):
    # Never expand the source archive into the repo or copy inputs into the bundle.
    with tempfile.TemporaryDirectory(prefix="swiss-growth-") as directory:
        source = Path(directory) / "tables.pdf"
        source.write_bytes(pdf)
        extractor = Path(__file__).with_name("extract-swiss-pdf-text.swift")
        result = subprocess.run(["swift", str(extractor), str(source)],
                                text=True, capture_output=True, check=True)
        return result.stdout


def parse_table(text, table):
    if table not in TABLES:
        raise ValueError(f"Unsupported table {table}")
    metric, sex, source_metric, units, end = TABLES[table]
    # Delimit at the next table/figure, rather than at page boundaries: S2/S6
    # continue onto a second page, with repeated column headers.
    headers = list(re.finditer(rf"^Table {table}:.*$", text, re.MULTILINE))
    if len(headers) != 1:
        raise ValueError(f"{table}: missing or repeated table header")
    expected = f"Table {table}: Smoothed {source_metric} percentiles (in {units}) and model parameters for {sex} aged 0 to {end} years."
    if headers[0].group().strip() != expected:
        raise ValueError(f"{table}: unexpected scope, sex, or units in header")
    body = text[headers[0].end():]
    boundary = re.search(r"^(?:Table S\d+:|Figure S\d+:)", body, re.MULTILINE)
    if boundary:
        body = body[:boundary.start()]
    lines = [" ".join(line.split()) for line in body.splitlines() if line.strip()]
    if not lines or lines[0] != "Percentiles Model parameters":
        raise ValueError(f"{table}: missing parameter header")
    if "*50th percentile = model parameter M" not in lines:
        raise ValueError(f"{table}: missing P50=M footnote")
    if "years 3 10 25 50* 75 90 97 L S" not in lines:
        raise ValueError(f"{table}: missing centile/parameter column header")

    rows = []
    seen = set()
    for line in lines:
        if line.startswith("years"):
            if line != "years 3 10 25 50* 75 90 97 L S":
                raise ValueError(f"{table}: unexpected column header")
            continue
        tokens = line.split()
        if not re.match(r"^(?:[+\-]?\d|[+\-]?(?:nan|inf))", tokens[0], re.IGNORECASE):
            continue
        try:
            age = float(tokens[0])
        except ValueError as error:
            raise ValueError(f"{table}: malformed age") from error
        if not math.isfinite(age) or age < 0:
            raise ValueError(f"{table}: age must be finite and nonnegative")
        if age > 5:
            continue
        if tokens[0] in seen:
            raise ValueError(f"{table}: duplicate age {tokens[0]}")
        seen.add(tokens[0])
        if len(tokens) != 10:
            raise ValueError(f"{table}: row must have ten numeric fields")
        try:
            values = [float(token) for token in tokens]
        except ValueError as error:
            raise ValueError(f"{table}: malformed numeric field") from error
        if not all(math.isfinite(value) for value in values):
            raise ValueError(f"{table}: nonfinite parameter or centile")
        centiles, l, s = values[1:8], values[8], values[9]
        if any(value <= 0 for value in centiles) or s <= 0:
            raise ValueError(f"{table}: centiles/M and S must be positive")
        if any(a >= b for a, b in zip(centiles, centiles[1:])):
            raise ValueError(f"{table}: centiles must be strictly ordered")
        m = centiles[3]  # The published footnote explicitly identifies P50 as M.
        tolerance = 0.01 if units == "cm" else 0.003
        for percentile, printed in zip(CENTILES, centiles):
            z = NormalDist().inv_cdf(percentile / 100)
            if l == 0:
                reconstructed = m * math.exp(s * z)
            else:
                base = 1 + l * s * z
                if base <= 0:
                    raise ValueError(f"{table}: LMS reconstruction has nonpositive support")
                reconstructed = m * math.exp(math.log(base) / l)
            if not math.isfinite(reconstructed) or abs(reconstructed - printed) > tolerance:
                raise ValueError(f"{table}: LMS P{percentile} reconstruction exceeds {tolerance} {units}")
        rows.append(dict(zip(COLUMNS, [metric, sex, 0, *tokens])))
    if len(rows) != 31 or [row["printedYears"] for row in rows] != AGES:
        raise ValueError(f"{table}: requires 31 nodes in the exact prescribed printed age sequence")
    for row, month in zip(rows, MONTHS):
        row["month"] = month
    return rows


def parse_tables(text):
    rows = [row for table in TABLES for row in parse_table(text, table)]
    if len(rows) != 124:
        raise ValueError("Expected exactly 124 published reference rows")
    return rows


def csv_bytes(rows):
    output = io.StringIO(newline="")
    writer = csv.DictWriter(output, fieldnames=COLUMNS, lineterminator="\n")
    writer.writeheader()
    writer.writerows(rows)
    return output.getvalue().encode("utf-8")


def attribution_bytes(csv_hash):
    return f"""# Swiss PEZZ 2025 reference — attribution

Derived aggregate reference data from Eiholzer U, Stephan A, Dubinski I,
Fritz C, and Noordam C, *Updated Swiss Growth References 2025:
No Height Differences, but BMI Variations Associated with Migration*,
*Journal of Clinical Medicine* 14 (2025), 5912. Publisher: MDPI.

Article: {ARTICLE_URL}
DOI: https://doi.org/10.3390/jcm14165912
Official supplementary material: {SOURCE_URL}

© 2025 by the authors; licensed to MDPI. The published tables and this extraction
are available under Creative Commons Attribution 4.0 International (CC BY 4.0):
{LICENSE_URL}
Publisher supplementary-material reuse policy: https://www.mdpi.com/openaccess
No endorsement by the authors, PEZZ, or MDPI is implied. The app's original code
has separate licensing; this notice applies to the reference data.

## Extraction and application changes

{CHANGE_NOTICE}

S1 = girls' length/height in cm; S2 = boys' length/height in cm;
S5 = girls' weight in kg; S6 = boys' weight in kg. P3/P10/P25/P50/P75/P90/P97
are retained; P50 is the model median M. L is the signed Box–Cox power and S is
the coefficient of variation. Source-node LMS reconstruction is verified to
0.01 cm / 0.003 kg at all seven centiles, allowing for printed model rounding.
Only these aggregate birth-through-five rows are bundled; source ZIP/PDF files
and the separate pädiatrie schweiz combined charts are not included.

## Reproducible provenance (SHA-256)

Original ZIP: `{ZIP_NAME}`
`{ZIP_SHA256}`

Selected PDF member: `{PDF_NAME}`
`{PDF_SHA256}`

Derived CSV:
`{csv_hash}`

The accompanying deterministic manifest records the source, table mapping, age
grid, CSV hash, and this attribution's hash. It contains no generation timestamp
or machine-specific path.
""".encode("utf-8")


def generate(source, output, check=False):
    output = Path(output)
    rows = parse_tables(extract_text(load_source(source)))
    data = csv_bytes(rows)
    attribution_path = output.with_name(output.stem + "-ATTRIBUTION.md")
    manifest_path = output.with_name(output.stem + "-manifest.json")
    attribution = attribution_bytes(sha256(data))
    manifest = {
        "schemaVersion": 1,
        "reference": "Swiss PEZZ 2025",
        "articleURL": ARTICLE_URL,
        "licenseURL": LICENSE_URL,
        "source": {"name": ZIP_NAME, "sha256": ZIP_SHA256, "url": SOURCE_URL,
                   "member": {"name": PDF_NAME, "sha256": PDF_SHA256}},
        "csv": {"name": output.name, "sha256": sha256(data), "columns": COLUMNS, "rowCount": len(rows)},
        "attribution": {"name": attribution_path.name, "sha256": sha256(attribution)},
        "tables": {table: {"metric": spec[0], "sex": spec[1], "unit": spec[3], "rowCount": 31}
                   for table, spec in TABLES.items()},
        "ageGrid": [{"month": month, "printedYears": age} for month, age in zip(MONTHS, AGES)],
        "lmsTolerance": {"lengthHeightCm": 0.01, "weightKg": 0.003, "centiles": CENTILES},
        "changeNotice": CHANGE_NOTICE,
    }
    manifest_data = (json.dumps(manifest, indent=2, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")
    artifacts = {output: data, attribution_path: attribution, manifest_path: manifest_data}
    if check:
        for path, expected in artifacts.items():
            if not path.is_file() or path.read_bytes() != expected:
                raise ValueError(f"{path.name} differs from the verified deterministic extraction")
    else:
        output.parent.mkdir(parents=True, exist_ok=True)
        for path, content in artifacts.items():
            path.write_bytes(content)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path, help="user-supplied official ZIP")
    parser.add_argument("--output", required=True, type=Path, help="derived CSV destination")
    parser.add_argument("--check", action="store_true", help="compare CSV, attribution, and manifest without writing")
    args = parser.parse_args()
    try:
        generate(args.source, args.output, args.check)
    except (ValueError, OSError, zipfile.BadZipFile, subprocess.CalledProcessError) as error:
        parser.exit(1, f"Swiss reference extraction failed: {error}\n")
    print(f"{'Verified' if args.check else 'Generated'} 124 Swiss PEZZ 2025 reference rows and provenance artifacts")


if __name__ == "__main__":
    main()
