-- Fails while any object of 2 GiB or more is recorded, since INTEGER cannot
-- hold its size.
ALTER TABLE lfs_objects ALTER COLUMN size TYPE INTEGER;
