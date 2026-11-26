// Package async runs subagents in the background. With it, the model starts a
// subagent with start_task and keeps working while it runs; it can look at a
// task's progress or wait for it (check_task), send it new instructions or,
// once it ended, a follow-up (update_task), cancel it (cancel_task) and list
// the tasks of the conversation (list_tasks). When a task ends, the next
// turn of the thread starts with a user message announcing it with an
// excerpt of its result, so the model learns of it without polling.
//
// A background subagent is started exactly like one of the task tool (see
// subagents.Launcher): the same specs, the stricter permission mode, the
// narrowed skills scope and the call's backend. By default it runs in this
// process; Config.Runner runs tasks elsewhere instead.
//
// A task runs in episodes, each a Handle: its start is the first, and a
// follow-up sent with update_task to an ended task starts the next, on the
// same history, when the Handle is a Continuer. The task keeps its id and
// is announced again when the episode ends. The local runner runs each task
// on a thread of its own, <parent thread>/<task id>, and saves it with the
// parent agent's Checkpointer: those threads are never deleted, by the
// middleware or anything else in it, and they show up among the store's
// threads (checkpoint.Lister), so never give a thread of your own such a
// name; delete them yourself once you no longer need them. Without a
// Checkpointer it keeps them in memory, for this process only, until the
// task's record is pruned (Config.MaxFinished); only those entries are ever
// deleted. A task the middleware released is taken over again through the
// Runner's Reattacher when a follow-up names it.
//
// A background run has nobody to ask for approval in-band: with an Approver
// on the parent agent the subagent inherits it and asks it, in-band, as a
// foreground one does. Without one, a subagent that needs a decision stops
// as StatusWaiting, with the actions it waits for in Outcome.Pending and
// TaskRecord.Pending. The parent model is told the task waits and on what,
// but it cannot answer: no tool approves another run's actions, update_task
// refuses a waiting task, and cancel_task only ends it. The decision belongs
// to the user, through the host: Config.OnSettled tells the host when a task
// starts waiting, and Resolve answers its pending actions and resumes it,
// still nested in the run that started it. A waiting task is never released
// nor forgotten (it does not count against Config.MaxFinished) until it is
// resolved or cancelled.
//
// A task's permission mode is fixed when an episode starts: for the first,
// the stricter of the parent's mode at that moment and the spec's; for a
// follow-up or a decision, the stricter of the parent's current mode and
// the task's own. A parent that enters plan mode or a stricter mode does not
// change the mode of an episode already running, but a follow-up it sends,
// or a decision the host gives after a turn of the parent's thread in that
// mode, runs in it. The middleware
// passes the parent's mode to the runner (FollowUpMode) and the runner
// enforces it: the built-in runner does, and since a custom Runner may not,
// a follow-up or decision through one is refused while the parent's mode is
// stricter than when it started the task (TaskRecord.StartMode). A
// background subagent's State is not merged into the parent's (the parent
// may have moved on by the time it ends); files are shared through the
// backend.
//
// Tasks belong to a thread, not to a run: start_task needs the run's
// agent.Input.ThreadID and fails in a run without one. A top-level run may
// end while its tasks keep running, and a later run on the thread is told
// when they end.
// A run about to end while its tasks run is reminded of them once
// (Config.DisableStopReminder). The reminder is a BeforeStop continuation:
// it uses one of the run's agent.MaxStopContinuations slots, shared with
// every other BeforeStop middleware (rubric, hooks Stop hooks), and, placed
// after them in the stack, it is consulted before them. A nested run's
// tasks are stopped when it ends, so a subagent's own tasks never outlive
// it. Close stops every task; call it (or Agent.Close) before the program
// exits.
//
// Task records live in the run's State (see Tasks); a run only reads,
// announces and cancels the tasks of its own thread. While tasks run, the
// system prompt lists them from those records, so the model still knows of
// them after a compaction.
//
// # After a restart
//
// A task outlives the process that ran it only through its records and its
// thread. When a run starts, every record of its thread that shows a task
// running which this process does not hold (it was saved before a restart)
// is taken over through the Runner's Reattacher, in the stricter of the
// task's permission mode and the parent's current one: the built-in runner
// loads the task's thread from the parent agent's Checkpointer and, when
// that thread holds a pending action, the task waits again; otherwise it
// continues in a new episode told that the process restarted. A task that
// cannot be taken over (no Checkpointer, its thread is gone, or the Runner
// is no Reattacher) is reported to the model as lost. Ended tasks are taken
// over when update_task names them. Resolve takes over a waiting task the
// process does not hold, from the State saved with the Checkpointer of the
// agent the middleware is bound to (see Bind): use one Middleware per
// top-level agent when Resolve must work across restarts. Resolve may go
// on with a task even when it fails with ErrNotWaiting (a task recorded
// running is reattached first). Every episode of a task taken over runs
// nested (agent.RunInfo.Depth at least 1), as one started by start_task
// does, though no tool call of the parent starts it, and on the run's
// backend (for Resolve, the thread's own, see agent.Agent.ThreadBackend).
// One live process must own a thread's tasks at a time: two processes
// that take over the same task both run it.
package async
