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
//
// The stale-claim reaper (Python's reap_dead_jobs_once) is deliberately not a subsystem here: it must call the
// job types' failure hooks, which only the worker registers (with worker-only collaborators such as the
// destination sync engine), so every worker replica runs queue.RunReaper instead. Replicas share it safely
// (FOR UPDATE SKIP LOCKED), and it keeps working when the scheduler is down.
package schedboot
