#!/usr/bin/env python3
"""Check that every translated string catalog covers the supported languages."""

import json
import re
from collections import Counter
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
LANGUAGES = ("de", "fr", "es", "it")
PLACEHOLDER = re.compile(r"%(?:\d+\$)?(?:ll)?(?:@|d|u|f)|\$\{[^}]+\}")


def tokens(value: str) -> Counter[str]:
    return Counter(re.sub(r"^%\d+\$", "%", token) for token in PLACEHOLDER.findall(value))


def forms(localization: dict) -> list[str]:
    """Every text a localization can show: its one string, or each plural form."""
    value = localization.get("stringUnit", {}).get("value")
    if value:
        return [value]
    plural = localization.get("variations", {}).get("plural", {})
    return [v for form in plural.values() if (v := form.get("stringUnit", {}).get("value"))]


def plural_has_other(localization: dict) -> bool:
    """Foundation falls back to 'other' for any category a language lacks."""
    return "other" in localization.get("variations", {}).get("plural", {})


def main() -> int:
    problems: list[str] = []
    count = 0
    paths = sorted((ROOT / "Apps").rglob("*.xcstrings")) + sorted(
        (ROOT / "Packages/BabyTracker/Sources").rglob("*.xcstrings")
    )
    for path in paths:
        catalog = json.loads(path.read_text())
        if catalog.get("sourceLanguage") != "en":
            problems.append(f"{path}: source language must be en")
        for key, item in catalog["strings"].items():
            if item.get("shouldTranslate") is False:
                continue
            count += 1
            # An identifier key (an APNs loc-key) carries its English text,
            # and therefore its placeholders, in the en localization.
            source = item.get("localizations", {}).get("en", {}).get("stringUnit", {}).get("value") or key
            expected = tokens(source)
            for language in LANGUAGES:
                values = forms(item.get("localizations", {}).get(language, {}))
                if not values:
                    problems.append(f"{path}: {language} is missing {key!r}")
                elif any(tokens(value) != expected for value in values):
                    problems.append(f"{path}: {language} placeholders differ for {key!r}")
                elif "variations" in item.get("localizations", {}).get(language, {}) and not plural_has_other(
                    item["localizations"][language]
                ):
                    problems.append(f"{path}: {language} plural forms lack 'other' for {key!r}")
    for problem in problems:
        print(problem)
    if problems:
        return 1
    print(f"OK: {count} catalog keys cover {', '.join(LANGUAGES)} in {len(paths)} catalogs")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
