package watcher

import (
	"math"
	"os"
	"time"
)

func HeartbeatIsFresh(path string, pollIntervalSeconds, buildTimeoutSeconds int, now time.Time) bool {
	if pollIntervalSeconds < 0 || buildTimeoutSeconds < 0 {
		return false
	}

	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	maxDurationSeconds := int64(math.MaxInt64 / int64(time.Second))
	pollSeconds, buildSeconds := int64(pollIntervalSeconds), int64(buildTimeoutSeconds)
	if pollSeconds > maxDurationSeconds-120 || buildSeconds > maxDurationSeconds-pollSeconds-120 {
		return true
	}

	staleAfter := time.Duration(pollSeconds+buildSeconds+120) * time.Second
	return now.Sub(info.ModTime()) < staleAfter
}
