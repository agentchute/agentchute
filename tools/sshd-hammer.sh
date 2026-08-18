#!/bin/sh
# sshd-hammer.sh — TEMPORARY. Provokes red #3 rather than waiting for it.
#
# TestSSHDChildEnvSendAndSupervisedRelaunchDefault fails ~15% of ubuntu runs
# (2 of 13 measured) and 0% of macOS runs. At that rate a green head is what a
# fix that does nothing would produce ~85% of the time, so watching one run per
# head cannot confirm or refute anything: it needs ~20 consecutive greens before
# the evidence beats chance.
#
# So run the row 20 times in ONE CI round instead. At 15% independent per
# iteration that is a ~96% chance of at least one failure, and the diagnostic
# output lands in the same round.
#
# A separate process per iteration, not `go test -count=20`: the row asserts on
# process state (child PIDs, mux sockets, an sshd it stops and restarts), and
# one process running it twenty times shares more than the row assumes. It also
# means an iteration that hangs or panics cannot take the others' evidence with
# it.
#
# DELETE THIS FILE AND ITS CI JOB before the PR is gated.
set -u
strip_env=$(env | awk -F= '/^AGENTCHUTE_/{print "-u " $1}')
iterations=${SSHD_HAMMER_ITERATIONS:-20}
failures=0
i=1
while [ "$i" -le "$iterations" ]; do
	printf '\n===== iteration %s/%s =====\n' "$i" "$iterations"
	# Word-splitting $strip_env is intentional: it expands to the -u flags.
	# shellcheck disable=SC2086
	if env $strip_env AGENTCHUTE_SSHD_TEST=1 AGENTCHUTE_SSHD_DUMP_ALWAYS=1 go test -tags sshd_integration \
		-run TestSSHDChildEnvSendAndSupervisedRelaunchDefault \
		-count=1 -v -timeout 300s ./integration/sshd/; then
		printf 'iteration %s: PASS\n' "$i"
	else
		failures=$((failures + 1))
		printf 'iteration %s: FAIL\n' "$i"
	fi
	i=$((i + 1))
done
printf '\n===== %s failure(s) in %s iterations =====\n' "$failures" "$iterations"

# Stage 2: the contention probe. It drives 32 concurrent one-shots for ONE agent
# with a serve lane polling the same hub, which is the condition hypothesised to
# end a hub session's channel. It stays green on macOS — but macOS never
# reproduces red #3 at all, so that proves nothing. Ubuntu is the only place this
# can discriminate: if contention between concurrent sessions is the mechanism,
# this reproduces it far faster than one-in-five whole-test runs.
printf '\n===== contention probe (ubuntu discriminator) =====\n'
# shellcheck disable=SC2086
if env $strip_env AGENTCHUTE_SSHD_TEST=1 AGENTCHUTE_SSHD_DUMP_ALWAYS=1 go test -tags sshd_integration \
	-run TestSSHDConcurrentOneShotsForOneAgentProbe \
	-count=3 -v -timeout 600s ./integration/sshd/; then
	printf 'contention probe: PASS (concurrency alone does not reproduce)\n'
else
	printf 'contention probe: FAIL (concurrency alone REPRODUCES)\n'
fi
# Always exit 0. A failure here is the POINT of the job, and this is a
# diagnostic job — it must not turn the branch red and make the real suite's
# verdict ambiguous, which is the exact problem it exists to resolve.
exit 0
