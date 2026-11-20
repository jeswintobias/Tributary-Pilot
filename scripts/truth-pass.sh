#!/usr/bin/env bash
# Check the README's claims against the repository.
#
# WHY THIS IS A SCRIPT AND NOT A READ-THROUGH
#
# Documentation drifts silently. A test count goes up, a script is renamed, a
# coverage figure moves, and the README keeps saying what used to be true —
# and nobody notices, because nothing fails. This project's whole posture is
# "never claim something works unless a test or script proves it", which has to
# include the claims the README itself makes.
#
# So the mechanical claims are checked mechanically. What this CANNOT check is
# whether a claim is honest in substance; that still takes reading. The things
# it does check are the ones that rot without anybody lying.
#
#   ./scripts/truth-pass.sh
#
# No network required. Everything here is the repository checking itself.

set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

T_EXIT=0
CHECKS=0
pass() { CHECKS=$(( CHECKS + 1 )); printf '  \033[0;32mok\033[0m    %s\n' "$*"; }
fail() { CHECKS=$(( CHECKS + 1 )); T_EXIT=1; printf '  \033[0;31mFAIL\033[0m  %s\n' "$*"; }
step() { printf '\n\033[1;36m── %s\033[0m\n' "$*"; }

README="README.md"

# ---------------------------------------------------------------------------
step "Every file the README points at exists"
# ---------------------------------------------------------------------------
# Markdown links to local paths, plus any ./network or ./scripts command in a
# fenced block.
missing=0
while IFS= read -r target; do
  [ -z "${target}" ] && continue
  # Strip an anchor, and skip URLs.
  path="${target%%#*}"
  case "${path}" in http*|"") continue ;; esac
  if [ ! -e "${path}" ]; then
    fail "README links to ${path}, which does not exist"
    missing=$(( missing + 1 ))
  fi
done < <(grep -o '](\([^)]*\))' "${README}" | sed 's/^](//;s/)$//')
[ "${missing}" -eq 0 ] && pass "every local link in the README resolves"

missing=0
while IFS= read -r script; do
  [ -z "${script}" ] && continue
  if [ ! -x "${script}" ]; then
    fail "README tells the reader to run ${script}, which is missing or not executable"
    missing=$(( missing + 1 ))
  fi
done < <(grep -oE '\./(network|scripts)/[a-z0-9._-]+\.sh' "${README}" | sort -u)
[ "${missing}" -eq 0 ] && pass "every script the README tells you to run exists and is executable"

