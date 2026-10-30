package vm

import "time"

// CrashExitStatus is expanse-block-run's exit status when a guest stops on its own within
// CrashWindow of booting; the unit doesn't restart on it, and the agent backs off instead.
const CrashExitStatus = 75

// CrashWindow is how soon after boot a guest that stops on its own counts as crashed.
const CrashWindow = time.Minute
