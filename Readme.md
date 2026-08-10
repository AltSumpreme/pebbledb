# PebbleDB

PebbleDB is an experimental distributed SQL database written in Go. Its public
interface is PostgreSQL-compatible SQL; internally it layers a typed relational
engine and serializable MVCC over range routing, Raft replication, and an
ordered LSM store.

> [!WARNING]
>
> PebbleDB is a learning and research implementation. The milestone foundations
> are implemented and tested, but this repository is not ready for production
> data or compatibility-sensitive PostgreSQL workloads.

## Implemented foundations

- PostgreSQL v3 simple and extended query protocols, cancellation, TLS, and
  trust, TLS-protected password, or SCRAM-SHA-256 authentication.
- Typed SQL parsing, binding, planning, execution, catalog metadata, joins,
  aggregates, DDL/DML, secondary indexes, and `EXPLAIN`.
- Serializable MVCC transactions with snapshots, conflict detection, and atomic
  durable batches.
- Persistent key ranges, Raft replication, distributed read execution, and
  automatic placement decisions.
- Checksummed WAL/SSTable LSM storage with Bloom filters, a bounded block cache,
  background compaction, online checkpoints, directory locking, and an explicit
  storage-format marker.
- Durable two-phase commit decision logging and crash recovery primitives for
  multi-range mutation integration.
- Durable rolling binary-upgrade coordination with per-node compatibility
  ranges, readiness acknowledgments, monotonic activation, restart recovery,
  and fail-closed rejection of old binaries.
- Seeded chaos/recovery coverage, health/status/metrics endpoints, benchmarks,
  race testing, and release verification targets.

See [docs/architecture.md](docs/architecture.md) for layer boundaries, milestone
details, and the remaining limitations.

## Run the PostgreSQL endpoint

Go 1.20 or newer is required.

```bash
go run ./cmd/pebbledb-server \
  -data ./pebbledb-data \
  -listen 127.0.0.1:5432 \
  -admin 127.0.0.1:8080
```

Then connect with a PostgreSQL client:

```bash
psql -h 127.0.0.1 -p 5432 -U pebbledb
```

For password authentication, TLS is required. Supply a PEM certificate/key
pair together:

```bash
go run ./cmd/pebbledb-server \
  -data ./pebbledb-data \
  -auth password -user pebbledb -password change-me \
  -tls-cert ./server.crt -tls-key ./server.key
```

SCRAM-SHA-256 avoids retaining the plaintext password in the running server
configuration and can also load a PostgreSQL-format verifier:

```bash
go run ./cmd/pebbledb-server \
  -data ./pebbledb-data \
  -auth scram -user pebbledb -password change-me \
  -tls-cert ./server.crt -tls-key ./server.key

# Or replace -password with:
# -scram-verifier 'SCRAM-SHA-256$4096:<salt>$<stored-key>:<server-key>'
```

The admin listener provides `/healthz`, `/status`, and `/metrics`.

## Rolling binary upgrades

Cluster behavior is controlled by a durable active version. The safe sequence
is: roll every node to a binary supporting both versions, register those binary
ranges, call `BeginUpgrade`, acknowledge the target from every node, and call
`FinalizeUpgrade`. Until finalization, behavior remains on the old active
version and the activation can be aborted. After finalization, a binary that
does not support the active version is rejected during engine startup.

The engine exposes `RegisterUpgradeNode`, `BeginUpgrade`,
`AcknowledgeUpgrade`, `FinalizeUpgrade`, and `AbortUpgrade`; current state is in
the admin status response and `pebbledb_cluster_version_*` metrics.

## SQL example

```sql
CREATE TABLE users (
    id BIGINT PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT UNIQUE,
    age INT
);

INSERT INTO users VALUES (1, 'Reuben', 'reuben@example.com', 21);
CREATE INDEX users_age_idx ON users(age);

BEGIN;
UPDATE users SET age = age + 1 WHERE id = 1;
SELECT name, age FROM users WHERE id = 1;
COMMIT;
```

## Verify a release candidate

```bash
make verify
```

This builds all packages, runs normal and race-enabled tests, runs `go vet`, and
executes one iteration of every storage/SQL benchmark. Individual targets are
`build`, `test`, `race`, `vet`, and `benchmark-smoke`.

## Important limitations

- The SQL dialect and PostgreSQL catalogs/types are a subset, not drop-in
  PostgreSQL compatibility.
- Certificate rotation and automated restore orchestration are not implemented.
- The durable distributed-commit component is not yet wired into the SQL range
  mutation path, so cross-range SQL writes remain fail-closed.
- Placement actions expose an orchestration boundary; production-grade node
  lifecycle and Raft membership automation remain external.
