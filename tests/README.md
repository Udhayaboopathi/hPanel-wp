# Tests

Go tests live beside the code they cover (`*_test.go` in each package):

```bash
go test ./...     # 29 test functions across 11 packages, all passing
go vet ./...
```

Docker-dependent integration tests are build-tagged and excluded from the
default run:

```bash
go test -tags integration ./internal/docker/
```

They require a real Docker daemon and were not executed in the development
VM (no Docker available there). See `../REPORT.md` for the full, honest
verification record, including the local panel/agent smoke tests that were run.
