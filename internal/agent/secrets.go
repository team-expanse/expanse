package agent

import "github.com/expanse/expanse/internal/secrets"

// secretStore seals block secrets with the cluster secret; nil off-cluster, where
// blocks keep them in their config.
func (a *Agent) secretStore() *secrets.Store {
	if a.ctl == nil || len(a.ctl.secret) == 0 {
		return nil
	}
	return &secrets.Store{St: a.store, ClusterSecret: a.ctl.secret}
}
