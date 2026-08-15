-- Fitted placement-model coefficients.
--
-- The site score is a Cobb-Douglas index, which is linear in logs, so its
-- exponents are estimable by ordinary regression rather than being hand-chosen
-- constants. This table is where a fit lands so Go can read it at runtime and
-- the weights can move as the fleet grows.
--
-- ONE ROW PER FIT, never updated in place. A fit is a historical claim about
-- what the data said on a given day; overwriting it would destroy the ability
-- to see whether successive fits are converging or thrashing, which is the main
-- thing worth watching at small sample sizes.
--
-- `is_active` selects the one Go uses. Exactly one active row per organization
-- is enforced by a partial unique index rather than by convention.
--
-- Deliberately NOT the same thing as `placement_decisions`: that table records
-- individual decisions for selection-bias work later. This records the model
-- those decisions were scored with.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS placement_model (
    id                TEXT PRIMARY KEY,
    organization_id   TEXT NOT NULL,
    fitted_at         BIGINT NOT NULL,

    -- Cobb-Douglas terms. Stored as named columns rather than JSON because Go
    -- reads them on the scoring path and a typo in a JSON key would surface as
    -- a silently-zero exponent instead of a compile or scan error.
    coef_constant     DOUBLE PRECISION NOT NULL,
    coef_density      DOUBLE PRECISION NOT NULL,
    coef_anchor       DOUBLE PRECISION NOT NULL,
    coef_pop          DOUBLE PRECISION NOT NULL,

    -- What the fit was earned on, so a future reader can judge whether it is
    -- still credible instead of assuming.
    n_bins            INTEGER NOT NULL,
    mae               DOUBLE PRECISION,   -- leave-one-out, %/day
    mae_baseline      DOUBLE PRECISION,   -- same, for "predict the median"
    rank_rho          DOUBLE PRECISION,   -- out-of-sample Spearman

    is_active         BOOLEAN NOT NULL DEFAULT FALSE,
    -- Why this fit was or was not promoted. Populated even for rejected fits:
    -- a refit that failed its guardrail is evidence, and discarding it would
    -- hide a model that is degrading.
    note              TEXT,

    created_at        BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_placement_model_org_fitted
    ON placement_model (organization_id, fitted_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_placement_model_one_active
    ON placement_model (organization_id) WHERE is_active;

ALTER TABLE placement_model ENABLE ROW LEVEL SECURITY;
ALTER TABLE placement_model FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS placement_model_tenant ON placement_model;
CREATE POLICY placement_model_tenant ON placement_model
    USING (organization_id = NULLIF(current_setting('app.org_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('app.org_id', true), ''));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS placement_model;

-- +goose StatementEnd
