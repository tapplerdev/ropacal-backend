-- Fix bin status values to be lowercase
UPDATE bins SET status = LOWER(status);
