-- Add placement_source to route_tasks to distinguish between:
--   "potential_location" → create a brand new bin (existing behaviour, default)
--   "warehouse"          → redeploy an existing in_storage bin to a new field address
--
-- All existing rows default to 'potential_location' so nothing breaks backward.
ALTER TABLE route_tasks ADD COLUMN placement_source TEXT DEFAULT 'potential_location';