# ---------------------------------------------------------------------------
step "Every script in network/ is syntactically valid"
# ---------------------------------------------------------------------------
bad=0
for f in network/*.sh scripts/*.sh; do
  [ -e "${f}" ] || continue
  if ! bash -n "${f}" 2>/dev/null; then
    fail "${f} does not parse"
    bad=$(( bad + 1 ))
  fi
done
[ "${bad}" -eq 0 ] && pass "all shell scripts parse"

# Inline python inside those scripts, too. A broken block there fails at the
# worst moment — mid-demo, after several minutes of setup.
bad=0
for f in network/*.sh scripts/*.sh; do
  [ -e "${f}" ] || continue
  # This script is skipped: it embeds the very regex it searches for, so
  # scanning itself finds the pattern inside its own source and reports a
  # syntax error in something that is not python at all.
  [ "${f}" = "scripts/truth-pass.sh" ] && continue
  if ! python3 - "${f}" <<'PY'
import re, ast, sys
src = open(sys.argv[1]).read()
bad = 0
for i, block in enumerate(re.findall(r"python3 -c '([\s\S]*?)'", src)):
    try:
        ast.parse(block)
    except SyntaxError as e:
        print(f"    block {i}: {e}")
        bad += 1
sys.exit(1 if bad else 0)
PY
  then
    fail "${f} contains inline python that does not parse"
    bad=$(( bad + 1 ))
  fi
done
[ "${bad}" -eq 0 ] && pass "all inline python inside shell scripts parses"

# ---------------------------------------------------------------------------
step "The chaincode test count and coverage are what the README says"
# ---------------------------------------------------------------------------
if command -v go >/dev/null 2>&1; then
  # Take the coverage of the package that HAS tests. With -cover, a package
  # with no test files still prints "coverage: 0.0% of statements", and taking
  # the first line reports that instead of the real figure.
  # The package WITH tests, by name. With -cover, the parent package also
  # prints a coverage line — 0.0%, because it has no tests — and taking the
  # first line reports that instead of the real figure.
  actual_cov="$(cd chaincode/settlement && go test -count=1 -cover ./... 2>/dev/null \
    | grep 'settlement/settlement' | grep -o 'coverage: [0-9.]*%' | sed 's/coverage: //' | head -1)"
  actual_tests="$(cd chaincode/settlement && go test -v -count=1 ./... 2>/dev/null | grep -c '^--- PASS\|^    --- PASS')"

  claimed_tests="$(grep -oE '[0-9]+ unit tests' "${README}" | head -1 | grep -oE '[0-9]+')"
  claimed_cov="$(grep -oE '[0-9]+\.[0-9]+% statement coverage' "${README}" | head -1 | grep -oE '[0-9]+\.[0-9]+%')"

  if [ -z "${claimed_tests}" ]; then
    fail "the README makes no test-count claim to check"
  elif [ "${claimed_tests}" = "${actual_tests}" ]; then
    pass "README says ${claimed_tests} chaincode tests; ${actual_tests} ran"
  else
    fail "README says ${claimed_tests} chaincode tests, but ${actual_tests} ran"
  fi

  if [ -z "${claimed_cov}" ]; then
    fail "the README makes no coverage claim to check"
  elif [ "${claimed_cov}" = "${actual_cov}" ]; then
    pass "README says ${claimed_cov} coverage; go test reports ${actual_cov}"
  else
    fail "README says ${claimed_cov} coverage, but go test reports ${actual_cov}"
  fi

  if (cd chaincode/settlement && go vet ./... >/dev/null 2>&1); then
    pass "go vet is clean"
  else
    fail "go vet reports problems"
  fi
  unformatted="$(cd chaincode/settlement && gofmt -l . 2>/dev/null)"
  if [ -z "${unformatted}" ]; then
    pass "gofmt is clean"
  else
    fail "gofmt would rewrite: ${unformatted}"
  fi
else
  fail "go is not installed, so the chaincode claims cannot be checked"
fi

# ---------------------------------------------------------------------------
step "The service test counts are what the docs say"
# ---------------------------------------------------------------------------
check_node_tests() {
  local dir="$1" doc="$2"
  # No node_modules check: a service with no dependencies never has one, and
  # skipping it silently is how a test count stops being verified.
  if ! [ -f "${dir}/package.json" ]; then
    fail "${dir} has no package.json"
    return
  fi
  local actual
  actual="$(cd "${dir}" && npm test 2>/dev/null | grep -E '^# pass|^ℹ pass' | grep -oE '[0-9]+' | head -1)"
  local claimed
  claimed="$(grep -oE '[0-9]+ tests' "${doc}" 2>/dev/null | head -1 | grep -oE '[0-9]+')"
  if [ -z "${actual}" ]; then
    fail "${dir}: could not read a test count from npm test"
  elif [ -z "${claimed}" ]; then
    fail "${doc} makes no test-count claim for ${dir}"
  elif [ "${actual}" = "${claimed}" ]; then
    pass "${doc} says ${claimed} tests for ${dir}; ${actual} ran"
  else
    fail "${doc} says ${claimed} tests for ${dir}, but ${actual} ran"
  fi
}
check_node_tests services/projector services/projector/README.md
check_node_tests services/iso20022 services/iso20022/README.md

# ---------------------------------------------------------------------------
step "Evidence transcripts referenced by the docs exist and record a pass"
# ---------------------------------------------------------------------------
for log in docs/evidence/*.log; do
  [ -e "${log}" ] || continue
  if grep -qE '^(B_EXIT|N_EXIT|M_EXIT|P_EXIT|D_EXIT|V_EXIT|A_EXIT|I_EXIT|S_EXIT|T_EXIT)=0' "${log}"; then
    pass "$(basename "${log}") records a passing run"
  elif grep -qE '^(B_EXIT|N_EXIT|M_EXIT|P_EXIT|D_EXIT|V_EXIT|A_EXIT|I_EXIT|S_EXIT|T_EXIT)=[1-9]' "${log}"; then
    fail "$(basename "${log}") records a FAILING run but is kept as evidence"
  else
    fail "$(basename "${log}") has no exit marker, so it proves nothing either way"
  fi
done

# ---------------------------------------------------------------------------
step "Claims that have been withdrawn are marked as withdrawn"
# ---------------------------------------------------------------------------
# The Phase B conservation claim was read through a truncated range query. It
# must not quietly reappear as a live result.
if grep -q "Books verified balanced on-chain" "${README}"; then
  fail "the README still states the withdrawn Phase B on-chain conservation claim"
else
  pass "the withdrawn Phase B conservation claim is not restated"
fi
if [ -f docs/DRUNIX_RANGE_QUERY_BUG.md ] && grep -q "DRUNIX_RANGE_QUERY_BUG" "${README}"; then
  pass "the platform defect is written up and linked from the README"
else
  fail "the DRUNIX range-query defect is not linked from the README"
fi

# ---------------------------------------------------------------------------
step "Nothing claims a throughput number"
# ---------------------------------------------------------------------------
# The repository says repeatedly that no TPS or latency figure can be taken
# from an emulated stack. This checks nobody has quietly added one.
if grep -riE '[0-9]+ ?(tps|transactions per second)' README.md docs/*.md 2>/dev/null \
     | grep -viE 'not |never |no |cannot|without' | grep -q .; then
  fail "something now quotes a throughput figure:"
  grep -rniE '[0-9]+ ?(tps|transactions per second)' README.md docs/*.md 2>/dev/null \
    | grep -viE 'not |never |no |cannot|without' | sed 's/^/        /'
else
  pass "no throughput figure is claimed anywhere"
fi

# ---------------------------------------------------------------------------
step "Upstream DRUNIX is not vendored"
# ---------------------------------------------------------------------------
if [ -d drunix ] || git ls-files 2>/dev/null | grep -q '^drunix/'; then
  fail "upstream DRUNIX appears to be vendored into this repository"
else
  pass "upstream DRUNIX is not vendored"
fi

# ---------------------------------------------------------------------------
step "No secret material is committed"
# ---------------------------------------------------------------------------
leaked="$(git ls-files 2>/dev/null | grep -E '\.(key|pem)$|\.tokens\.json$' || true)"
if [ -n "${leaked}" ]; then
  fail "key or token material is tracked by git:"
  echo "${leaked}" | sed 's/^/        /'
else
  pass "no key or token material is tracked"
fi

# ---------------------------------------------------------------------------
step "Summary"
# ---------------------------------------------------------------------------
echo "  ${CHECKS} claims checked"
if [ "${T_EXIT}" -ne 0 ]; then
  echo "  Some are no longer true. Fix the claim or fix the thing."
fi
echo
echo "T_EXIT=${T_EXIT}"
exit "${T_EXIT}"
