package web

import (
	"context"
	"strings"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/auth"
)

// ampSchedule reports AMP's own backup schedule, when it is still running.
//
// Two schedules backing up one instance is the most expensive misconfiguration
// available here, and nothing else would ever mention it: on the instance this
// was written against, AMP was writing a complete 7.37 GB zip every hour --
// 78.7 GB in total -- while amp-bb's entire repository, holding forty-nine
// snapshots, came to 6.5 GiB.
//
// It is read and nothing more. Switching AMP's schedule off is AMP's business,
// and an interface that reaches into another tool's configuration is one
// nobody trusts twice.
type ampSchedule struct {
	Enabled bool `json:"enabled"`
	// Triggers describes each thing that would take an AMP backup.
	Triggers []ampTrigger `json:"triggers,omitempty"`
}

type ampTrigger struct {
	Description string   `json:"description"`
	Tasks       []string `json:"tasks,omitempty"`
}

// scheduleData is the subset of Core.GetScheduleData this needs. AMP returns a
// great deal more; naming only what is read keeps a schema change from turning
// a warning banner into a decoding error.
type scheduleData struct {
	PopulatedTriggers []struct {
		Description  string `json:"Description"`
		EnabledState int    `json:"EnabledState"`
		Enabled      *bool  `json:"Enabled"`
		Tasks        []struct {
			TaskMethodName string `json:"TaskMethodName"`
			Description    string `json:"Description"`
		} `json:"Tasks"`
	} `json:"PopulatedTriggers"`
}

// ampBackupSchedule asks AMP what it has scheduled, as the caller.
//
// Deliberately as the caller and not as the service account: reading the
// schedule is not something the backup account has permission for, and it
// should not grow one for a banner. A caller who may not read it simply does
// not see the warning, which is better than asking for a wider permission.
func (s *server) ampBackupSchedule(ctx context.Context, sess *auth.Session) *ampSchedule {
	if s.UserClient == nil || sess == nil || sess.AMP == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client, err := s.UserClient(sess.AMP)
	if err != nil {
		return nil
	}
	var data scheduleData
	if err := client.Call(ctx, "Core", "GetScheduleData", nil, &data); err != nil {
		// No permission, an older build, a renamed method: all of them mean
		// "cannot tell", and cannot tell is not the same as "all clear", so
		// nothing is claimed either way.
		return nil
	}

	out := &ampSchedule{}
	for _, trigger := range data.PopulatedTriggers {
		var tasks []string
		for _, task := range trigger.Tasks {
			if !isBackupTask(task.TaskMethodName) {
				continue
			}
			label := task.Description
			if label == "" {
				label = task.TaskMethodName
			}
			tasks = append(tasks, label)
		}
		if len(tasks) == 0 {
			continue
		}
		if !triggerEnabled(trigger.Enabled, trigger.EnabledState) {
			continue
		}
		out.Enabled = true
		out.Triggers = append(out.Triggers, ampTrigger{
			Description: trigger.Description,
			Tasks:       tasks,
		})
	}
	if !out.Enabled {
		return nil
	}
	return out
}

// isBackupTask recognises the tasks that produce an AMP backup. Matching on
// the plugin name rather than an exact method keeps this working if CubeCoders
// adds a second way to take one.
func isBackupTask(method string) bool {
	m := strings.ToLower(method)
	return strings.Contains(m, "backup") && !strings.Contains(m, "restore") &&
		!strings.Contains(m, "delete")
}

// triggerEnabled copes with AMP having spelled this two ways across versions.
func triggerEnabled(enabled *bool, state int) bool {
	if enabled != nil {
		return *enabled
	}
	// EnabledState is a tri-state in AMP's own configuration: unset, disabled,
	// enabled. Anything that is not explicitly disabled counts as running,
	// because a warning that fails to appear is worse than one that does.
	return state != 1
}
