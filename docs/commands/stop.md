# stop

`maya-stall stop` stops a kept run and releases its Host Lock.

```sh
maya-stall stop <run-id>
maya-stall stop --control-plane https://maya-stall.example.com <run-id>
```

Use it after debugging a kept session from `--keep-on-failure` or
`--stop-after never`.

Stop is authoritative cleanup. For broker-backed runs, it asks the Session
Broker to stop the retained Maya UI Session, removes the retained remote run
workspace when supported, then removes local run state under
`.maya-stall/state/` and makes the Maya Host selectable for the next Fresh Run.

If the broker stop or cleanup operation fails, local run state and the Host Lock
remain in place so the command does not lie about cleanup.

Configured `stop` cancels a queued Run or explicitly releases a Kept Session.
Queue cancellation durably records `run.canceled`, produces terminal status and
minimal Evidence, and never acquires or releases a Host Lock. Kept-session stop
records an authorized stop event and directs the owning Agent to verify and
stop the exact retained Broker session before cleanup and Host Lock release.
Its successful response is `stop-requested: <run-id>` because cleanup remains
pending until that Agent completes it; `status` confirms terminal cleanup.
Other assigned active Runs are rejected. Authentication uses the same Control
Plane bearer-token options as `run` and `status`.
