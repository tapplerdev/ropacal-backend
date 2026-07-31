#!/usr/bin/env python3
"""Build internal/geo/data/cities.json from the GeoNames cities5000 dump.

WHY THIS EXISTS
    The AI placement recommender needs to answer "which nearby cities could this
    tenant expand into?" That list used to be eight hardcoded Bay Area cities,
    which meant a Toronto tenant was told to expand into Fremont and Hayward.

WHY GEONAMES
    It is the only source checked that has worldwide coverage, real populations,
    and coordinates in one file under a license that allows embedding in a
    commercial binary (CC BY 4.0 — attribution required, see ATTRIBUTION below).
    US Census/TIGER is US-only. HERE has no population field at all. OSM's
    population tags are missing for Oakville, Ajax, Newmarket and others in the
    GTA, which would silently drop real cities from a population ranking.

    cities5000 = every populated place over 5,000 people (~50k worldwide, 5.3 MB
    zipped). cities15000 is smaller but omits towns worth placing bins in;
    cities1000 is 2.6x larger for places mostly too small to matter.

ATTRIBUTION (required by CC BY 4.0, keep it shipped)
    "Includes data from GeoNames (https://www.geonames.org/), CC BY 4.0."

USAGE
    python3 scripts/build_cities.py            # download + build
    python3 scripts/build_cities.py --keep     # keep the raw download for inspection

GeoNames rebuilds the dump daily; re-run this whenever you want fresher
populations. Nothing breaks if it goes stale — city locations do not move.
"""
import argparse
import csv
import io
import json
import os
import subprocess
import sys
import zipfile

URL = "https://download.geonames.org/export/dump/cities5000.zip"
OUT = os.path.join(os.path.dirname(__file__), "..", "internal", "geo", "data", "cities.json")

# GeoNames "feature codes" for populated places we would actually consider.
# PPLX (a section of a populated place) and PPLL (a locality) are excluded:
# suggesting "expand into North York" when North York is part of Toronto is
# noise, and those entries also duplicate their parent city's coordinates.
KEEP_CODES = {
    "PPL",    # populated place
    "PPLA",   # seat of a first-order admin division (state/province capital)
    "PPLA2",  # seat of a second-order division (county seat)
    "PPLA3",
    "PPLA4",
    "PPLC",   # national capital
    "PPLG",   # seat of government
}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true", help="keep the raw zip")
    args = ap.parse_args()

    print(f"Downloading {URL} ...")
    # curl, not urllib: python.org builds on macOS ship without the system trust
    # store, so urllib fails with CERTIFICATE_VERIFY_FAILED on a clean machine.
    # curl is present on macOS and Linux and uses the OS trust store.
    try:
        raw = subprocess.run(
            ["curl", "-fsSL", "--max-time", "300", URL],
            check=True, capture_output=True,
        ).stdout
    except FileNotFoundError:
        print("ERROR: curl not found on PATH", file=sys.stderr)
        return 1
    except subprocess.CalledProcessError as e:
        print(f"ERROR: download failed ({e.returncode}): {e.stderr[:200]!r}", file=sys.stderr)
        return 1
    print(f"  {len(raw)/1048576:.1f} MB")

    if args.keep:
        with open("cities5000.zip", "wb") as fh:
            fh.write(raw)

    with zipfile.ZipFile(io.BytesIO(raw)) as z:
        text = z.read("cities5000.txt").decode("utf-8")

    rows, skipped = [], 0
    # Tab-separated, no header, no quoting — QUOTE_NONE matters because real
    # place names contain quote characters.
    for f in csv.reader(io.StringIO(text), delimiter="\t", quoting=csv.QUOTE_NONE):
        if len(f) < 15:
            skipped += 1
            continue
        name, lat, lng, fcode, country, admin1, pop = (
            f[1], f[4], f[5], f[7], f[8], f[10], f[14],
        )
        if fcode not in KEEP_CODES:
            skipped += 1
            continue
        try:
            lat_f, lng_f, pop_i = float(lat), float(lng), int(pop or 0)
        except ValueError:
            skipped += 1
            continue
        # Population 0 means "unknown" in GeoNames, not "empty". Keeping those
        # would put unrankable entries at the bottom of every list forever.
        if pop_i <= 0:
            skipped += 1
            continue
        rows.append({
            "name": name,
            "lat": round(lat_f, 5),   # ~1 m; these are centroids, not boundaries
            "lng": round(lng_f, 5),
            "country": country,
            "admin1": admin1,
            "pop": pop_i,
        })

    rows.sort(key=lambda r: (-r["pop"], r["name"]))

    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as fh:
        json.dump(rows, fh, ensure_ascii=False, separators=(",", ":"))

    size = os.path.getsize(OUT)
    print(f"Wrote {OUT}")
    print(f"  {len(rows):,} cities, {size/1048576:.2f} MB  (skipped {skipped:,})")

    # Sanity-check the two metros this was built for. A silent regression here
    # (wrong column index after a GeoNames format change) would be invisible
    # until a tenant got nonsense recommendations.
    by_name = {}
    for r in rows:
        by_name.setdefault((r["name"], r["country"]), r)
    checks = [
        ("Mississauga", "CA"), ("Brampton", "CA"), ("Vaughan", "CA"),
        ("Markham", "CA"), ("Oakville", "CA"),
        ("Hayward", "US"), ("Fremont", "US"), ("Oakland", "US"),
    ]
    print("  spot check:")
    missing = []
    for key in checks:
        r = by_name.get(key)
        if r:
            print(f"    {key[0]:<14} pop {r['pop']:>9,}  ({r['lat']}, {r['lng']})")
        else:
            missing.append(key[0])
            print(f"    {key[0]:<14} MISSING")
    if missing:
        print(f"\nERROR: expected cities missing: {', '.join(missing)}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
