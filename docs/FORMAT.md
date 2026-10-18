# The store file

A store is one file. Next to it the writer keeps `NAME.lock`, an empty file it
holds a lock on, and during a compaction `NAME.compact` exists for a moment.
Neither holds data. Don't delete `NAME.lock` while the store is in use: a
writer that opened after the deletion would lock a different file, and two
writers could then change the store at once. If opening ever cut a torn
record off the end (see Crashes), its bytes are kept in `NAME.torn-<time>`.

## Layout

```
header   8 bytes   "MVERGE", a flags byte, 0x01 (format 1)
record   ...
record   ...
```

Each record is:

```
length   4 bytes   little-endian length of the payload
crc      4 bytes   little-endian CRC-32C (Castagnoli) of the payload
payload  length bytes
```

The flags byte is 0x00 for a log and 0x01 for a compacted file, whose first
record is a snapshot. No payload is empty.

The first byte of a payload says what it is: a change (1 to 10) or a snapshot
(0x7F). A snapshot can only be the first record. A file is read by starting
from the snapshot, or from an empty store if there is none, and applying the
changes in order. Applying a change runs exactly the code that made it, with
the time it recorded, so the result is the same state, bit for bit.

## Writing

Each change is applied in memory first. Only if it succeeds is its record
written, in one write call, at the end of the file, and synced to disk
(`fsync`; `F_FULLFSYNC` on macOS) before the call returns. A change the rules
refuse writes nothing. If the write or the sync fails, the writer cuts the
record back off the file, returns the error, and stops accepting changes until
the store is reopened, so memory and file can't drift apart. If cutting it off
fails too, the record may still be there when the file is reopened; the error
says so.

Promote is one record. So is an Observe, however many facts, deaths and weight
changes it makes.

## Crashes

A crash can only damage the end of the file: the record being written. On
open, a tail is **torn** when:

- it is all zero bytes, which a filesystem can leave when the file grew before
  its data reached the disk; or
- the last record is shorter than its length says, or fails its checksum, and
  no valid record follows it.

The writer cuts a torn tail off (keeping its bytes in `NAME.torn-<time>`
unless they're all zeros), and a reader ignores it. The change in a torn record
never returned to its caller, so dropping it loses nothing that was confirmed.

Everything else that doesn't parse is **corruption**, and opening fails with
`ErrCorrupt`, leaving the file as it is: a bad record with a valid one after it,
a length field that can't be right, an empty record, a snapshot that fails its
checksum or its checks, or a change that no longer applies. A compacted file's
snapshot can't be torn, since it was written to another file and synced before
the rename, so any damage to it is corruption.

A file shorter than the header, which a crash while creating it can leave, is
an empty store.

## Compaction

When the log written since the last snapshot passes a limit (by default the
larger of 1 MiB and twice the last snapshot), the writer:

1. writes the whole store as one snapshot record to `NAME.compact`, and syncs
   it;
2. renames it over `NAME`, and syncs the directory;
3. carries on appending to the new file.

A crash before the rename leaves the old file, which holds the same state, and
the next open deletes the unfinished `NAME.compact`. A crash after it leaves the
new one. If writing the new file or the rename fails (on Windows, the rename
fails while a reader has the file open), the old file stays in use and the next
try waits until the log has doubled, so a failing compaction doesn't rewrite the
store on every change. If the store's file can't be reopened after a successful
rename, the change that triggered it is still saved and reported as done, and
the store refuses later changes until it is opened again.

The snapshot writes each node of the copy-on-write maps once, even when the
trunk and many branches share it, and a reload shares them again. So a
thousand branches that each changed three keys of a 10,000-key trunk take
about as much file as the trunk and their changes, not a thousand trunks.

The snapshot also keeps the history: the most recent events, up to
`Options.HistoryLimit` (10,000 by default). In memory the history is trimmed
back to that limit each time it reaches twice it, so keeping it costs nothing
per change.

## Locking

A writer (`Open`) holds an exclusive lock on `NAME.lock` for as long as it is
open: `flock` on Linux, macOS and the BSDs, and an open with no sharing on
Windows. The operating system drops it when the process ends, however it
ends. A second writer waits for it, up to `Options.LockTimeout`.

Readers (`OpenReadOnly`, and the command's read commands) take no lock. They
read the file in one go and see every change that had completed by then; a
record being written at that moment looks like a torn tail and is skipped.
Because compaction renames a complete new file into place, a reader sees the
old file or the new one, never a mix.

## Payloads

Integers are unsigned or zigzag varints, as in Go's `encoding/binary`. Strings
and byte strings are a varint length and the bytes. Floats are the 8-byte
little-endian IEEE 754 bits, so weights and likelihoods are kept exactly. Times
are Unix nanoseconds.

| Kind | Byte | Fields after the kind and the time |
|---|---|---|
| Set | 1 | branch, key, value |
| Delete | 2 | branch, key |
| Fork | 3 | name, from, weight |
| Weight | 4 | name, weight |
| Constrain | 5 | name, count, then key, operator byte, value for each |
| Expire | 6 | name, deadline (0 clears it) |
| Observe | 7 | fact count, then key, delete flag, value for each; likelihood count, then name, value for each, sorted by name; rest flag, rest; prune |
| Discard | 8 | name |
| Promote | 9 | name |
| Sweep | 10 | nothing |

Operator bytes: `==` 1, `!=` 2, `<` 3, `<=` 4, `>` 5, `>=` 6, `exists` 7,
`absent` 8.

A snapshot (0x7F) holds the generation, the next fork number, the trunk's
version and the next event number; then the map nodes, each a key, a value
and the numbers of its left and right children (plus one, 0 for none), children
before parents; then the trunk's root; then each branch with its name, parent,
fork number, root, weight, trunk version at its fork, constraints, deadline,
creation time, dead flag, cause, detail and time of death; then the history.
It is checked as it is read: a node whose children aren't in key order or heap
order, a bad branch name, an unknown parent, an invalid constraint, or a weight
or cause that doesn't fit the branch being live or dead is rejected as
corruption.
