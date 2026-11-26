// Package asynctest holds the conformance suite every async.Runner and the
// async.Handle values it returns must pass. It is a regular (non-test) file
// so that other packages' tests can import it; nothing outside tests should.
//
// The suite checks the frozen Handle contract: a Handle covers one episode of
// a task, from its start until it ends or starts waiting; Done returns the
// same channel on every call, which closes when the episode stops and stays
// closed; Outcome reports ok only once Done is closed, with Pending set
// exactly when the task is waiting, and never changes afterwards; Stop
// cancels a running episode and is a no-op once Done, also on a waiting
// one; Send fails with async.ErrFinished once Done. Where a Handle
// implements async.Continuer or async.Resolver, it also checks that the
// next episode is a new Handle that runs to its end (approving every pending
// request, for a Resolver) and that only one call per Handle succeeds.
package asynctest
