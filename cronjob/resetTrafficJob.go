package cronjob

import (
	"time"

	"github.com/alireza0/s-ui/logger"
	"github.com/alireza0/s-ui/service"

	"github.com/robfig/cron/v3"
)

type ResetTrafficJob struct {
	service.ClientService
	service.ConfigService
	service.SettingService
	schedule cron.Schedule
}

func NewResetTrafficJob(schedule cron.Schedule) *ResetTrafficJob {
	return &ResetTrafficJob{schedule: schedule}
}

func (s *ResetTrafficJob) Run() {
	loc, err := s.SettingService.GetTimeLocation()
	if err != nil {
		logger.Warning("ResetTrafficJob: get time location failed: ", err)
		return
	}
	now := time.Now().In(loc)

	last, err := s.SettingService.GetGlobalResetLast()
	if err != nil {
		logger.Warning("ResetTrafficJob: get last reset time failed: ", err)
		return
	}
	// Configured start date / next boundary not reached yet.
	//
	// Logged, because this used to be a silent return: the setting holds the
	// *next* boundary despite its name, and it is not cleared when the operator
	// changes the schedule. Someone switching a monthly reset to a daily one
	// would then see nothing happen for up to a month, with no clue why.
	if last > now.Unix() {
		logger.Debug("ResetTrafficJob: next reset is at ", time.Unix(last, 0).In(loc).Format(time.RFC3339), ", nothing to do")
		return
	}

	if err = s.ClientService.ResetAllClientsTraffic(); err != nil {
		logger.Warning("ResetTrafficJob: reset all clients failed: ", err)
		return
	}

	// Restart before the bookkeeping write, not after.
	//
	// Clients were just re-enabled in the database, but the running core still
	// holds the old user list. When the write below failed, the old code
	// returned early and never restarted, so those clients stayed disconnected
	// -- and the watchdog does not help, because it only starts a core that is
	// not running.
	if err = s.ConfigService.RestartCore(); err != nil {
		logger.Error("ResetTrafficJob: unable to restart core: ", err)
	}

	// Advance to the next boundary. schedule.Next returns the nearest upcoming
	// occurrence, so if several periods were missed (e.g. downtime) it snaps
	// forward instead of resetting once per missed period.
	next := s.schedule.Next(now)
	if err = s.SettingService.SetGlobalResetLast(next.Unix()); err != nil {
		logger.Warning("ResetTrafficJob: set last reset time failed: ", err)
		return
	}
	logger.Info("ResetTrafficJob: traffic reset for all clients; next reset at ", next.Format(time.RFC3339))
}
