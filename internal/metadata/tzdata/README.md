# Schedule timezone bundles

`zoneinfo-2026c.zip` is the Go 1.27.3 `lib/time/zoneinfo.zip`; its SHA-256 is `b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779`. Go's `lib/time/update.bash` identifies its IANA data and code releases as 2026c.

To update the write version, use a complete Go `lib/time/zoneinfo.zip` built from the desired IANA release. Record the Go and IANA versions and archive digest here, add the archive and its digest to `schedule_zone.go`, and select the new digest as `ScheduleTZDataVersion`. Before selecting a new write digest, extend the federation capability contract so peers advertise the bundles they can read. Normal feeds, relay delivery, and signed checkpoints must refuse publication of descriptors whose digest the peer does not support. `issue_schedule_v1` covers only the first bundle. Keep all published archives and map entries: existing schedules name their exact digest. Update the bundle and retained-rules tests to cover both old and new rules. Go's `lib/time/update.bash` documents how to build an archive from IANA sources in a separate Go source checkout.

The test fixture in `../testdata` contains only America/Inuvik and is not a complete write bundle. Its README records its IANA source version.
