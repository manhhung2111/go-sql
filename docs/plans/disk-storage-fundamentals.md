# Disk storage fundamentals — study notes

Running notes from a topic-by-topic walkthrough of the concepts behind
saving data to disk, ahead of designing on-disk persistence for
go-sql's storage engine. General database-internals theory first;
we'll connect each concept back to `internal/engine`'s `SqlTable` once
the theory is solid.

Topics, in order:

1. Data serialization
2. Fixed-width vs. variable-width encoding
3. File seek operations
4. OS page cache
5. Slotted page layout
6. B-tree in memory
7. B-tree on disk

---

## 1. Data serialization

### What serialization actually is

In memory, your data isn't just "the values" — it's values sitting at
specific memory addresses, wired together by pointers, laid out
according to whatever your language runtime and CPU architecture
decided. A Go `struct` in RAM has fields at specific byte offsets,
padding inserted for alignment, and any slice/string/map field is
really just a pointer to another allocation somewhere else in the
heap.

None of that survives being written to a file. When the process exits
and restarts, those memory addresses mean nothing — the OS will hand
out completely different addresses next time, ASLR will randomize
them, and the file itself is just a flat sequence of bytes sitting on
a block device, with no concept of "pointer to heap object #4271."

**Serialization is the act of converting an in-memory value into a
self-contained, addressless byte sequence** — one that carries enough
information to reconstruct the value later, in a different process,
on possibly a different machine, using nothing but those bytes. The
inverse operation (bytes → in-memory value) is deserialization.

### Why you can't just `memcpy` the struct to disk

The naive idea — "just write the raw bytes of my struct to a file" —
breaks for several concrete reasons:

1. **Pointers are meaningless outside the process.** Any field that's
   a pointer, slice header, string header, or map — in Go, a `string`
   is actually a `(pointer, length)` pair, a slice is `(pointer,
   length, capacity)` — points to heap memory that won't exist when
   you reload the file. You'd get a dangling pointer, not your data.
2. **Struct layout isn't guaranteed stable.** The compiler decides
   field order, padding, and alignment — often differently across
   compiler versions, architectures (32-bit vs 64-bit), or even build
   flags. A raw byte dump written by one build might not even *parse*
   correctly when read back by a slightly different build of the same
   program.
3. **Endianness** — whether the most-significant or least-significant
   byte of a multi-byte integer comes first — differs across CPU
   architectures. A raw 4-byte `int32` written on a little-endian
   machine reads back as a different number on a big-endian one.
4. **Padding bytes are uninitialized garbage.** Struct alignment often
   inserts unused padding bytes between fields, whose contents are
   whatever memory happened to contain — not meaningful data, but if
   you serialize them naively you're storing (and needing to account
   for) noise.

### The actual solution: an explicit encoding

Instead, you define an **encoding**: a deterministic, documented
mapping from "the logical value" (an int64, a string, a row of
columns) to "a specific, self-contained sequence of bytes," with a
matching decoding function that's the exact inverse. You pick:

- A fixed byte order (almost always little-endian by convention now,
  since x86/ARM are little-endian and it avoids needless conversion)
- An explicit width for every fixed-size field (e.g. "this integer is
  always exactly 8 bytes")
- An explicit strategy for anything variable-length (a string, a
  variable number of columns) — which is exactly the fixed-width vs.
  variable-width encoding question, next

Once you have that, the byte sequence is portable: any program that
knows the encoding — regardless of language, architecture, or
compiler — can decode it correctly. This is precisely why formats like
Protocol Buffers, or a database's on-disk row format, are specified as
an explicit byte layout document, not "whatever the struct happens to
look like in memory."

---

## 2. Fixed-width vs. variable-width encoding

*(pending)*

## 3. File seek operations

*(pending)*

## 4. OS page cache

*(pending)*

## 5. Slotted page layout

*(pending)*

## 6. B-tree in memory

*(pending)*

## 7. B-tree on disk

*(pending)*