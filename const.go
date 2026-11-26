package telemetry

import "time"

const (
	// DefaultInterval is how often an exporter sends what it holds under a
	// Config that names no Interval.
	DefaultInterval = 10 * time.Second
	// DefaultQueue is how many log records an exporter holds under a Config
	// that names no Queue.
	DefaultQueue = 2048
	// DefaultCrashDelivery is how long a watcher takes at most to deliver
	// the report of a crash under a Config that names no CrashDelivery.
	DefaultCrashDelivery = 30 * time.Second
)
