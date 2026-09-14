// Package validate implements the Phase 04 admission validation rules
// (PHASE04.md §3.2, V1–V24). Task T03 fills this in; the import below pins
// the robfig/cron dependency into vendor/ from day one (V21 validates cron
// schedules).
package validate

import (
	_ "github.com/robfig/cron/v3"
)
