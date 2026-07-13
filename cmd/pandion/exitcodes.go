// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Exit codes are the CLI's contract with scripts and CI: a command's status must
// tell the truth about what happened. They match the de-facto scheme already
// emitted across the commands; this is the single place that names them. Keep in
// sync with the docs (P3.1) and `pandion up -h`.
//
// IMPORTANT: a workload `run:` command exits with ITS OWN code (1–255), mirroring
// `pandion ssh -- cmd`. So `up`/`start`/`attach` can exit with an arbitrary code
// that is the workload's — not necessarily one of the constants below — when a
// run command fails. A clean run (or a Ctrl+C detach, which leaves the workload
// running) exits 0.
const (
	codeOK            = 0 // success
	codeError         = 1 // generic / unclassified failure
	codeUsage         = 2 // bad flags/args, missing --id, refused non-interactively
	codeNotFound      = 3 // missing manifest / cluster / node / key
	codeShareError    = 4 // debug-share token assembly failure
	codeRollback      = 5 // provisioning failed and the cluster was rolled back (nothing left)
	codeRefused       = 6 // budget / lockdown / safety refusal (nothing changed)
	codeInfraDegraded = 7 // post-provision setup failed; the cluster is left UP for triage
	codeReapFailure   = 8 // teardown / reap could not reconcile to empty
)
