package server

import (
	"time"
)

const pluginBackgroundJobSchedulerInterval = time.Minute

func (s *Server) StartBillingScheduler() {
	s.billing.StartScheduler(30 * time.Second)
	s.reconciliation.StartScheduler(30 * time.Second)
	s.credentialRefresh.StartScheduler(providerCredentialRefreshInterval)
	s.payloadRetention.StartScheduler(requestPayloadRetentionInterval)
	if s.providerMonitoring != nil {
		s.providerMonitoring.StartScheduler(providerMonitoringSchedulerInterval)
	}
	s.pluginBackgroundRunner.StartScheduler(pluginBackgroundJobSchedulerInterval)
	if s.config.TeamQuotaRebalancerEnabled {
		if gormStore, ok := s.store.(*GormStore); ok {
			interval := time.Duration(s.config.TeamQuotaRebalancerIntervalSeconds) * time.Second
			s.teamQuotaRebalancer = NewTeamQuotaRebalancer(gormStore, interval, s.config.TeamQuotaRebalancerWindowDays)
			s.teamQuotaRebalancer.StartScheduler(interval)
		}
	}
}
