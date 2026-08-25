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
mkdir -p "$SRC"/{a/b/c,logs/2024,logs/2023,empty}
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
"$AZCP" -r --include '*.gz' "$AZ/tree" "$WORK/only" >/dev/null
check "--include selects" "$(find "$WORK/only" -type f -name '*.gz' | wc -l)" \
                          "$(find "$SRC" -type f -name '*.gz' | wc -l)"
check "--include excludes the rest" "$(find "$WORK/only" -type f ! -name '*.gz' | wc -l)" "0"

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
