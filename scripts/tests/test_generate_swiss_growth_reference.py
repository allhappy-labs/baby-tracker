"""Contract tests for the published Swiss birth-through-five extraction.

Literal centiles and goldens come from the independently audited PDF/article,
not from the generator's LMS arithmetic.
"""

import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/generate-swiss-growth-reference.py"
SOURCE = ROOT / ".superpowers/sdd/2026-09-30-growth-measurements/source/jcm-14-05912-s001.zip"
AGES = "0 0.083 0.167 0.25 0.333 0.417 0.5 0.583 0.667 0.75 0.833 0.917 1 1.083 1.167 1.25 1.333 1.417 1.5 1.583 1.667 1.75 1.833 1.917 2 2.5 3 3.5 4 4.5 5".split()
MONTHS = list(range(25)) + [30, 36, 42, 48, 54, 60]
S1 = "46.284 47.379 48.491 49.732 50.977 52.102 53.215 0.8513 0.0371"
S6 = "2.779 2.997 3.227 3.495 3.775 4.039 4.310 0.4026 0.1163"


def fixture(table="S1", values=S1):
    metric = "height" if table in ("S1", "S2") else "weight"
    sex = "girls" if table in ("S1", "S5") else "boys"
    units = "cm" if metric == "height" else "kg"
    end = "18" if sex == "girls" else "20"
    header = f"Table {table}: Smoothed {metric} percentiles (in {units}) and model parameters for {sex} aged 0 to {end} years."
    return "\n".join([header, "Percentiles Model parameters", "years 3 10 25 50* 75 90 97 L S", *[f"{age} {values}" for age in AGES], "*50th percentile = model parameter M"])


class GeneratorTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.generator = None
        if SCRIPT.exists():
            spec = importlib.util.spec_from_file_location("swiss_generator", SCRIPT)
            cls.generator = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(cls.generator)

    def api(self):
        self.assertIsNotNone(self.generator, "Swiss reference generator is not implemented")
        return self.generator

    def test_maps_s1_birth_without_rounding(self):
        row = self.api().parse_table(fixture(), "S1")[0]
        self.assertEqual(row, dict(zip(
            "metric sex month printedYears p3 p10 p25 m p75 p90 p97 l s".split(),
            ["lengthHeight", "girls", 0, "0", "46.284", "47.379", "48.491", "49.732", "50.977", "52.102", "53.215", "0.8513", "0.0371"])))

    def test_maps_s6_month_and_sex_without_rounding(self):
        row = self.api().parse_table(fixture("S6", S6), "S6")[1]
        self.assertEqual((row["metric"], row["sex"], row["month"], row["printedYears"], row["m"], row["l"], row["s"]),
                         ("weight", "boys", 1, "0.083", "3.495", "0.4026", "0.1163"))

    def test_requires_31_ordered_prescribed_nodes(self):
        rows = self.api().parse_table(fixture(), "S1")
        self.assertEqual([row["month"] for row in rows], MONTHS)
        self.assertEqual([row["printedYears"] for row in rows], AGES)

    def test_rejects_duplicate_node(self):
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.api().parse_table(fixture().replace("0.083 " + S1, "0 " + S1), "S1")

    def test_rejects_missing_node(self):
        with self.assertRaisesRegex(ValueError, "31|missing|sequence"):
            self.api().parse_table(fixture().replace("0.083 " + S1 + "\n", ""), "S1")

    def test_rejects_unprescribed_printed_age(self):
        with self.assertRaisesRegex(ValueError, "age|sequence"):
            self.api().parse_table(fixture().replace("0.083 " + S1, "0.084 " + S1), "S1")

    def test_rejects_reordered_nodes(self):
        text = fixture().replace("0 " + S1 + "\n0.083 " + S1, "0.083 " + S1 + "\n0 " + S1)
        with self.assertRaisesRegex(ValueError, "sequence|order"):
            self.api().parse_table(text, "S1")

    def test_rejects_nonfinite_and_nonpositive_numbers(self):
        for bad in ("nan", "inf", "-inf", "0", "-1"):
            with self.subTest(value=bad), self.assertRaises(ValueError):
                self.api().parse_table(fixture().replace("0 " + S1, "0 " + S1.replace("49.732", bad)), "S1")
        for bad in ("nan", "inf", "-inf"):
            with self.subTest(l=bad), self.assertRaises(ValueError):
                self.api().parse_table(fixture().replace("0 " + S1, "0 " + S1.replace("0.8513", bad)), "S1")

    def test_accepts_signed_and_zero_l(self):
        negative = "8.006 8.529 9.103 9.795 10.552 11.294 12.088 -0.2026 0.1095"
        zero = "9.814 9.873 9.933 10.000 10.068 10.129 10.190 0.0000 0.0100"
        for values, want in ((negative, "-0.2026"), (zero, "0.0000")):
            with self.subTest(l=want):
                self.assertEqual(self.api().parse_table(fixture("S6", values), "S6")[0]["l"], want)

    def test_rejects_inverted_centiles_and_all_seven_parity_errors(self):
        api = self.api()
        with self.assertRaisesRegex(ValueError, "order"):
            api.parse_table(fixture().replace("47.379", "46.000"), "S1")
        for column in ("46.284", "47.379", "48.491", "50.977", "52.102", "53.215"):
            with self.subTest(column=column), self.assertRaisesRegex(ValueError, "LMS|centile"):
                api.parse_table(fixture().replace(column, f"{float(column) + 0.1:.3f}"), "S1")

    def test_requires_correct_table_header_units_and_m_footnote(self):
        for text in (fixture().replace("in cm", "in kg"), fixture().replace("for girls", "for boys"),
                     fixture().replace("50*", "50"), fixture().replace("*50th percentile = model parameter M", "")):
            with self.subTest(text=text[:100]), self.assertRaises(ValueError):
                self.api().parse_table(text, "S1")

    def test_rejects_missing_and_repeated_tables(self):
        with self.assertRaises(ValueError):
            self.api().parse_tables(fixture())
        with self.assertRaises(ValueError):
            self.api().parse_table(fixture() + "\n" + fixture(), "S1")

    def test_rejects_source_hash_mismatch_before_pdf_processing(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "source.zip"
            source.write_bytes(b"unverified source")
            with self.assertRaisesRegex(ValueError, "ZIP.*SHA-256"):
                self.api().load_source(source)

    def test_rejects_pdf_hash_mismatch(self):
        with self.assertRaisesRegex(ValueError, "PDF.*SHA-256"):
            self.api().verify_hash(b"unverified PDF", "fb05a53a8ffbbac45201ad8407ff8c0810abd1fa15e38e4d964681f6043f141a", "PDF")

    @unittest.skipUnless(SOURCE.exists(), "user-supplied source ZIP not retained locally")
    def test_source_goldens_and_deterministic_check(self):
        api = self.api()
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "SwissPEZZ2025.csv"
            api.generate(SOURCE, output)
            original = {path.name: path.read_bytes() for path in Path(directory).iterdir()}
            api.generate(SOURCE, output)
            self.assertEqual({path.name: path.read_bytes() for path in Path(directory).iterdir()}, original)
            api.generate(SOURCE, output, check=True)
            import csv
            rows = list(csv.DictReader(output.read_text().splitlines()))
            self.assertEqual(len(rows), 124)
            heights = {
                "girls": [(46.3,49.7,53.2),(55.6,59.8,64.0),(61.6,66.2,70.9),(69.3,74.5,79.7),(81.0,86.8,92.8),(89.0,95.7,102.6),(102.0,110.3,119.0)],
                "boys": [(47.0,50.5,54.2),(57.1,61.2,65.5),(63.6,67.9,72.3),(71.1,76.0,81.1),(82.2,88.1,94.3),(90.1,96.9,103.9),(103.1,111.4,120.0)],
            }
            weights = {
                "girls": ["2.658 3.357 4.161 0.3668 0.1189", "4.554 5.705 7.053 0.2753 0.1161", "5.895 7.331 9.037 0.1917 0.1135", "7.528 9.256 11.366 0.0316 0.1095", "9.786 12.011 14.922 -0.2750 0.1120", "11.614 14.342 18.231 -0.5716 0.1192", "14.755 18.535 24.900 -0.9910 0.1361"],
                "boys": ["2.779 3.495 4.310 0.4026 0.1163", "4.916 6.225 7.669 0.5535 0.1176", "6.320 7.839 9.613 0.2555 0.1114", "8.006 9.795 12.088 -0.2026 0.1095", "10.264 12.585 15.844 -0.5635 0.1148", "11.912 14.700 18.785 -0.6743 0.1202", "15.242 19.104 25.491 -0.9556 0.1340"],
            }
            for sex in ("girls", "boys"):
                for i, month in enumerate((0, 3, 6, 12, 24, 36, 60)):
                    height = next(r for r in rows if (r["metric"],r["sex"],r["month"]) == ("lengthHeight",sex,str(month)))
                    for key, value in zip(("p3", "m", "p97"), heights[sex][i]):
                        self.assertAlmostEqual(float(height[key]), value, delta=0.1)
                    weight = next(r for r in rows if (r["metric"],r["sex"],r["month"]) == ("weight",sex,str(month)))
                    self.assertEqual([weight[k] for k in ("p3", "m", "p97", "l", "s")], weights[sex][i].split())
            manifest = json.loads((output.parent / "SwissPEZZ2025-manifest.json").read_bytes())
            self.assertEqual(manifest["csv"]["sha256"], hashlib.sha256(original[output.name]).hexdigest())
            attribution = original["SwissPEZZ2025-ATTRIBUTION.md"]
            self.assertEqual(manifest["attribution"]["sha256"], hashlib.sha256(attribution).hexdigest())
            for name in original:
                path = output.parent / name
                path.write_bytes(original[name] + b"corrupt")
                with self.subTest(name=name), self.assertRaisesRegex(ValueError, "differs"):
                    api.generate(SOURCE, output, check=True)
                self.assertEqual(path.read_bytes(), original[name] + b"corrupt", "check must not overwrite")
                path.write_bytes(original[name])


if __name__ == "__main__":
    unittest.main()
