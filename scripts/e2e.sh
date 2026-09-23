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
"$AZCP" -rT "$AZ/tree/empty" "$AZ/empty-copy" >/dev/null
"$AZCP" -rT "$AZ/empty-copy" "$WORK/empty-copy" >/dev/null
[ -d "$WORK/empty-copy" ] && ok "a completely empty prefix survives blob-to-blob" \
                          || bad "blob-to-blob lost an empty root directory"

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
touch -h -d '2020-01-02T03:04:05Z' "$ATTR/src/link.txt"
"$AZCP" -a "$ATTR/src" "$AZ/attrs" >/dev/null
mkdir -p "$ATTR/back"
"$AZCP" -a "$AZ/attrs" "$ATTR/back" >/dev/null
check "-a preserves the mode through blob storage" \
  "$(stat -c '%a' "$ATTR/back/attrs/mode.txt" 2>/dev/null)" "4750"
[ -L "$ATTR/back/attrs/link.txt" ] && ok "-a round-trips a symbolic link" \
                                   || bad "the symbolic link did not come back"
check "-a preserves a symbolic link's own timestamp" \
  "$(stat -c '%Y' "$ATTR/back/attrs/link.txt")" "$(stat -c '%Y' "$ATTR/src/link.txt")"

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

# Copy Blob does not accept content-header overrides in the copy request.
# The fallback must apply them afterwards without discarding the checksum.
"$AZCP" --put-md5 "$WORK/page.gz" "$AZ/header-source.gz" >/dev/null
"$AZCP" --content-encoding=gzip "$AZ/header-source.gz" "$AZ/header-copy.gz" >/dev/null
"$AZCP" --decompress --check-md5=require "$AZ/header-copy.gz" "$WORK/header-copy.gz" >/dev/null
check "asynchronous copy applies headers and keeps MD5" "$(cat "$WORK/header-copy")" "compressed payload"

echo "keep expanded" > "$WORK/out"
"$AZCP" -n --decompress "$AZ/page.gz" "$WORK/out.gz" >/dev/null
check "-n protects the expanded destination" "$(cat "$WORK/out")" "keep expanded"
"$AZCP" --decompress --resume "$AZ/page.gz" "$WORK/resumed.gz" >/dev/null
check "--resume works with decompression" "$(cat "$WORK/resumed")" "compressed payload"
[ ! -e "$WORK/resumed.azcp-part" ] && ok "decompression finalizes the resume record" \
                                      || bad "decompression left a resume record"

"$AZCP" --content-encoding=gzip "$WORK/page.gz" "$AZ/encoded/page.gz" >/dev/null
mkdir -p "$WORK/expanded"
echo stale > "$WORK/expanded/extra"
"$AZCP" -rT --delete --decompress "$AZ/encoded" "$WORK/expanded" >/dev/null
check "--delete keeps decompressed files" "$(cat "$WORK/expanded/page")" "compressed payload"
[ ! -e "$WORK/expanded/extra" ] && ok "--delete removes extras beside decompressed files" \
                                || bad "--delete left an extra beside a decompressed file"

echo "attributes only" > "$WORK/attr-encoded.gz"
"$AZCP" --attributes-only --decompress "$AZ/page.gz" "$WORK/attr-encoded.gz" >/dev/null
check "--attributes-only never decompresses existing data" "$(cat "$WORK/attr-encoded.gz")" "attributes only"

# --- compression --------------------------------------------------------------
# --compress stores the file under its name plus the extension, with the
# encoding set — exactly what --decompress expects to find.
"$AZCP" --compress=gzip:9 "$SRC/file.txt" "$AZ/packed/" >/dev/null
"$AZCP" "$AZ/packed/file.txt.gz" "$WORK/file.txt.gz" >/dev/null
check "--compress writes gzip under the extended name" "$(gunzip -c "$WORK/file.txt.gz")" "hello"
"$AZCP" --decompress "$AZ/packed/file.txt.gz" "$WORK/unpacked.gz" >/dev/null
check "--decompress reads back what --compress wrote" "$(cat "$WORK/unpacked")" "hello"

# A multi-block source streams up as it is compressed; the checksum follows it.
"$AZCP" --compress=zstd --put-md5 "$SRC/big.bin" "$AZ/packed/" >/dev/null
mkdir -p "$WORK/unpack"
"$AZCP" --decompress --check-md5=require "$AZ/packed/big.bin.zst" "$WORK/unpack/" >/dev/null
cmp -s "$SRC/big.bin" "$WORK/unpack/big.bin" \
  && ok "a multi-block compressed upload round-trips with its checksum" \
  || bad "a multi-block compressed upload did not round-trip"

