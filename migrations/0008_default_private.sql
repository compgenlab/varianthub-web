-- New sources default to private.
--
-- Default closed: a source registered without anyone stating who may see it
-- should not be readable by everyone, and the cost of the two policies is
-- asymmetric -- publishing something private is a disclosure, while a private
-- source nobody can see is a support request.
--
-- Existing rows keep the visibility they were registered with. Flipping them
-- would revoke access that people are currently relying on, silently.
ALTER TABLE source ALTER COLUMN visibility SET DEFAULT 'private';
