#!/usr/bin/env python3
"""Add plain (non-plural) translated keys to a String Catalog.

usage: add-catalog-strings.py CATALOG.xcstrings ENTRIES.json

ENTRIES.json maps each source-language key to {"de": "...", "es": "...", ...}.
A key already in the catalog is replaced. Other keys keep their order, so the
diff holds only the keys added.
"""
import json
import sys


def main() -> int:
    catalog_path, entries_path = sys.argv[1], sys.argv[2]
    with open(catalog_path, encoding="utf-8") as f:
        catalog = json.load(f)
    with open(entries_path, encoding="utf-8") as f:
        entries = json.load(f)
    for key, translations in entries.items():
        catalog["strings"][key] = {
            "localizations": {
                language: {"stringUnit": {"state": "translated", "value": value}}
                for language, value in translations.items()
            }
        }
    with open(catalog_path, "w", encoding="utf-8") as f:
        json.dump(catalog, f, ensure_ascii=False, indent=2, separators=(",", ": "))
        f.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
