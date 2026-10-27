// Package schedboot is the Go scheduler's reconcile loop and its nine cold-start bootstraps
// (apps/api/scheduler.py): every SCHEDULER_RECONCILE_SECONDS each subsystem looks for due rows in its
// non-RLS schedule mirror and enqueues the next occurrence, once per fire key, so a restart never loses
// a recurring job and a second pass never duplicates one.
//
// Three bootstraps already lived in their feature packages (automations, intent poller, source health);
// signal_scan, outreach, outreach_inbound, audiences, retention and playbooks are ported here from the
// Python bootstrap_* functions and are tested against rows recorded from them (testdata).
//
// One scheduler is active at a time: Lease holds a Postgres session advisory lock (LeaseKey). The Python
// scheduler must take the same lock before its first reconcile for the guarantee to hold across both.
package schedboot
