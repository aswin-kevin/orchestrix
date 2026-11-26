package file

// Synced reports whether c fsyncs each append; tests use it to pin that the
// zero Config keeps fsync on.
func (c *Checkpointer) Synced() bool { return c.sync }

// Every reports the effective number of deltas between snapshots.
func (c *Checkpointer) Every() int { return c.snapshotEvery }
