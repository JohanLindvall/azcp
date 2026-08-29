#!/usr/bin/env bash
#
# End-to-end check of the blob paths against the Azurite emulator.
#
# Run it with `make e2e`, which starts and stops the emulator around it, or
# point it at an already-running one:
#
#     AZCP=./bin/azcp scripts/e2e.sh
#
# The account key below is Azurite's published development key. It is the same
# for every installation and grants access to nothing but a local emulator.

set -euo pipefail

AZCP=${AZCP:-./bin/azcp}
AZURITE_PORT=${AZURITE_PORT:-10000}

export AZURE_STORAGE_ACCOUNT=devstoreaccount1
export AZURE_STORAGE_KEY='Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=='

ACCOUNT="http://127.0.0.1:${AZURITE_PORT}/devstoreaccount1"
CONTAINER="e2e-$$"
AZ="${ACCOUNT}/${CONTAINER}"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

ok()   { printf '  \033[32mok\033[0m    %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail + 1)); }
check() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

# sums lists every file under a directory with its checksum, relative to it, so
# two trees can be compared regardless of where they live.
sums() { ( cd "$1" && find . -type f | sort | xargs -r md5sum | sed "s#  \./#  #" ); }

echo "azcp end-to-end against ${ACCOUNT}"
echo

# --- a tree worth copying ---------------------------------------------------
SRC="$WORK/src"
mkdir -p "$SRC"/{a/b/c,logs/2024,logs/2023,empty,hollow/inner}
echo "hello" > "$SRC/file.txt"
echo "deep"  > "$SRC/a/b/c/deep.txt"
echo "note"  > "$SRC/logs/note.md"
for i in 1 2 3; do head -c 100000 /dev/urandom > "$SRC/logs/2024/app-$i.log"; done
head -c 50000 /dev/urandom > "$SRC/logs/2023/old.log"
head -c 12000000 /dev/urandom > "$SRC/big.bin"   # forces a multi-block transfer

# --- upload -----------------------------------------------------------------
"$AZCP" --create-container -r "$SRC" "$AZ/tree" >/dev/null
ok "recursive upload"

# --- download and compare ---------------------------------------------------
mkdir -p "$WORK/down"
"$AZCP" -r "$AZ/tree" "$WORK/down" >/dev/null
if diff -q <(sums "$SRC") <(sums "$WORK/down/tree") >/dev/null; then
  ok "downloaded tree is byte-identical"
else
  bad "downloaded tree differs"
  diff <(sums "$SRC") <(sums "$WORK/down/tree") | head -10
fi

# An empty directory has no object of its own; it survives as a marker blob.
[ -d "$WORK/down/tree/empty" ] && ok "empty directory round-trips" \
                               || bad "empty directory was lost"

# A directory holding only an empty directory needs no marker of its own — the
# leaf's marker sits beneath it — but the shape must still round-trip.
[ -d "$WORK/down/tree/hollow/inner" ] && ok "a nested empty directory round-trips" \
                                      || bad "a nested empty directory was lost"

# --- wildcards over blob names ----------------------------------------------
mkdir -p "$WORK/glob"
"$AZCP" "$AZ/tree/**/*.log" "$WORK/glob" >/dev/null
check "** matches across prefixes" "$(ls "$WORK/glob" | wc -l)" "4"

mkdir -p "$WORK/ext"
"$AZCP" "$AZ/tree/logs/2024/!(app-2).log" "$WORK/ext" >/dev/null
check "extended pattern excludes a match" "$(ls "$WORK/ext" | wc -l)" "2"

mkdir -p "$WORK/brace"
"$AZCP" "$AZ/tree/logs/{2023,2024}/*.log" "$WORK/brace" >/dev/null
check "brace expansion" "$(ls "$WORK/brace" | wc -l)" "4"

# --- blob to blob -----------------------------------------------------------
"$AZCP" -r "$AZ/tree" "$AZ/copy" >/dev/null
mkdir -p "$WORK/s2s"
"$AZCP" -r "$AZ/copy" "$WORK/s2s" >/dev/null
if diff -q <(sums "$SRC") <(sums "$WORK/s2s/copy") >/dev/null; then
  ok "blob-to-blob copy is byte-identical"
else
  bad "blob-to-blob copy differs"
fi
[ -d "$WORK/s2s/copy/hollow/inner" ] && ok "a nested empty directory survives blob-to-blob" \
                                     || bad "blob-to-blob lost the nested empty directory"

# The emulator does not implement the from-URL operations, so this also proves
# the fallback to the asynchronous Copy Blob route works.
routes=$("$AZCP" --log-level=debug "$AZ/tree/file.txt" "$AZ/route-probe.txt" 2>&1 \
         | grep -c 'copied server-side' || true)
check "copy happens server-side" "$routes" "1"

# --- the account root -------------------------------------------------------
# It has no path element of its own, so the account name stands in for one.
mkdir -p "$WORK/acct"
"$AZCP" -r "$ACCOUNT/" "$WORK/acct" >/dev/null
if [ -d "$WORK/acct/devstoreaccount1/$CONTAINER/tree" ]; then
  ok "the whole account copies under its own name"
else
  bad "copying the account root did not produce devstoreaccount1/$CONTAINER/tree"
fi

# -T asks for the contents rather than the account as a directory.
mkdir -p "$WORK/acctT"
"$AZCP" -rT "$ACCOUNT/" "$WORK/acctT" >/dev/null
[ -d "$WORK/acctT/$CONTAINER" ] && ok "-T copies the account's containers directly" \
                                || bad "-T at the account root put things in the wrong place"

# A pattern that spans containers.
mkdir -p "$WORK/across"
"$AZCP" -r "$ACCOUNT/*/tree/logs" "$WORK/across" >/dev/null
[ -d "$WORK/across/logs/2024" ] && ok "a wildcard matches across containers" \
                               || bad "a container wildcard matched nothing"

# --- filtering --------------------------------------------------------------
mkdir -p "$WORK/filtered"
"$AZCP" -r --exclude '*.log' "$AZ/tree" "$WORK/filtered" >/dev/null
if [ -f "$WORK/filtered/tree/logs/2024/app-1.log" ]; then
  bad "--exclude did not exclude"
else
  ok "--exclude skips by name at any depth"
fi
[ -f "$WORK/filtered/tree/file.txt" ] && ok "--exclude kept everything else" \
                                      || bad "--exclude took too much"

mkdir -p "$WORK/pruned"
"$AZCP" -r --exclude 'logs/**' "$AZ/tree" "$WORK/pruned" >/dev/null
[ -d "$WORK/pruned/tree/logs" ] && bad "an excluded subtree was still copied" \
                               || ok "--exclude prunes a whole subtree"

mkdir -p "$WORK/only"
"$AZCP" -r --include '*.log' "$AZ/tree" "$WORK/only" >/dev/null
check "--include selects" "$(find "$WORK/only" -type f -name '*.log' | wc -l)" \
                          "$(find "$SRC" -type f -name '*.log' | wc -l)"
check "--include excludes the rest" "$(find "$WORK/only" -type f ! -name '*.log' | wc -l)" "0"

# --- integrity --------------------------------------------------------------
"$AZCP" --put-md5 "$SRC/big.bin" "$AZ/tree/md5.bin" >/dev/null
if "$AZCP" --check-md5=require "$AZ/tree/md5.bin" "$WORK/md5.bin" >/dev/null 2>&1; then
  ok "--put-md5 records a checksum that --check-md5=require accepts"
else
  bad "a checksum written by --put-md5 did not verify"
fi
cmp -s "$SRC/big.bin" "$WORK/md5.bin" && ok "the verified file is byte-identical" \
                                     || bad "the verified file differs"

# --- bandwidth --------------------------------------------------------------
# 12 MB at 8 MB/s cannot finish in under a second; the point is that the cap
# takes effect at all, not its precision.
start=$(date +%s)
"$AZCP" --bwlimit=8M "$AZ/tree/big.bin" "$WORK/capped.bin" >/dev/null
elapsed=$(( $(date +%s) - start ))
[ "$elapsed" -ge 1 ] && ok "--bwlimit paces the transfer" \
                     || bad "--bwlimit had no effect (finished in ${elapsed}s)"
cmp -s "$SRC/big.bin" "$WORK/capped.bin" && ok "a throttled transfer is still exact" \
                                         || bad "throttling corrupted the file"

# --- attributes survive the round trip --------------------------------------
ATTR="$WORK/attr"
mkdir -p "$ATTR/src"
echo mode > "$ATTR/src/mode.txt"; chmod 4750 "$ATTR/src/mode.txt"
ln -s mode.txt "$ATTR/src/link.txt"
touch -d '2021-06-15T10:20:30Z' "$ATTR/src/mode.txt" 2>/dev/null || true
"$AZCP" -a "$ATTR/src" "$AZ/attrs" >/dev/null
mkdir -p "$ATTR/back"
"$AZCP" -a "$AZ/attrs" "$ATTR/back" >/dev/null
check "-a preserves the mode through blob storage" \
  "$(stat -c '%a' "$ATTR/back/attrs/mode.txt" 2>/dev/null)" "4750"
[ -L "$ATTR/back/attrs/link.txt" ] && ok "-a round-trips a symbolic link" \
                                   || bad "the symbolic link did not come back"

# The same tree fetched without -a: --copy-metadata is what lets a blob that
# records a symbolic link come back as one instead of as an empty file.
mkdir -p "$ATTR/plain"
"$AZCP" -r --copy-metadata "$AZ/attrs" "$ATTR/plain" >/dev/null
[ -L "$ATTR/plain/attrs/link.txt" ] && ok "--copy-metadata downloads a recorded symlink as one" \
                                    || bad "a recorded symlink flattened to an empty file"

# --- content encoding -------------------------------------------------------
printf 'compressed payload\n' | gzip -9 > "$WORK/page.gz"
"$AZCP" --content-encoding=gzip "$WORK/page.gz" "$AZ/page.gz" >/dev/null
"$AZCP" "$AZ/page.gz" "$WORK/raw.gz" >/dev/null
cmp -s "$WORK/page.gz" "$WORK/raw.gz" \
  && ok "an encoded blob downloads as the bytes that were stored" \
  || bad "an encoded blob was altered in transit"
"$AZCP" --decompress "$AZ/page.gz" "$WORK/out.gz" >/dev/null
check "--decompress expands and drops the extension" \
  "$(cat "$WORK/out" 2>/dev/null)" "compressed payload"

# --- metadata ---------------------------------------------------------------
"$AZCP" --metadata "batch=nightly,source=e2e" "$SRC/file.txt" "$AZ/meta.txt" >/dev/null
ok "--metadata is accepted on upload"

# --- attributes-only ---------------------------------------------------------
echo "original blob" > "$WORK/attronly.txt"
"$AZCP" "$WORK/attronly.txt" "$AZ/attronly.txt" >/dev/null
echo "changed locally" > "$WORK/attronly.txt"
"$AZCP" --attributes-only --metadata stage=e2e "$WORK/attronly.txt" "$AZ/attronly.txt" >/dev/null
"$AZCP" "$AZ/attronly.txt" "$WORK/attronly-back.txt" >/dev/null
check "--attributes-only leaves blob content alone" \
  "$(cat "$WORK/attronly-back.txt")" "original blob"

echo "keep me" > "$WORK/attronly-dl.txt"
"$AZCP" --attributes-only "$AZ/attronly.txt" "$WORK/attronly-dl.txt" >/dev/null
check "--attributes-only leaves local content alone" \
  "$(cat "$WORK/attronly-dl.txt")" "keep me"

# --- attributes survive a blob-to-blob copy ----------------------------------
"$AZCP" -r --copy-metadata "$AZ/attrs" "$AZ/attrs-copy" >/dev/null
mkdir -p "$ATTR/copyback"
"$AZCP" -a "$AZ/attrs-copy" "$ATTR/copyback" >/dev/null
check "-a metadata survives a blob-to-blob copy" \
  "$(stat -c '%a' "$ATTR/copyback/attrs-copy/mode.txt" 2>/dev/null)" "4750"

# --- resume -----------------------------------------------------------------
"$AZCP" --resume "$SRC/big.bin" "$AZ/resume.bin" >/dev/null
"$AZCP" --resume "$AZ/resume.bin" "$WORK/resume.bin" >/dev/null
cmp -s "$SRC/big.bin" "$WORK/resume.bin" && ok "--resume completes a whole transfer" \
                                         || bad "--resume corrupted the file"
[ -f "$WORK/resume.bin.azcp-part" ] && bad "the resume record was left behind" \
                                    || ok "the resume record is cleaned up"

# --- delete -----------------------------------------------------------------
mkdir -p "$WORK/sync/keep"
echo one > "$WORK/sync/one.txt"
"$AZCP" -rT "$WORK/sync" "$AZ/synced" >/dev/null
"$AZCP" "$SRC/file.txt" "$AZ/synced/stray.txt" >/dev/null
"$AZCP" -rT --delete "$WORK/sync" "$AZ/synced" >/dev/null
if "$AZCP" "$AZ/synced/stray.txt" "$WORK/stray-check" >/dev/null 2>&1; then
  bad "--delete left a blob the source does not have"
else
  ok "--delete removes what the source does not have"
fi

# An empty directory at the destination is a marker blob; --delete removes it.
mkdir -p "$WORK/sync/vanish"
"$AZCP" -rT "$WORK/sync" "$AZ/synced" >/dev/null
rmdir "$WORK/sync/vanish"
"$AZCP" -rT --delete "$WORK/sync" "$AZ/synced" >/dev/null
mkdir -p "$WORK/sync-back"
"$AZCP" -rT "$AZ/synced" "$WORK/sync-back" >/dev/null
[ -d "$WORK/sync-back/vanish" ] && bad "--delete left an empty directory's marker behind" \
                                || ok "--delete removes an empty directory's marker"

# --- machine-readable output ------------------------------------------------
summary=$("$AZCP" --output=json "$SRC/file.txt" "$AZ/json.txt" 2>/dev/null | tail -1)
if printf '%s' "$summary" | grep -q '"event":"summary"'; then
  ok "--output=json ends with a summary object"
else
  bad "--output=json produced no summary: $summary"
fi

# --- benchmark --------------------------------------------------------------
if "$AZCP" --benchmark=2x1MiB --output=json "$AZ/bench/" 2>/dev/null \
     | grep -q upload_bytes_per_second; then
  ok "--benchmark measures and reports throughput"
else
  bad "--benchmark did not report a result"
fi

# --- overwrite rules --------------------------------------------------------
echo "changed" > "$WORK/changed.txt"
"$AZCP" -n "$WORK/changed.txt" "$AZ/tree/file.txt" >/dev/null
"$AZCP" "$AZ/tree/file.txt" "$WORK/after.txt" >/dev/null
check "-n leaves an existing blob alone" "$(cat "$WORK/after.txt")" "hello"

"$AZCP" "$WORK/changed.txt" "$AZ/tree/file.txt" >/dev/null
"$AZCP" "$AZ/tree/file.txt" "$WORK/after2.txt" >/dev/null
check "a plain copy overwrites" "$(cat "$WORK/after2.txt")" "changed"

# --- error reporting --------------------------------------------------------
if "$AZCP" "$AZ/tree/does-not-exist.txt" "$WORK/x" >/dev/null 2>&1; then
  bad "a missing blob should fail"
else
  ok "a missing blob is reported and exits non-zero"
fi

echo
if [ "$fail" -eq 0 ]; then
  printf '\033[32m%d checks passed\033[0m\n' "$pass"
else
  printf '\033[31m%d of %d checks failed\033[0m\n' "$fail" "$((pass + fail))"
fi
exit $(( fail > 0 ? 1 : 0 ))
