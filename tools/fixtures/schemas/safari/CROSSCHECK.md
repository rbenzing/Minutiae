# DDL cross-check (plan 4F, Task 6): Safari History.db

No public source defines the schema of Safari's `History.db`. Both snapshots in this directory are
written from memory (plan 4F ruling 4F-R12), not fetched, extracted or compared with any
definition. They are therefore not cross-checked by a script: there is nothing to compare them with.

Each snapshot states `-- verdict no-public-source` and `-- source memory (no public schema; plan 4F ruling 4F-R12)`
in its header. The fixtures generated from them (`tools/fixtures/browser_safari.py`) are validated only against
the same memory, so a fixture proves that the parser reads the remembered layout, never that a real
database has it. A real database that differs degrades through the parser's probe
(`unsupported-schema`, `not-applicable`) or through absent optional fields; running the parser over
real databases (plan 4H, `realdata`) is required before relying on it.

| File | Table | Verdict | Evidence |
|---|---|---|---|
| safari-current.sql | history_items | no-public-source | memory |
| safari-current.sql | history_visits | no-public-source | memory |
| safari-current.sql | history_tombstones | no-public-source | memory |
| safari-current.sql | metadata | no-public-source | memory |
| safari-no-tombstones.sql | history_items | no-public-source | memory |
| safari-no-tombstones.sql | history_visits | no-public-source | memory |
| safari-no-tombstones.sql | metadata | no-public-source | memory |

The meanings of `origin`, `generation`, `attributes`, `score` and `synthesized` are not known: the parser
stores them as the integers they are, uninterpreted (4F-R24).
