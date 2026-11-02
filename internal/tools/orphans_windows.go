package tools

import "context"

// reapOrphans does nothing on Windows yet: there's no process group to tell
// our helpers apart by.
func reapOrphans(context.Context, []string) {}
