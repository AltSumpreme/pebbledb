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
2. **Typed relational model and codecs**
   - SQL `NULL`, `BOOL`, `INT`, `BIGINT`, `TEXT`, `DECIMAL`, and typed values.
   - Stable key, row, and schema encodings with round-trip/order tests.
3. **Catalog**
   - Persistent databases, schemas, tables, columns, indexes, and constraints.
   - Transactional catalog operations, `SHOW TABLES`, and table description.
4. **SQL frontend**
   - Standards-shaped lexer/parser and typed AST for DDL and DML.
   - Binder resolves names and types exclusively through a catalog interface.
5. **Planning and local execution**
   - Logical and physical plans; scan, values, filter, project, limit, sort,
     insert, update, delete, aggregate, and join operators.
   - `EXPLAIN` exposes plans without leaking storage internals.
6. **Indexes**
   - Primary and secondary index maintenance, uniqueness checks, index scans,
     statistics, and initial cost-based access-path selection.
7. **MVCC and transactions**
   - Timestamped row/index versions, snapshots, intents, atomic write batches,
     conflict handling, recovery, and serializable validation.
8. **Ranges and routing**
   - Key-range descriptors, range-aware requests, splits, and a single-process
     multi-range test harness.
9. **Raft replication**
   - One consensus group per range, replicated commands/snapshots, membership
     changes, and quorum recovery tests.
10. **Distributed execution**
    - Span derivation, remote processors, streaming exchange, distributed joins
      and aggregation, cancellation, retry, and admission control.
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

Milestone 2 starts with standalone `types`, `codec`, and `catalog` contracts. The
first end-to-end vertical path will be:

```text
CREATE TABLE -> catalog records in LSM
INSERT -> typed row encoded under a primary key
SELECT by primary key -> ordered LSM lookup -> decoded row
```

Only after that path has restart tests should the legacy full-database snapshot
writer be retired or migrated.
