package scheduler

import "go.kenn.io/msgvault/internal/vector"

// EmbeddingStatus includes cron and post-sync passes. Post-sync passes acquire
// their own gate reservation after the sync releases its reservation.
func (s *Scheduler) EmbeddingStatus() vector.EmbeddingStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := vector.EmbeddingStatus{Job: vector.EmbeddingJobState{State: "idle"}, Scheduler: vector.EmbeddingSchedulerState{
		Registered: s.genericFuncs["embed"] != nil, Schedule: s.genericSchedules["embed"], RunAfterSync: s.genericAfterSync["embed"],
	}}
	if started := s.genericStartedAt["embed"]; !started.IsZero() {
		status.Job.State = "running"
		status.Job.StartedAt = &started
	} else if queued := s.genericQueuedAt["embed"]; !queued.IsZero() {
		status.Job.State = "queued"
		status.Job.QueuedAt = &queued
	}
	return status
}
