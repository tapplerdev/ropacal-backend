-- +goose Up
-- +goose StatementBegin

-- Which country an organization operates in, ISO 3166-1 alpha-2.
--
-- WHY THIS EXISTS: anchor-chain detection matches POI names against a list of
-- retail chains, and that list was US-only. Merging Canadian chains into one
-- global list looked safe — chain names seemed geographically distinctive — but
-- a live Toronto run disproved it immediately: "lucky", present for the
-- California supermarket, matched SIX unrelated Toronto corner shops as whole
-- words (Lucky Convenience, Lucky Lotto Centre, Lucky Dollar Food Centre...),
-- each scoring a tier-2 anchor. The collision class is systemic, not a one-off:
-- winners, staples, target, ross and metro are all ordinary English words.
--
-- Scoping the chain list by country removes the entire class. A Canadian org
-- never evaluates "lucky"; a US org never evaluates "metro".
--
-- Explicit column rather than deriving country from warehouse coordinates: it is
-- a stable fact about the business, set once at provisioning, and cannot flip
-- silently because someone placed a bin near a border.
ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS country TEXT NOT NULL DEFAULT 'US';

-- Every organization predating this column is Ropacal (Bay Area) or a test org.
UPDATE organizations SET country = 'US' WHERE country IS NULL OR country = '';

ALTER TABLE organizations
    DROP CONSTRAINT IF EXISTS organizations_country_check;
ALTER TABLE organizations
    ADD CONSTRAINT organizations_country_check
    CHECK (country ~ '^[A-Z]{2}$');

COMMENT ON COLUMN organizations.country IS
    'ISO 3166-1 alpha-2. Selects the anchor-chain list in the placement recommender.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_country_check;
ALTER TABLE organizations DROP COLUMN IF EXISTS country;
-- +goose StatementEnd
