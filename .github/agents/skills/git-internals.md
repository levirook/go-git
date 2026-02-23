# Git Internals

This skill captures the core Git internals knowledge needed when implementing or modifying features in go-git. The canonical reference is [git/git](https://github.com/git/git) and its documentation at https://git-scm.com/docs.

---

## Object Model

Git stores everything as **content-addressable objects**. An object's identity is the SHA-1 (or SHA-256 in `--object-format=sha256` repos) hash of: `"<type> <size>\0<content>"`.

| Type   | go-git type              | Description |
|--------|--------------------------|-------------|
| blob   | `plumbing.BlobObject`    | Raw file content; no filename or mode |
| tree   | `plumbing.TreeObject`    | Directory listing: sorted entries of `<mode> <name>\0<hash>` |
| commit | `plumbing.CommitObject`  | Points to one tree + zero or more parent commits |
| tag    | `plumbing.TagObject`     | Named, optionally-signed pointer to any object |

**Tree entry sort order**: entries are sorted as if directories had a trailing `/`. A subtree named `foo` sorts after a blob named `foo-bar` but before `foo/bar`. Incorrect sort order changes the tree hash. See `git/git: tree.c`.

**Commit format** (header lines, blank line, body):
```
tree <hash>
parent <hash>      (zero or more)
author <name> <email> <unix-ts> <tz-offset>
committer <name> <email> <unix-ts> <tz-offset>
encoding <name>    (optional, default UTF-8)
gpgsig <sig>       (optional, multi-line with space-continuation)
mergetag <tag>     (optional, embedded tag object for signed-tag merges)

<message>
```

**Hash computation**: always hash the decoded (uncompressed) content, not the on-disk representation.

---

## Object Storage

### Loose objects
Stored as zlib-deflated files at `.git/objects/<xx>/<38-hex-chars>`. Read/write via `plumbing/format/objfile`.

### Pack files
A single `.pack` file + `.idx` index file, stored in `.git/objects/pack/`. Format:
- Header: magic `PACK`, version (2), object count (uint32 big-endian)
- Objects: variable-length header encoding type+size, then zlib data (or delta)
- Trailer: SHA-1/SHA-256 checksum of the entire pack

**Delta types**: `OBJ_OFS_DELTA` (offset-relative base) and `OBJ_REF_DELTA` (hash-addressed base). Delta instructions are copy/insert ops applied to reconstruct the target. Depth limit is 50 (configurable). See `plumbing/format/packfile`.

**Pack index v2**: fan-out table → sorted SHA list → CRC32 list → offset list → large-offset table.

**Commit graph** (`.git/objects/info/commit-graph`): accelerates `git log`/reachability by pre-computing generations and parent pointers. See `plumbing/format/commitgraph`.

---

## References

Refs are pointers to object hashes. Hierarchy: `refs/heads/`, `refs/tags/`, `refs/remotes/`, `refs/notes/`. Symbolic refs (e.g. `HEAD`) store `ref: refs/heads/<branch>`.

**RefRevParseRules** (same as git's `expand_ref`):
```
%s  →  refs/%s  →  refs/tags/%s  →  refs/heads/%s  →  refs/remotes/%s  →  refs/remotes/%s/HEAD
```

Packed refs live in `.git/packed-refs`; loose refs in `.git/refs/**`. A loose ref takes precedence over a packed one.

---

## Index (Staging Area)

The index (`.git/index`) tracks the working tree. Each entry contains: `<ctime> <mtime> <dev> <ino> <mode> <uid> <gid> <size> <hash> <flags> <name>`. Version 2 is standard; version 3/4 add extended flags and path compression.

Extensions: `TREE` (cached tree for fast tree construction), `REUC` (resolve-undo for merge conflict recovery), `EOIE` (end-of-index entry for parallel reads). See `plumbing/format/index`.

**Stage bits** in flags: 0 = merged, 1 = ancestor (base), 2 = ours, 3 = theirs. Multiple stages for the same path signal an unresolved conflict.

---

## Transfer Protocols

go-git supports **smart HTTP**, **SSH**, **git://**, and **file://** transports (`plumbing/transport`).

**Upload-pack** (fetch/clone): reference discovery → `want`/`have` negotiation (multi-round) → pack delivery. Negotiation terminates when server finds a common `ACK` or client exhausts its history.

**Receive-pack** (push): reference advertisement → client sends pack → server updates refs and runs hooks. Ref update is rejected if it is not a fast-forward (unless `--force`).

**Protocol v2** (`plumbing/protocol/packp`): capability-driven, supports `ls-refs`, `fetch`, and `push` commands; reduces round-trips. `agent`, `side-band-64k`, `ofs-delta`, `shallow`, `filter` are common capabilities.

**pkt-line** framing: 4-hex-digit length prefix (including the 4 bytes themselves); `0000` flush; `0001` delimiter (v2); `0002` response-end (v2). See `plumbing/format/pktline`.

---

## Key Invariants to Enforce

1. **Hash must match content**: recompute and verify the hash whenever encoding or decoding an object.
2. **Tree entries must be sorted**: use the git tree-sort collation (directories sort with trailing `/`).
3. **Timestamps**: stored as Unix seconds + `±HHMM` offset; the `When` field in `Signature` must preserve the offset.
4. **Delta depth**: do not produce delta chains deeper than 50 levels.
5. **Behaviour parity**: any feature must be reproducible with the reference `git` binary. Run the same operation with `git` to confirm expected output before finalising an implementation.
6. **SHA-256 support**: use `plumbing.ObjectID` / `format.ObjectFormat`; never hardcode SHA-1 sizes (20 bytes / 40 hex chars).

---

## Useful References

| Topic | Source |
|-------|--------|
| Object formats | `git/git: Documentation/gitformat-*.txt` (e.g. `gitformat-pack`, `gitformat-index`) |
| Transfer protocols | `git/git: Documentation/gitprotocol-*.txt` |
| All git formats | https://git-scm.com/docs (filter by "Git Formats") |
| go-git internals | `plumbing/`, `plumbing/format/`, `plumbing/transport/` |
| Compatibility notes | `COMPATIBILITY.md` in repo root |
