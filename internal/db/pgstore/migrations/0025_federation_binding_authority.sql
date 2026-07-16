-- Preserve the actor and transport policy attached to a federation binding.
-- IF NOT EXISTS keeps fresh installations, whose canonical baseline already
-- includes these columns, on the same migration path as upgraded databases.

ALTER TABLE federation_bindings
  ADD COLUMN IF NOT EXISTS bound_actor TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS allow_insecure INTEGER NOT NULL DEFAULT 0
    CHECK(allow_insecure IN (0,1));

-- Version-24 bindings predate actor binding. Leaving push enabled would emit
-- events under arbitrary local actors that an actor-bound peer must reject.
UPDATE federation_bindings
   SET push_enabled = 0
 WHERE push_enabled = 1
   AND bound_actor = '';
