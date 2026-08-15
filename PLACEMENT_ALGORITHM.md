# The Placement Recommender

How Binly decides where a new bin should go. Read this before changing anything
in `chat_locations.go` scoring, and before proposing a new feature — several
obvious ideas have already been measured and rejected, with numbers, below.

## What it is

A Claude tool called `recommend_bin_locations`. The dashboard's placement panel
and the AI drawer both reach it through `POST /api/manager/chat`. Claude decides
*when* to call it and how to word the answer; it does not do the scoring. The
scoring is arithmetic.

The critical parameters are **not** left to the model. `injectTargetArea` forces
the dashboard's pinned area onto every call and strips any `target_city` the
model guessed, and `injectNumber` does the same for the expansion-radius slider.
This is deliberate: an agentic follow-up call once re-derived "Brentwood" from an
area label and resolved it to the wrong same-named city, and the last tool call
wins.

## Where it lives

| file | role |
|---|---|
| `handlers/chat_locations.go` | the whole pipeline: candidate generation, filters, scoring |
| `handlers/placement_predict.go` | opt-in predicted-yield scoring (see below) |
| `handlers/placement_logging.go` | freezes decision-time features to `placement_decisions` |
| `handlers/esri_enrichment.go` | ESRI batch enrichment (crime gate only) |
| `handlers/anchor_chains.go`, `anchor_match.go` | country-scoped anchor tenants |
| `handlers/placement_opportunities.go` | separate city-level scorer (map polygons) |
| `handlers/growth.go` | separate scorer for driver-suggested `potential_locations` |

There are **three different scorers** and they do not share a model. Unifying
them has been the plan since 2026-07 and has not happened.

## The pipeline

1. **Resolve the target.** Structured `target_area` preferred; a typed name gets
   geocoded and returns `disambiguation_needed` if ambiguous rather than guessing.
   Cities attach a real TIGER polygon; districts stay on bbox.
2. **Generate candidates** via HERE search. Mode decides the origins:
   `infill` clusters existing bins (gated to bins clearing 10%/day), `expand`
   tiles the target bbox or sweeps uncovered cities within the warehouse radius,
   `relocate` circles one bin, `auto` is a 70/30 expansion-led mix.
3. **Filter** — one centralized pass: no-go zones, B2B title keywords,
   malls/Safeway, minimum gap, pending spots, per-mode rules. Then core+halo
   geometric classification (`in_area` / `near_area`).
4. **ESRI gate** — crime index and missing-data only. Income and apparel spend
   were REMOVED after calibration (see history). Fails open, so the response
   carries `safety_gate`.
5. **Score** (below), cut at the quality bar, keep near-misses separately.
6. **Refine** — `refineByAreaProfile` keeps `near_area` picks only if they match
   the area's median feature profile and aren't spatial outliers.

## The score

```
density^0.4 × anchor^0.3 × fill^0.2 × pop^0.1 × 10
```

This is a **Cobb-Douglas function** — the exponents sum to 1.0. It was arrived at
by hand, but it is a standard functional form, and because it is linear in logs
the exponents are estimable by ordinary regression rather than being permanent
guesses.

Multiplicative on purpose: weakness in any one dimension tanks the result, so a
big-box anchor in a dead zone loses to the same anchor in a busy plaza.

- `density` — errand-retail POIs within 300m, `ln(1+n)/ln(1+100)`. The ceiling is
  tied to `browseFetchLimit` because any lower ceiling clamps real candidates to
  an identical 1.0 and destroys their ordering. **This was learned twice.**
- `anchor` — 1.0 national / 0.7 regional / 0.15 none, country-scoped.
- `fill` — realized rate of the *nearest existing bin*. See "known defects".
- `pop` — ZIP residential population, neutral 1.0 when unknown.

## History — why it looks like this

**2026-06:** started as a weighted sum over census income, population, HERE
traffic jam factor and gap distance. Grew a POI whitelist, B2B filter, anchor
snapping, GraphVenn candidate service, and Claude Vision satellite checks. Most
of that is gone.

**2026-06-25:** v2. Scoring went multiplicative, ESRI arrived as a gate, vision
was switched off (it could not tell an office park from a retail plaza).

