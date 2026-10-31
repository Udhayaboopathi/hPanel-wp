# Root-Cause Analysis — `restart_site` failure for wp-1c46776b

Incident: job `job_ecf878b5ee837823` (12:30 UTC) and job `job_dcf1284714bcd5e1`
(12:38 UTC), kind `restart_site`, site `wp-1c46776b`, both failed with
`container "own-hpanel-wp-1c46776b" not found`, displayed progress 100%,
attempts 1/3, and a Retry button that could not fix anything.

## Timeline (all 2026-10-31, UTC)

| Time | Event |
|---|---|
| 12:21–12:22 | `create_site` job `job_3be6695535d93718` runs the full chain — validation, network, **database + least-privilege user created**, storage/.env/compose written — and fails at container creation: Docker had never pulled `wordpress:6.7.2-php8.3-apache` and container *create* does not pull. (Fixed separately in `8c4a2ba`: the agent now ensures the image before creating.) |
| 12:30 | `restart_site` → `container "own-hpanel-wp-1c46776b" not found`. |
| 12:38 | `restart_site` again → same error. |
| ~13:10 | Operator-supplied production evidence (below) proves the container does not exist in any state. |

## Production evidence (operator-provided, read-only)

A full `docker ps` on the production host at ~13:10 UTC shows:

- The hPanel stack healthy: `own-hpanel-agent`, `own-hpanel` (healthy),
  `wordpress-mariadb` (healthy, 38 min up).
- **All 10 protected production containers Up/healthy with unchanged IDs**
  (`krweb-ui`, `redberyl-api`, `redberyl-ui`, `redberyl-rustfs`,
  `redberyl-postgres`, `construction-api`, `construction-ui`,
  `construction-mariadb`, `redberyl-postgres-proxy`, `redberyl-traefik`).
- **No `own-hpanel-wp-1c46776b` container at all.** The agent's resolver used
  `ListContainers(all=true)`, which includes stopped containers, so the two
  "not found" results plus this listing establish that the container was
  **never successfully created** — it was not deleted, renamed, or hidden.

Conclusion: the site existed only as panel metadata + its MariaDB database
(`wp_1c46776b_db`) + its files on disk. The restarts were attempts to revive a
site whose provisioning had never completed.

## Why the failure became a dead end (the actual defects)

The missing container was only the trigger. Five code defects turned a
recoverable situation into an unexplained dead end:

- **D1 — Name-only container identity.** `cmd/agent` `lifecycle()` resolved
  the container exclusively by the exact constructed name
  (`FindByName(s.ContainerName)`). Labels (`own-hpanel.managed`,
  `own-hpanel.site-id`) were checked *after* a name match but never used for
  resolution. Any recreation under a different name/ID would have produced
  the same false "not found", and the error carried no category, no state
  distinction, and no recovery guidance.
- **D2 — Failed jobs displayed 100% progress.** The provisioning runner
  wrote `UpdateJob(jobID, "failed", 100, …)` on failure paths, so a job that
  failed at its first step claimed full progress.
- **D3 — No identity reconciliation.** Successful start/stop/restart
  operations discarded the resolved container ID; the panel record could
  silently go stale after any recreation.
- **D4 — Retry was a placebo.** `Runner.Retry()` merely re-marked the job
  `queued` with the message "re-run the operation from the website page" —
  nothing ever executed it again. The job sat queued forever.
- **D5 — Attempt limits were decorative.** `MaxAttempts` (3) existed in the
  schema but no automatic retry logic existed for transient failures, and
  no distinction was made between retryable (transport) and non-retryable
  (operational) failures.

## Hypotheses from the audit brief, evaluated

| # | Hypothesis | Verdict |
|---|---|---|
| 1 | Container genuinely missing | **CONFIRMED — primary cause** (create failed before creation; production `docker ps` evidence). |
| 2 | Container name mismatch | Not this incident (nothing existed to mismatch), but the name-only lookup made it a real fragility — fixed by label-based resolution. |
| 3 | Container ID mismatch | Same as 2 — possible in principle, unprovable here; resolution no longer depends on the stored ID. |
| 4 | Container recreated outside the panel | No evidence; production listing shows no site container at all. |
| 5 | Stale panel metadata | Partially: the website row claimed a container name that never existed. Lifecycle ops now reconcile the real ID on every success, and not-found sets the site status to `missing`. |
| 6 | Wrong lookup key (name vs label/ID) | **CONFIRMED as a design defect** (D1). |
| 7 | Docker socket / daemon issue | Ruled out: the agent answered other calls; the create job reached the daemon and got a precise image error. |
| 8 | Database record inconsistency | The DB row existed without a container — an inconsistency created by D4/D2 hiding the original failure, not by data corruption. |
| 9 | Restart implemented as stop-then-create | Ruled out: lifecycle never created containers. |
| 10 | Job targets a deleted resource | Ruled out: nothing was deleted; creation never completed. |
| 11 | Permissions / ownership guard | Ruled out for this incident (no container to guard); the guard itself had a stub site-id check (security finding L11), now implemented. |
| 12 | Weak container↔website identity model | **CONFIRMED** — identity was a mutable name string. Now: labels are identity, name is a hint, ID is reconciled state. |
| 13 | Job lifecycle bug | **CONFIRMED** (D2, D4, D5). |
| 14 | Suppressed provisioning error | **CONFIRMED in effect** — the original create failure was visible in its job, but the restart errors gave no link back to it and no recovery path. |

## Root cause statement

The container was never created because the original `create_site` job
failed at image-pull time (fixed in `8c4a2ba`). Every layer that should
have surfaced that fact and offered a recovery path was defective:
name-only container resolution with an uncategorised bare error (D1),
failure displayed as 100% progress (D2), no container-ID reconciliation
(D3), a Retry button that queued a job nothing would ever run (D4), and
attempt limits with no retry engine behind them (D5). The repair is
therefore not "make restart find the container" — it is a label-based
identity model, truthful job lifecycle, and a dedicated **Repair Website**
operation that verifies the site's database and files still exist and
recreates only the container against them.
