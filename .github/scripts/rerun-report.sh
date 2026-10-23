#!/usr/bin/env bash
# Makes the tests that gotestsum re-ran visible on the run page. gotestsum's --rerun-fails-report
# writes one line per re-run test, "<package>.<Test>: <runs> runs, <failures> failures"; a parent
# test and its failing subtest have a line each. A test with more runs than failures failed and then
# passed, which leaves the job green: it gets a warning annotation. Every line goes to the step
# summary, and the step output reruns=true tells the workflow to keep the test output. Always exits
# 0: the re-run policy decides the job's result, not this report.
#
# Usage: rerun-report.sh <report> <title>
set -u

report=$1
title=$2

[ -s "$report" ] || exit 0

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "reruns=true" >> "$GITHUB_OUTPUT"
fi

awk '{
  runs = $2; failures = $4
  if (runs > failures) {
    line = $0
    gsub(/%/, "%25", line)
    print "::warning title=Flaky test (passed on re-run)::" line
  }
}' "$report"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Tests re-run by gotestsum: $title"
    echo
    echo "| Test | Runs | Failures | Outcome |"
    echo "| --- | --- | --- | --- |"
    awk '{
      name = $1; sub(/:$/, "", name)
      outcome = ($2 > $4) ? "passed on re-run" : "failed"
      printf "| `%s` | %s | %s | %s |\n", name, $2, $4, outcome
    }' "$report"
    echo
    echo "The output of every attempt is in the tests.json artifact of this job."
  } >> "$GITHUB_STEP_SUMMARY"
fi

exit 0