# A file that already is compressed is stored as it is, not wrapped again.
"$AZCP" --compress "$WORK/page.gz" "$AZ/packed/" >/dev/null
"$AZCP" "$AZ/packed/page.gz" "$WORK/page-again.gz" >/dev/null
cmp -s "$WORK/page.gz" "$WORK/page-again.gz" \
  && ok "--compress leaves an already compressed file alone" \
  || bad "--compress wrapped a .gz a second time"

# A tarball's abbreviated suffix expands to the .tar it stands for.
cp "$WORK/page.gz" "$WORK/src-bundle.tgz"
"$AZCP" --content-encoding=gzip "$WORK/src-bundle.tgz" "$AZ/packed/bundle.tgz" >/dev/null
"$AZCP" --decompress "$AZ/packed/bundle.tgz" "$WORK/bundle.tgz" >/dev/null
check "--decompress turns .tgz into .tar" "$(cat "$WORK/bundle.tar" 2>/dev/null)" "compressed payload"

# Reserved URL characters belong to the key, including in a copy-source URL.
"$AZCP" "$SRC/file.txt" "$AZ/reserved%3Fname%23percent%25.txt" >/dev/null
"$AZCP" "$AZ/reserved%3Fname%23percent%25.txt" "$AZ/reserved-copy.txt" >/dev/null
"$AZCP" "$AZ/reserved-copy.txt" "$WORK/reserved.txt" >/dev/null
check "server-side copy preserves reserved key characters" "$(cat "$WORK/reserved.txt")" "hello"

mkdir -p "$WORK/excluded"
"$AZCP" -rT --exclude logs "$AZ/tree" "$WORK/excluded" >/dev/null
[ ! -e "$WORK/excluded/logs" ] && ok "a directory exclusion protects the whole remote subtree" \
                              || bad "remote listing copied an excluded subtree"

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

"$AZCP" --attributes-only --metadata stage=remote "$AZ/tree/file.txt" "$AZ/attronly.txt" >/dev/null
"$AZCP" "$AZ/attronly.txt" "$WORK/attronly-remote.txt" >/dev/null
check "blob-to-blob --attributes-only leaves content alone" \
  "$(cat "$WORK/attronly-remote.txt")" "original blob"

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

echo partial > "$WORK/restart.txt"
echo stale > "$WORK/restart.txt.azcp-part"
"$AZCP" -n "$AZ/tree/file.txt" "$WORK/restart.txt" >/dev/null
check "-n restarts an incomplete download without --resume" "$(cat "$WORK/restart.txt")" "hello"
[ ! -e "$WORK/restart.txt.azcp-part" ] && ok "a restarted download clears the stale record" \
                                     || bad "a restarted download left its stale record"

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

# A dry run says, per blob, when it was last written and how it is encoded:
# enough to decide from the dry run alone whether a copy already made is current.
would=$("$AZCP" --dry-run --output=json "$AZ/page.gz" "$WORK/would.gz" 2>/dev/null | grep '"would-copy"' || true)
printf '%s' "$would" | grep -q '"content_encoding":"gzip"' \
  && ok "a dry run names the encoding --decompress would undo" \
  || bad "a dry run left out the blob's encoding: $would"
printf '%s' "$would" | grep -Eq '"modified":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z"' \
  && ok "a dry run says when the blob was last written, in UTC" \
  || bad "a dry run left out when the blob was written: $would"

# --- benchmark --------------------------------------------------------------
"$AZCP" "$SRC/file.txt" "$AZ/bench/azcp-benchmark-000.bin" >/dev/null
if "$AZCP" --benchmark=2x1MiB --output=json "$AZ/bench/" 2>/dev/null \
     | grep -q upload_bytes_per_second; then
  ok "--benchmark measures and reports throughput"
else
  bad "--benchmark did not report a result"
fi
"$AZCP" "$AZ/bench/azcp-benchmark-000.bin" "$WORK/bench-existing" >/dev/null
check "benchmark leaves preexisting blobs untouched" "$(cat "$WORK/bench-existing")" "hello"

# --- overwrite rules --------------------------------------------------------
echo "changed" > "$WORK/changed.txt"
"$AZCP" -n "$WORK/changed.txt" "$AZ/tree/file.txt" >/dev/null
"$AZCP" "$AZ/tree/file.txt" "$WORK/after.txt" >/dev/null
check "-n leaves an existing blob alone" "$(cat "$WORK/after.txt")" "hello"

"$AZCP" "$WORK/changed.txt" "$AZ/tree/file.txt" >/dev/null
"$AZCP" "$AZ/tree/file.txt" "$WORK/after2.txt" >/dev/null
check "a plain copy overwrites" "$(cat "$WORK/after2.txt")" "changed"