**2026-07-11 — the calibration that shaped everything.** 93 live bins measured
against `fill_rate_per_day`. All five ESRI demographic candidates failed. Median
income came back at **ρ = −0.15**, meaning the income gate was pointing the wrong
way and filtering out good sites. Errand-retail density led at **+0.39**, anchor
**+0.35**, population **+0.20**. Income and apparel gates deleted; exponents set
from these numbers.

**2026-07-12:** area targeting became geometry rather than city-name strings
(a Brentwood LA candidate reports its postal city as "Los Angeles"). TIGER
polygons, bbox tiling, core+halo.

**2026-07-22 to 07-26:** de-regioned nationwide. `auto` flipped from infill-led
to expansion-led — infill assumes a site out-draws one bin's capacity, and almost
no bins overflow. Fill rates became per-pitch rather than per-bin-lifetime (bin
78's entire 11.5%/day came from sitting in the yard).

**2026-07-30/31:** expansion cities derived from the org's own warehouse, ranked
by **drive time** — population was measured and does not predict fill
(ρ = −0.122; the best bin in the fleet is in Newark, pop 45k, and a 100k floor
would have excluded it). `signalcheck` returned out-of-sample **ρ = +0.167 on
n = 79**, below the significance bar, so the formula was frozen pending more bins.

**2026-08-14 — see the two sections below.**

## Features that have been TESTED AND REJECTED

Measured against realized fill on 73–78 bins with a current-pitch rate. The
incumbent errand-POI count reproduced at **+0.239** in the same harness, which is
what makes these negatives trustworthy rather than a broken measurement.

| feature | result | why it fails |
|---|---|---|
| **AADT** (Caltrans vehicle counts) | ρ = −0.023 | Freeway frontage has the highest counts and mediocre fill. Volume measures the road, not whether anyone stops. Coverage is also poor: 5% of bins within 100m of a count point, median 607m. |
| **Popular-times weekend/weekday ratio** | ρ = −0.019 | Google popular times is self-normalised per venue, so magnitude is unusable; the *shape* was the hypothesis, and it is flat. |
| **Review count / review-weighted density** | ρ = +0.122 raw | Review weighting adds nothing (mean-reviews-per-place ρ = +0.059). |
| **HERE traffic jam factor** | never measured | Was 15% of the v1 score, went dark in the v2 rewrite. It measures *congestion*, not volume — a jammed two-lane street scores high, a free-flowing arterial low. |
| ESRI income, charitable MPI, apparel spend, recent-mover, daytime workers | all ≤ 0 | 2026-07 calibration. One "affluent suburb" factor, wrong sign. |

**Do not re-propose these without new evidence.** Scripts to re-run any of them
live in the session scratchpad (`aadt_test.py`, `features.py`, `analyze.py`).

### The trap that nearly shipped

Google place count appeared to score **ρ = +0.320**, beating the incumbent and
looking nearly orthogonal to it. It was an artifact. 53% of bins piled up at
exactly 16 places — the scraper's per-query ceiling. Split at that boundary and
the correlation **reverses to −0.173**; the pooled number was a two-group
difference between "sparse area" and "hit the cap", not a gradient.

**Any feature built from a capped API call needs this check before you believe
it.** The same censoring shape has now bitten three times here: the `limit=20`
density ceiling, the 100%-full fill readings, and this.

## Known defects

**1. The fill term is circular.** It scores a candidate using the realized rate
of the nearest existing bin — a fact about where the fleet already is, not about
the site. The model can therefore only discover places resembling places already
chosen. The same circularity was recognised and removed from the growth ranker
and remains in the main formula.

**2. No competition or catchment.** The standard model for retail siting (Huff)
makes a site's value depend on what else competes for the same customers. This
scores each site in isolation and substitutes a hard 0.3-mile minimum gap — a
cliff where reality is a smooth decay. Two spots half a mile apart both score
well and would split one stream in practice.

**3. The score is unfalsifiable.** "8.5 / 10" makes no claim about the world, so
no outcome can contradict it. This is the reason there is no feedback loop, and
it is not an agent problem.

**4. The ceiling is the data, not the model.** Site features explain roughly
**6% of the variance** in fill rate (R² = 0.057 for errand-POI; 0.106 for the
best two-feature combination). About 90% of why one bin outperforms another is
not about the site — servicing cadence, season, driver, the bin itself. No model
class recovers signal that was never in the inputs.

## The predicted-yield mode (opt-in)

`placement_predict.go`, behind `PLACEMENT_PREDICT_FILL=1`. Production ordering is
unchanged unless it is explicitly turned on.

It addresses defects 1 and 3. Taking logs turns the same multiplicative form into
a linear regression, so the exponents are **fitted** and the output lands in
%/day rather than an arbitrary index. The circular fill term is dropped.

Fitted on 78 bins:

```
predicted fill = 9.274 × density^0.364 × anchor^0.109 × pop^0.108
```

| term | hand-tuned | fitted |
|---|---|---|
| density | 0.400 | 0.364 |
| **anchor** | **0.300** | **0.109** |
| pop | 0.100 | 0.108 |

Density and population were nearly right. **Anchor was over-weighted about 3×** —
picks were riding on brand presence rather than surrounding retail density.

**Honest accuracy.** Leave-one-out MAE 2.14 %/day against 2.25 for predicting the
median: a 4.7% improvement. Predictions compress into roughly 5–8 %/day while
real bins span 2.4–15.5, so it under-calls strong sites and over-calls weak ones.
That is what a 6%-of-variance signal looks like when forced to state a number,
and it is the picture the 0–10 scale concealed. Treat the output as "roughly
typical, or notably below typical" and nothing finer.

Ranking is barely affected: before/after on the same pinned area gave 67–100%
overlap with rank agreement ρ +0.71 to +1.00. The largest single move was a
tier-2-anchor CVS falling from #1 to #5 in infill mode, which is the anchor
correction working as intended.

**The value is the feedback loop, not better picks.** Every recommendation is now
a bet that settles once the bin has a few checks.

## The refit loop (self-updating weights)

The coefficients are no longer typed in. `handlers/placement_refit.go` runs
**weekly per organization**, recomputes them from realized outcomes, and writes
them to `placement_model`; the scorer reads the active row (15-minute cache,
invalidated on promotion) and falls back to the constants in
`placement_predict.go` when there is no fit.

- **Fitting lives in `internal/placementfit`** — pure, no DB, no clock, so it is
  testable. Its tests fit synthetic data from a KNOWN model and require the
  coefficients back; one test asserts that a fit on pure noise does **not** claim
  to beat the baseline, which is the only thing protecting the promotion gate.
- **The label is bins, not `placement_decisions`.** Most recommendations never
  become bins, so that table cannot supply a realized fill rate for most rows.
  It stays the right source for selection-bias work once explore slots and real
  propensities exist; it is not the training set today.
- **Promotion guardrail:** a fit is stored but only promoted if its leave-one-out
  MAE beats "predict the median". Failed fits are recorded too — a refit that
  got worse is evidence, and hiding it would hide a degrading model.
- **In-sample error is never reported.** With four coefficients and <100 rows it
  only shows the model can memorise, and would make every refit look like a win.
- **Weekly on purpose.** At this sample size coefficients are noisy; refitting
  faster tracks a handful of new checks rather than the market. The previous
  coefficients are logged beside the new ones so thrashing is visible.

### Censoring (the label fix)

A fill rate is measured between two checks. When the later one reads exactly
100%, the bin filled at SOME point in that window — the observed rate is a
**lower bound**, not a measurement. And the censoring is not random: on the live
fleet 165 of 795 intervals (20.8%) are censored, and their median observed rate
is **13.3 %/day against 3.99 for uncensored ones**. The fastest sites are the
ones being under-measured.

Deleting them was tried in 2026-07 and made things worse (rho +0.167 → +0.090),
because a bin found full is the strongest evidence a site is good. The correct
treatment is censored (Tobit) regression, implemented in
`internal/placementfit/tobit.go`: uncensored rows contribute the normal density,
censored rows the survival function, maximised by Nelder-Mead.

Two things this required getting right:

- **Interval-level, not bin-level.** 95 of 106 bins have at least one censored
  interval, so a per-bin flag would mark almost everything.
- **Evaluated on uncensored intervals only.** A Tobit model predicts the LATENT
  rate, so scoring it against truncated observations would penalise it for being
  correct. `EvaluateOnUncensored` is the fair yardstick for both estimators.
- **Significance counted in BINS, not intervals** (`EffectiveN`). Intervals from
  one bin share a site and are not independent draws.

**Current status: computed every refit, NOT adopted.** On the live fleet the
censored fit beats OLS (MAE 3.715 vs 4.094 on 596 uncensored intervals) but
loses to the median baseline (3.632). Adopting "the better of two models that
are both worse than guessing" would be a regression dressed as an improvement,
so the guardrail requires beating BOTH. It will adopt itself automatically if
that changes as the fleet grows.

That result is a statement about the label, not about censoring: interval-level
fill rate is noisy enough that neither estimator beats the median on it.

First production fit (2026-08-14, n=78):

```
C=9.580  density^0.387  anchor^0.115  pop^0.109
LOO MAE 2.119 vs baseline 2.247   rho +0.295   → promoted
```

**On the collapsed anchor tiering — MEASURED, do not "fix" it.**

`anchorScoreFor` can only use the density scan's detection (0.7 / 0.15), because
a bin has no candidate business name to tier on the way a candidate does. This
looks like it should bias the fitted anchor exponent downward. It was tested on
the same 78 bins, fitting full tiering against the collapsed version:

| | anchor exponent | LOO MAE | rank rho |
|---|---|---|---|
| full tiering (1.0 / 0.7 / 0.15) | +0.1094 | 2.142 | +0.271 |
| collapsed (0.7 / 0.15) | +0.1137 | 2.139 | +0.273 |

The difference is **0.004**, and the collapsed version is marginally better.
Sharing the full tiering with the refit would be work for nothing.

The reason is that the tiers do not separate on outcome at all:

| tier | n | median fill %/day |
|---|---|---|
| tier 1 (Target, Costco, Home Depot…) | 15 | 8.12 |
| tier 2 (CVS, Walgreens, Ross…) | 21 | 8.06 |
| no anchor | 42 | 6.05 |

**Anchor vs no-anchor is real; tier 1 vs tier 2 is not.** A national big-box
predicts the same fill as a drugstore. That means the 1.0/0.7 split in the LIVE
candidate scorer is currently unearned.

Not acted on: n=15 and n=21, and 8.12 vs 8.06 is inside noise in either
direction. **Recheck at ~150 bins** — if it still holds, collapse the candidate
anchor term to binary and delete the tier lists.

## Open items

- **`placement_decisions` has 0 rows.** The INSERT was tested against production
  in a rolled-back transaction on 2026-08-14 and **works** — so this is not the
  silent-failure bug it was in the past. The recommender simply has not run in
  prod since logging shipped on 2026-07-30. The refit does not depend on it
  (see above), but selection-bias correction later will.
- **`PLACEMENT_LOG_DISABLED=1`** exists so benchmark runs cannot pollute that
  table with rows describing a scoring variant that was never shipped. Unset in
  every deployed environment.
- **A larger label is unused.** `potential_locations` holds 75 converted vs 191
  not — 266 labeled rows against 79 fill-rate rows. Different question ("would an
  operator site here" rather than "does it yield") so it inherits team bias, but
  it is 3.4× the data.
- **`ai_recommendations` is effectively unused** — 191 pending, 3 accepted, 3
  dismissed. Not a viable label source.
- **`ropacal-placement`** (54 Python modules: GLM, GBM, ranker, causal, bandit,
  MCLP, judge) is built and deliberately NOT deployed. At n=79 the simple GLM beat
  the GBM. Re-run `signalcheck` around 250–300 bins.
- Worth trying when the label improves: **TabPFN**. Tabular foundation models now
  lead gradient boosting specifically at small n, which is this exact regime, and
  it runs on CPU at this scale.

## If you want to improve this

In order of leverage, and none of it needs an agent:

1. **Fix the label.** Fill rate over a ~4-day median interval with 17% of
   readings pinned at 100% is a noisy target. Censored/Tobit regression, not
   dropping rows — deleting censored observations was tried and made it worse
   (+0.167 → +0.090), because a bin found full is the strongest evidence the site
   is good.
2. **Replace the gap cliff with a distance decay** (the Huff idea).
3. **Drop or rebuild the fill term** so it stops feeding on its own decisions.
4. **Then** consider a better model class.

Chasing more site features is the thing that has been tried most and returned
least.
