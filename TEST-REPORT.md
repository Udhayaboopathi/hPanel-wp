# Test Report — audit remediation (31 Oct 2026)

Environment: this build VM (Linux, Go 1.23.12). No Docker daemon and no
server access exist here — by the project's standing rule the production
server is never touched directly — so Docker behaviour is tested against
fake Docker daemons on real Unix sockets, and live verification is the
operator's post-deploy Repair/Restart run (see DEPLOYMENT-REPORT.md).

## Commands and results

| Command | Result |
|---|---|
| `go build ./...` | PASS |
| `go vet ./...` | PASS (no findings) |
| `go test ./...` | PASS — all 13 test packages, 0 failures |

## Regression tests for the brief's scenarios (new)

`internal/docker/docker_find_test.go` (label resolution) and
`cmd/agent/lifecycle_test.go` (agent lifecycle against a recording fake
daemon that honours label filters and records every mutating call):

| Scenario | Test | Result |
|---|---|---|
| A. Container exists with expected name | `TestLifecycleRestartExpectedName`, `TestFindBySiteIDResolvesByLabel/expected name` | PASS — restarted, correct ID returned |
| B. Exists under a different name, correct labels | `TestLifecycleRestartRenamedContainerStaleMetadata`, `…/renamed container` | PASS — resolved by label, restarted, fresh ID reported for reconciliation |
| C. Recreated with a new container ID | `…/recreated new id` | PASS |
| D. Container exists but stopped | `TestLifecycleRestartStoppedStarts`, `TestFindBySiteIDStopped` | PASS — restart starts it; exactly one `start` call, no `restart` |
| E. Genuinely missing, DB/files intact | `TestLifecycleMissingContainerCategorised` | PASS — error category `not_found`, message names the label-based lookup and the Repair path, zero mutating calls |
| F. Belongs to another site | `TestLifecycleNameSquatterGuarded`, `TestLifecycleActsOnlyOnOwnContainer`, `TestFindBySiteIDOtherSite` | PASS — guard category, the foreign container is never touched; with both present only the site's own container is restarted |
| G. Stale panel metadata (wrong stored name/ID) | `TestLifecycleRestartRenamedContainerStaleMetadata` | PASS — labels win over the stale record |
| H. Docker daemon unavailable | `TestLifecycleDaemonDownIsNotNotFound` | PASS — transport error, deliberately NOT categorised `not_found` (so the panel's bounded retry applies) |

Supporting tests: idempotent start/stop no-ops (`TestLifecycleIdempotentNoOps`),
helper-container exclusion (`TestFindBySiteIDExcludesEphemeralHelpers`),
managed-label requirement (`TestFindBySiteIDRequiresManagedLabel`),
ambiguity (`TestFindBySiteIDAmbiguous`), repair refuses to invent data when
files are gone (`TestRepairAbortsWhenFilesMissing` → category
`data_missing`, no mutating calls), repair no-op on a healthy site
(`TestRepairNoOpWhenContainerExists`).

## Other new/updated tests

- `internal/security/yamlpolicy_test.go` — fixture now mirrors the real
  generated compose; 17 rejection cases including the new classes
  (namespace sharing, named volumes, forged site-id, `seccomp=unconfined`,
  build sections, foreign network joins, renamed networks) plus Traefik
  host pinning (`TestPolicyPinsTraefikHosts`).
- `cmd/agent` `TestMariaDBArgvUsesAdminUserNotRoot` — updated: asserts the
  password is absent from argv entirely.
- `cmd/panel` `TestOtherTemplatesRender` — render smoke tests for the
  create wizard (input rebinding), websites list (sort retention, `missing`
  filter), audit page (pagination guard) and login (pre-auth CSRF field);
  `TestDetailTemplateRenders` continues to cover the simplified website
  page including the Repair card.
- All pre-existing suites (store, jobs, provisioning, configgen, docker
  pull, panel detail, agent validation) pass unchanged except the two
  intentional updates above.

## Not covered here (stated honestly)

- Repair against the *real* MariaDB + Docker on the production host: the
  DB-existence probe and the full recreate run execute only in production.
  The post-deploy operator verification (DEPLOYMENT-REPORT.md) is the
  live test: Repair Website on wp-1c46776b, then Restart on the existing
  container.
- The backup → restore chain with the new checksum gate is covered by
  code review and the agent-side verify logic (`backups.Verify` is the
  same routine used at backup creation); a live restore drill remains an
  operator-side exercise.
