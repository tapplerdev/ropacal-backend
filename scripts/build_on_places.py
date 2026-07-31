#!/usr/bin/env python3
"""Build internal/geo/data/on_places.json — Ontario municipal boundaries.

WHY
    geo/boundaries.go embeds true legal city outlines so the placement overlay
    can draw a real boundary instead of a bounding box. That asset is US Census
    TIGER, California only. A GTA tenant therefore gets the rectangle fallback
    for Toronto, Mississauga, Brampton and everywhere else.

SOURCE
    Ontario GeoHub, "Municipal Boundary - Lower and Single Tier", published by
    the Ministry of Municipal Affairs and Housing. 414 municipalities, exactly
    1:1 with Ontario's real local municipalities (241 lower-tier + 173 single-
    tier). Already NAD83 lat/lon (EPSG:4269 ~ WGS84 within 1-2 m), so no
    reprojection is needed — unlike the StatCan census files, which are in
    Statistics Canada Lambert and would need a projection step.

    Chosen over StatCan Census Subdivisions because CSDs are a superset: 578 in
    Ontario, the extra ~164 being reserves and unorganized territories that are
    not municipalities. Chosen over OpenStreetMap because ODbL share-alike
    attaches to an embedded extract, and because Ontario tags single-tier cities
    (Toronto, Hamilton) at a different admin level than lower-tier ones — a naive
    pull silently omits Toronto.

LICENCE (Open Government Licence - Ontario; attribution is required)
    "Contains information licensed under the Open Government Licence - Ontario."

EXTENT TYPES
    Each municipality is split into up to three records: Mainland, Islands and
    Water. Water is dropped — it extends municipal limits far into Lake Ontario,
    which would draw lakeshore cities with long tails out over open water and
    make point-in-polygon match boats. Mainland + Islands is the land extent.

USAGE
    curl -sL 'https://geohub.lio.gov.on.ca/api/download/v1/items/64fb702e16204c3e88b528d9759f1174/geojson?layers=14' -o on_raw.geojson
    python3 scripts/build_on_places.py on_raw.geojson
"""
import json
import math
import os
import sys

OUT = os.path.join(os.path.dirname(__file__), "..", "internal", "geo", "data", "on_places.json")

# Matches the ~33 m used for the TIGER assets, so both layers render with a
# comparable level of detail.
TOLERANCE_M = 30.0
DROP_EXTENTS = {"Water"}


def perp_distance_m(pt, a, b, mlat_scale):
    """Perpendicular distance from pt to segment a-b, in metres.

    Equirectangular approximation around the ring's mean latitude: at these
    distances the error is millimetres, and it avoids a projection dependency.
    """
    px, py = (pt[0] * mlat_scale, pt[1])
    ax, ay = (a[0] * mlat_scale, a[1])
    bx, by = (b[0] * mlat_scale, b[1])
    dx, dy = bx - ax, by - ay
    if dx == 0 and dy == 0:
        d = math.hypot(px - ax, py - ay)
    else:
        t = max(0.0, min(1.0, ((px - ax) * dx + (py - ay) * dy) / (dx * dx + dy * dy)))
        d = math.hypot(px - (ax + t * dx), py - (ay + t * dy))
    return d * 111320.0  # degrees of latitude -> metres


def simplify(ring, tol_m, mlat_scale):
    """Douglas-Peucker, iterative (recursion blows the stack on big rings)."""
    if len(ring) < 4:
        return ring
    keep = [False] * len(ring)
    keep[0] = keep[-1] = True
    stack = [(0, len(ring) - 1)]
    while stack:
        lo, hi = stack.pop()
        if hi <= lo + 1:
            continue
        worst_d, worst_i = -1.0, -1
        for i in range(lo + 1, hi):
            d = perp_distance_m(ring[i], ring[lo], ring[hi], mlat_scale)
            if d > worst_d:
                worst_d, worst_i = d, i
        if worst_d > tol_m:
            keep[worst_i] = True
            stack.append((lo, worst_i))
            stack.append((worst_i, hi))
    out = [p for p, k in zip(ring, keep) if k]
    # A polygon ring needs 4 points minimum (closed). If simplification ate it,
    # keep the original rather than emit an invalid ring.
    if len(out) < 4:
        return ring
    if out[0] != out[-1]:
        out.append(out[0])
    return out


