# extend

`maya-stall extend` adds time to an unexpired Kept Session.

```sh
maya-stall extend --by 30m <run-id>
maya-stall extend --control-plane https://maya-stall.example.com --by 30m <run-id>
```

`--by` is required and accepts a positive Go duration. The extension starts
from the current keep deadline, not the request time. It cannot move the
deadline beyond the Host Lock hard lifetime. An expired session, a non-kept
Run, or an over-policy extension fails without changing the deadline.

Configured mode is an authenticated explicit operator action. The bearer token
defaults to `MAYA_STALL_CONTROL_PLANE_TOKEN`;
`--control-plane-token-env <name>` selects another environment variable.
Embedded mode requires local access to the retained Run State. Both modes
persist the new deadline and an extension event.

Use [`status`](status.md) to inspect `keepDeadline`, `keepRemaining`, and the
Host Lock hard deadline before extending.
