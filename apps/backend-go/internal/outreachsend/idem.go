package outreachsend

// The send idempotency key scheme (seq:<ws>:<seq>:<enrollment>:<step>) is
// SequenceStepIdem in enqueue.go (Task 17 owns the producer); the handler consumes
// the key from the job payload and never rebuilds it.
