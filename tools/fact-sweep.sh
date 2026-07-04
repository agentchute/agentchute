#!/bin/sh
# fact-sweep — verify the published project numbers against the tree (AGENTS.md E6).
# Run from the repo root before tagging or writing launch copy. Exit 1 on any mismatch.
# shellcheck disable=SC2086  # $SURFACES and $claims word-split by design (path/number lists)
set -u

fail=0
say() { printf '%s\n' "$*"; }
bad() { say "FAIL: $*"; fail=1; }

# Surfaces that carry published numbers (docs + website; CHANGELOG history is exempt below).
SURFACES="README.md AGENTCHUTE.md web"

# 1. "NNN-line ... Python" claims must equal the actual runner.py line count.
actual_lines=$(wc -l < conformance/example-python-binding/runner.py | tr -d ' ')
claims=$(grep -rhoE '[0-9]+-line[^.]{0,30}Python|[0-9]+-line stdlib' $SURFACES CHANGELOG.md 2>/dev/null | grep -oE '^[0-9]+' | sort -u)
for n in $claims; do
  [ "$n" = "$actual_lines" ] || bad "a '${n}-line' Python-proof claim exists but runner.py is ${actual_lines} lines"
done
say "python proof: runner.py=${actual_lines} lines; claims found: $(printf '%s ' $claims)"

# 2. Vector-count claims ("9 vectors", badge '9%20vectors') must equal core.json's vector count.
actual_vectors=$(grep -c '"id"' conformance/vectors/core.json)
vclaims=$(grep -rhoE '[0-9]+ vectors|[0-9]+%20vectors' $SURFACES 2>/dev/null | grep -oE '^[0-9]+' | sort -u)
for n in $vclaims; do
  [ "$n" = "$actual_vectors" ] || bad "a '${n} vectors' claim exists but core.json has ${actual_vectors}"
done
say "vectors: core.json=${actual_vectors}; claims found: $(printf '%s ' $vclaims)"

# 3. The subtraction number: canonical is the tag-range figure 8,262 (see CHANGELOG v0.9.0
#    measurement note). The superseded 8,281 may appear ONLY inside that historical entry.
stray=$(grep -rn '8,281' $SURFACES 2>/dev/null)
[ -n "$stray" ] && bad "superseded figure 8,281 found outside CHANGELOG history:
$stray"
changelog_8281=$(grep -c '8,281' CHANGELOG.md)
[ "$changelog_8281" -le 2 ] || bad "CHANGELOG mentions 8,281 ${changelog_8281}x — expected <=2 (entry + measurement note)"
say "subtraction figure: no stray 8,281 outside CHANGELOG history"

# 4. Command snippets: the known-wrong flag must not reappear anywhere user-facing.
wrongflag=$(grep -rn 'in-reply-to' $SURFACES 2>/dev/null | grep -v 'in_reply_to')
[ -n "$wrongflag" ] && bad "'--in-reply-to' (not a real flag; use --reply-to) found:
$wrongflag"
say "flags: no --in-reply-to in published surfaces"

if [ "$fail" -eq 0 ]; then say "fact-sweep: PASS"; else say "fact-sweep: FAIL"; fi
exit "$fail"