def title_case(name):
    """'CITY OF ST. CATHARINES' shortform 'ST. CATHARINES' -> 'St. Catharines'."""
    return " ".join(w.capitalize() if not w.isdigit() else w for w in name.split())


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 1
    src = sys.argv[1]
    print(f"Reading {src} ...")
    data = json.load(open(src))
    feats = data.get("features", [])
    print(f"  {len(feats)} features")

    # Group every land part under its municipality id.
    by_mun = {}
    dropped = 0
    for f in feats:
        p = f["properties"]
        if p.get("MUNICIPAL_AREA_EXTENT_TYPE") in DROP_EXTENTS:
            dropped += 1
            continue
        munid = p.get("MUNID")
        short = p.get("MUNICIPAL_NAME_SHORTFORM") or p.get("MUNICIPAL_NAME") or ""
        if not munid or not short:
            continue
        rec = by_mun.setdefault(munid, {
            "name": title_case(short),
            "full": title_case(p.get("MUNICIPAL_NAME") or short),
            "type": p.get("MUNICIPAL_TYPE") or "",
            "upper": title_case(p.get("UPPER_TIER_MUNICIPALITY") or ""),
            "polys": [],
        })
        g = f.get("geometry") or {}
        if g.get("type") == "Polygon":
            rec["polys"].append(g["coordinates"])
        elif g.get("type") == "MultiPolygon":
            rec["polys"].extend(g["coordinates"])

    print(f"  {len(by_mun)} municipalities  ({dropped} water-extent records dropped)")

    out, pts_before, pts_after = [], 0, 0
    for munid, rec in by_mun.items():
        # Multiple parts per municipality are kept as a MultiPolygon rather than
        # unioned: geo/boundaries.go already handles MultiPolygon and holes, and
        # a real union would need a geometry library for no visible gain.
        simplified, minx, miny, maxx, maxy = [], 180.0, 90.0, -180.0, -90.0
        for rings in rec["polys"]:
            new_rings = []
            for ring in rings:
                ring = [(round(x, 6), round(y, 6)) for x, y in ring]
                pts_before += len(ring)
                mlat = sum(p[1] for p in ring) / len(ring)
                s = simplify(ring, TOLERANCE_M, math.cos(math.radians(mlat)))
                pts_after += len(s)
                for x, y in s:
                    minx, miny = min(minx, x), min(miny, y)
                    maxx, maxy = max(maxx, x), max(maxy, y)
                new_rings.append([[x, y] for x, y in s])
            if new_rings:
                simplified.append(new_rings)
        if not simplified:
            continue
        out.append({
            "name": rec["name"],
            "name_norm": " ".join(rec["name"].lower().split()),
            "namelsad": rec["full"],
            "parent": rec["upper"],
            "bbox": [minx, miny, maxx, maxy],
            "geometry": {"type": "MultiPolygon", "coordinates": simplified},
        })

    out.sort(key=lambda r: r["name"])
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as fh:
        json.dump(out, fh, ensure_ascii=False, separators=(",", ":"))

    size = os.path.getsize(OUT)
    print(f"Wrote {OUT}")
    print(f"  {len(out)} municipalities, {size/1048576:.2f} MB")
    print(f"  vertices {pts_before:,} -> {pts_after:,} ({100*pts_after/max(pts_before,1):.1f}% kept)")

    # Fail loudly if the GTA is not present — that is the whole reason this exists.
    idx = {r["name_norm"]: r for r in out}
    missing = []
    print("  GTA check:")
    for want in ["toronto", "mississauga", "brampton", "vaughan", "markham",
                 "oakville", "burlington", "pickering", "ajax", "richmond hill"]:
        r = idx.get(want)
        if r:
            print("    %-14s bbox %.3f,%.3f .. %.3f,%.3f" % (r["name"], *r["bbox"]))
        else:
            missing.append(want)
            print("    %-14s MISSING" % want)
    if missing:
        print(f"\nERROR: missing municipalities: {', '.join(missing)}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