# --- a SAS scoped to one container ------------------------------------------
# The credential people are usually handed: every blob in one container and its
# listing, and nothing about the container itself. Get Container Properties is
# a 403 for such a token whatever permissions it carries, and the emulator
# enforces that as the service does, so this is the real failure rather than a
# stand-in for it. The token is signed here, with the development key, so the
# check needs no Azure CLI.
#
# The string to sign is the one sv=2020-10-02 defines: the fields in this
# order, the ones left out empty, the resource "c" for a container.
container_sas() {  # CONTAINER PERMISSIONS
  local version=2020-10-02 expiry=2099-01-01T00:00:00Z hexkey sig
  hexkey=$(printf %s "$AZURE_STORAGE_KEY" | base64 -d | od -An -tx1 | tr -d ' \n')
  sig=$(printf '%s\n\n%s\n/blob/%s/%s\n\n\n\n%s\nc\n\n\n\n\n\n' \
          "$2" "$expiry" "$AZURE_STORAGE_ACCOUNT" "$1" "$version" \
        | openssl dgst -sha256 -mac HMAC -macopt "hexkey:$hexkey" -binary | base64 \
        | sed 's/+/%2B/g; s,/,%2F,g; s/=/%3D/g')
  printf 'sv=%s&sr=c&sp=%s&se=%s&sig=%s' "$version" "$2" "${expiry//:/%3A}" "$sig"
}

# with_sas runs azcp holding $SAS and nothing else, so the account key cannot
# be what made something work.
with_sas() {
  env -u AZURE_STORAGE_ACCOUNT -u AZURE_STORAGE_KEY AZURE_STORAGE_SAS_TOKEN="$SAS" "$AZCP" "$@"
}

if ! command -v openssl >/dev/null; then
  bad "a container SAS cannot be signed without openssl"
else
  SCOPED="${CONTAINER}-scoped"
  "$AZCP" --create-container "$SRC/file.txt" "$ACCOUNT/$SCOPED/other.txt" >/dev/null
  SAS=$(container_sas "$SCOPED" rwl)

  # The shape a job takes that keeps one file of state in a container: fetch it
  # if it is there, and write it back. The container root is the source, so it
  # is the container itself that has to be found.
  mkdir -p "$WORK/scoped"
  if with_sas -r --include state.json -T "$ACCOUNT/$SCOPED" "$WORK/scoped" >/dev/null 2>&1 \
     && [ -z "$(ls -A "$WORK/scoped")" ]; then
    ok "a container SAS reads the container root, and a blob not there yet is no error"
  else
    bad "a container SAS could not copy from the container root"
  fi

  echo "first" > "$WORK/state.json"
  if out=$(with_sas --log-level=debug -T "$WORK/state.json" "$ACCOUNT/$SCOPED/state.json" 2>&1); then
    ok "a container SAS writes a new blob"
  else
    bad "a container SAS could not write a new blob: $out"
  fi
  # Were the emulator to stop refusing, everything here would pass and prove
  # nothing.
  check "the emulator refuses such a token the container's properties" \
    "$(printf '%s\n' "$out" | grep -c 'may not ask whether the container exists' || true)" "1"

  echo "second" > "$WORK/state.json"
  with_sas -T "$WORK/state.json" "$ACCOUNT/$SCOPED/state.json" >/dev/null 2>&1 || true
  with_sas -r --include state.json -T "$ACCOUNT/$SCOPED" "$WORK/scoped" >/dev/null 2>&1 || true
  check "a container SAS overwrites the blob and fetches it back" \
    "$(cat "$WORK/scoped/state.json" 2>/dev/null)" "second"
  check "--include still leaves the rest of the container alone" "$(ls "$WORK/scoped")" "state.json"

  # A sync each way, which asks about the container as a destination too.
  # Removing what the source does not have is the one thing that takes more
  # than rwl, and reading takes less.
  mkdir -p "$WORK/scoped-src" "$WORK/scoped-mirror"
  echo kept > "$WORK/scoped-src/kept.txt"
  echo stale > "$WORK/scoped-mirror/stale.txt"
  SAS=$(container_sas "$SCOPED" rwdl)
  with_sas -r -u --delete -T "$WORK/scoped-src" "$ACCOUNT/$SCOPED" >/dev/null 2>&1 || true
  SAS=$(container_sas "$SCOPED" rl)
  with_sas -r -u --delete -T "$ACCOUNT/$SCOPED" "$WORK/scoped-mirror" >/dev/null 2>&1 || true
  check "a container SAS syncs with --delete in both directions" \
    "$(ls "$WORK/scoped-mirror")" "kept.txt"

  # Such a token cannot be told the container is missing, nor create it, so the
  # write is what says so.
  SAS=$(container_sas "${CONTAINER}-absent" rwl)
  out=$(with_sas --create-container -T "$WORK/state.json" "$ACCOUNT/${CONTAINER}-absent/state.json" 2>&1 || true)
  case "$out" in
    *ContainerNotFound*) ok "a missing container is reported by the write" ;;
    *) bad "a missing container was reported as: $out" ;;
  esac
fi

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
