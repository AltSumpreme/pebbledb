# PebbleDB architecture and build plan

PebbleDB's public model is relational. The ordered key-value layer is an internal
implementation detail used to encode catalog records, table rows, indexes, and
MVCC versions.

```text
PostgreSQL client
      |
PostgreSQL wire protocol
      |
SQL parser -> AST -> binder -> logical plan -> optimizer -> physical plan
      |
local/distributed executor
      |
transaction manager (MVCC and serializable validation)
      |
range router -> replicated range/Raft
      |
ordered LSM storage
```

The layers are intentionally separated. In particular, SQL code must not know
about WAL records or SSTable files, and the LSM must not know what a table or row
is.

## Current repository state

The original implementation has a small SQL-like parser, an in-memory relational
model, a tuple pager, and persistence that rewrites table page files. That path is
still active so existing commands and tests remain usable while the new engine is
built beside it.

Milestone 1A introduces `storage/lsm`, an internal, concurrent ordered store with:

- a checksummed, synchronously persisted write-ahead log;
- WAL replay and safe removal of an incomplete trailing record;
- a mutable memtable with configurable automatic flush;
- immutable, checksummed, bytewise-sorted SSTables;
- newest-write-wins reads across the memtable and multiple SSTables;
- durable tombstones and inclusive-start/exclusive-end ordered scans;
- full-table compaction with crash-safe tombstone retention;
- explicit flush, close, diagnostics, and corruption reporting.

This first slice deliberately does not yet replace the old table snapshots. The
replacement happens only after catalog and row encodings exist, avoiding a format
with no schema-evolution story.

## Internal keyspace

Keys are binary and bytewise ordered. Human-readable examples use slashes only to
show their logical structure:

```text
/system/database/<database-id>
/system/schema/<schema-id>
/system/table/<table-id>
/system/column/<table-id>/<column-id>
/system/index/<index-id>
/table/<table-id>/primary/<encoded-primary-key>/<mvcc-timestamp>
/table/<table-id>/index/<index-id>/<encoded-index-key>/<primary-key>
```

The actual codec will use stable type tags, order-preserving integer encodings,
length-delimited strings, and a version byte. User-provided names will never be
concatenated directly into storage keys.

## Incremental milestones

Each milestone must keep `go test ./...` passing and must have restart/corruption
tests for any persistent format it introduces.

1. **LSM foundation**
   - 1A (implemented): WAL, memtable, SSTables, ordered scans, basic compaction.
   - 1B: sparse indexes, Bloom filters, block cache, background flush, leveled
     compaction, file manifest, directory locking, metrics, and fault injection.
2. **Typed relational model and codecs (implemented)**
   - SQL `NULL`, `BOOL`, `INT`, `BIGINT`, `TEXT`, bounded exact `DECIMAL`, and
     immutable typed values.
   - Versioned order-preserving composite keys, deterministic rows keyed by
     stable column IDs, and versioned table schemas with compatibility,
     round-trip, ordering, LSM integration, and corruption tests.
3. **Catalog (implemented)**
   - Checksummed, versioned database, schema, and atomic table descriptors over
     the LSM; table records include columns, indexes, constraints, and partitions.
   - Idempotent default bootstrap, normalized name resolution, sorted listings,
     complete table description, dependency-safe drops, revision tracking,
     concurrent DDL serialization, and restart/corruption coverage.
4. **SQL frontend (implemented)**
   - Source-positioned lexer, independent typed AST, precedence parser, standard
     DDL/DML forms, expressions, functions, multi-statement input, comments,
     SQL string escaping, and transaction statements.
   - Catalog-backed binder resolves qualified tables and columns, expands stars,
     validates predicates/functions/assignments, and contextually coerces typed
     INSERT, UPDATE, comparison, and exact-decimal literals.
5. **Planning and local execution (implemented)**
   - LSM-backed relational row store plus explicit logical/physical plans and
     primary-key-ordered scan, filter, project, limit, sort, global aggregate,
     nested-loop inner join, insert, update, delete, and catalog/DDL operators.
   - End-to-end SQL engine, typed expression evaluation with SQL NULL logic,
     restart persistence, multi-row validation, and `EXPLAIN` output that exposes
     operators/access paths without leaking WAL or SSTable internals.
6. **Indexes (implemented)**
   - Ordered primary lookups and maintained unique/non-unique secondary index
     entries with PostgreSQL-style multiple-NULL uniqueness, multi-row preflight,
     backfill validation/rollback, and UPDATE/DELETE maintenance.
   - Exact entry/distinct statistics and an initial optimizer that selects
     primary-key or selective single-column equality scans; chosen paths and
     index names appear in `EXPLAIN`.
7. **MVCC and transactions (implemented)**
   - Timestamped catalog/row/index versions, stable snapshots, buffered write
     intents, and all-or-nothing checksummed WAL batches with restart recovery.
   - Optimistic serializable validation covers write/write, point read/write,
     and range phantom conflicts. SQL statements are atomic by default, while
     `BEGIN`, `COMMIT`, and `ROLLBACK` provide multi-statement transactions with
     read-your-writes behavior and aborted-transaction handling.
8. **Ranges and routing (implemented)**
   - Checksummed persistent descriptors provide a gap-free byte-key range map,
     generation-checked range requests, ordered multi-range scans, and explicit
     rejection of cross-range atomic batches pending distributed coordination.
   - Splits copy the future right-hand span before atomically publishing both
     descriptors, then clean obsolete source copies. A multi-LSM harness covers
     boundary routing, data movement, stale clients, restart, and corruption.
9. **Raft replication (implemented)**
   - Each range can be backed by an independent durable Raft group with
     persisted terms, votes, checksummed logs, RequestVote elections, log
     matching/conflict repair, majority commit, and linearizable read barriers.
   - Replicated KV command batches, snapshot creation/installation, serialized
     add/remove membership entries, partitions, leader replacement, follower
     catch-up, and node restart are covered by multi-node recovery tests. The
     embedded SQL engine now runs its initial range through a one-member group.
10. **Distributed execution (read runtime implemented)**
    - Optimized primary, secondary, and full scans derive physical fragments at
      current range boundaries; the SQL engine exposes these scheduled spans.
    - Range-local endpoint tasks run through bounded streaming exchanges with
      admission limits, cancellation, missing-node handling, and retry-before-
      emit semantics. Streaming map/filter/project stages, equality hash joins,
      and typed global `COUNT`/`SUM` aggregation compose over those exchanges.
    - Cross-range mutations remain fail-closed; a recoverable distributed commit
      protocol is still required before enabling them.
11. **Automatic placement**
    - Replica placement, lease transfer, split/merge policy, hot-range detection,
      and rebalancing.
12. **PostgreSQL compatibility**
    - Startup/authentication, simple and extended query protocols, PostgreSQL
      type/result encoding, sessions, prepared statements, and cancellation.
13. **Hardening**
    - Deterministic simulation, crash/partition/disk-fault testing, backup and
      restore, observability, security, compatibility suites, and benchmarking.

PostgreSQL wire support is intentionally late in the dependency chain but can be
developed earlier as an adapter once stable session and result interfaces exist.

## Next implementation slice

Milestone 11 introduces automatic placement, hot-range policy, and rebalancing.
The completed read path is:

```text
optimized SQL scan -> range span derivation -> admitted remote processors
                   -> bounded exchanges -> join/aggregate -> result stream
```

Cross-range SQL commits remain deliberately disabled until the distributed
transaction coordinator arrives; no partial cross-range commit is allowed.
