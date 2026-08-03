-- +goose Up
-- +goose StatementBegin

-- Drop six tables that no code in any repo reads or writes.
--
-- They were removed from PRODUCTION by hand on 2026-08-02, immediately after
-- the boot-time inline DDL was deleted from database.go (Track A3). Before that
-- they could not be safely dropped: `database.Migrate()` ran CREATE TABLE IF
-- NOT EXISTS on every boot, so a drop would have been undone on the next
-- deploy — and recreated WITHOUT organization_id or RLS, turning protected
-- empty tables into unprotected ones. That is exactly what happened to
-- shift_bins in Feb 2026.
--
-- This migration exists so the chain records what production already reflects.
-- Against prod it is a no-op (IF EXISTS). Against a FRESH database it matters:
-- 00001 is a pg_dump baseline that still contains their CREATE statements, so
-- without this a new environment would come up carrying all six again.
-- Rewriting 00001 would be worse — a historical migration whose checksum no
-- longer matches what already ran everywhere.
--
-- What they were, and why each is safe to lose:
--   shift_bins           EMPTY. Superseded by route_tasks, which carries the
--                        same shift/bin/sequence relationship plus task_type.
--   zone_risk_overrides  EMPTY. Never wired to a handler.
--   zz_backup_*  (x4)    Manual snapshots taken 2026-07-10 during the overnight
--                        shift correction. 5-14 rows each, and they are copies
--                        of bins/route_tasks, both of which still exist.
--
-- All six were dumped to CSV before dropping. Verified zero Go references at
-- the time of removal.
--
-- DELIBERATELY NOT DROPPED: the two driver GPS archives. They are SEQUENTIAL,
-- not duplicates — together they are the movement record from Feb 2025 to
-- Jun 2026, with a ~2-month gap:
--
--   driver_locations         6,244 rows,  2 drivers,  2025-02-04 -> 2025-12-09
--   driver_location_history 94,647 rows,  8 drivers,  2026-02-05 -> 2026-06-01
--
-- MIND THE UNITS. driver_location_history.recorded_at is TIMESTAMPTZ. But
-- driver_locations.timestamp is BIGINT MILLISECONDS, with 8 of its 6,244 rows
-- in SECONDS (all 2025-02-04, the changeover). Passing it straight to
-- to_timestamp() dates 99.87% of the rows to the year 57910. Use:
--     CASE WHEN timestamp >= 1e11 THEN timestamp/1000 ELSE timestamp END
--
-- WHY THEY STAY: this is the ONLY long-term record of driver movement. The live
-- tables keep none — driver_current_location is one upserted row per driver,
-- driver_location_snapshots is pruned to 3 hours (stale_shift_monitor.go) — so
-- once these are gone that window is unreconstructable. ~45 MB total. Keeping
-- them was a considered decision, not an oversight; do not "finish the job".

DROP TABLE IF EXISTS public.shift_bins;
DROP TABLE IF EXISTS public.zone_risk_overrides;
DROP TABLE IF EXISTS public.zz_backup_overnight_20260710_bins;
DROP TABLE IF EXISTS public.zz_backup_overnight_20260710_route_tasks;
DROP TABLE IF EXISTS public.zz_backup_overnight2_20260710_bins;
DROP TABLE IF EXISTS public.zz_backup_overnight2_20260710_route_tasks;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Intentionally not reversible. These tables held no data worth restoring
-- (four were empty, two were 5-14 row snapshots of tables that still exist),
-- and recreating them without their original RLS would reintroduce the exact
-- untenanted-table hazard this cleanup removed. The CSV dumps are the record.
SELECT 1;

-- +goose StatementEnd
