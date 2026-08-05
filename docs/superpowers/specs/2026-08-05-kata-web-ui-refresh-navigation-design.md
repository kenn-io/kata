# Kata Web UI Refresh and Navigation Design

## Scope

Close two authority-consistency gaps in the first-class web UI:

- cancel a pending keyboard issue selection as soon as another navigation begins;
- retry an event-triggered snapshot refresh after transient failure.

This work does not change routes, persisted state, authentication, authorization, or snapshot contracts.

## Navigation ownership

`AppShell` owns the navigation generation because it owns every navigation initiated by the sidebar, filters, issue collection, and graph controls. A single navigation wrapper increments the generation synchronously before delegating to the existing route callback. `AppShell` passes that generation to `IssueCollection`, whose existing cancellation effect clears any pending keyboard selection when the value changes.

The generation is an in-memory cancellation signal only. It is not serialized into routes and does not delay navigation.

## Event refresh retry

`InvalidationController` treats a false return or exception from an event-driven refresh as a transient failure unless the controller was paused or stopped. It retains dirty state and schedules another attempt with exponential backoff beginning at one second and capped at thirty seconds. A successful refresh resets the delay.

Reset events keep their stronger contract: the full-refresh latch remains set until an unconditional refresh succeeds. Authentication pauses continue to suppress timers, and reauthentication resumes without losing the reset latch.

## Verification

Component integration coverage will hold a keyboard navigation key, begin sidebar navigation, release the key, and verify that the stale issue selection is not emitted. Controller coverage will fail an ordinary event refresh, verify the delayed retry, then succeed and verify that later failures restart at the initial delay. Existing reset-latch tests must continue to pass unchanged.
