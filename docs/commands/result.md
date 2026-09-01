# result

`maya-stall result` reads one run's Scenario Result, Validators, lifecycle, and
cleanup truth from Embedded Mode or a Configured Control Plane.

```sh
maya-stall result <run-id>
maya-stall result --json <run-id>
maya-stall result --control-plane https://maya-stall.example.com --json <run-id>
```

JSON is a versioned object with `kind: result`, Run ID, lifecycle `state`,
Scenario `status`, `cleanupState`, `final`, and `success`. Success is true only
when the Scenario passed, evidence is readable, lifecycle state is `completed`,
and cleanup completed. Kept and cleanup-failed runs are never successful.
While execution or evidence finalization is still active, the same contract
returns `final: false`, `success: false`, and `cleanupState: pending`.

Terminal `run --json` and `evidence collect --json` records also include a
versioned `report` projection. It is the typed source for the stored HTML
report's verdict, counts, failure category, lifecycle, cleanup, missing evidence,
and next command; consumers do not need to scrape terminal or HTML text.
