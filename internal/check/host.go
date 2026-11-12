package check

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// CPUTimes is the busy and total CPU time since boot, in seconds.
type CPUTimes struct {
	Busy  float64
	Total float64
}

func ReadCPUTimes(ctx context.Context) (CPUTimes, error) {
	stats, err := cpu.TimesWithContext(ctx, false)
	if err != nil {
		return CPUTimes{}, err
	}
	if len(stats) == 0 {
		return CPUTimes{}, fmt.Errorf("no CPU times reported")
	}
	stat := stats[0]
	idle := stat.Idle + stat.Iowait
	busy := stat.User + stat.System + stat.Nice + stat.Irq + stat.Softirq + stat.Steal
	return CPUTimes{Busy: busy, Total: busy + idle}, nil
}

// CPUProbe measures CPU use over the time since its previous run, so its first run only samples.
type CPUProbe struct {
	Degraded, Down float64
	Read           func(context.Context) (CPUTimes, error)

	previous    CPUTimes
	hasPrevious bool
}

func (probe *CPUProbe) Run(ctx context.Context) Result {
	current, err := probe.Read(ctx)
	if err != nil {
		probe.hasPrevious = false
		return Result{Status: Down, Detail: "cannot read CPU use: " + describe(err)}
	}
	previous, hadPrevious := probe.previous, probe.hasPrevious
	probe.previous, probe.hasPrevious = current, true
	if !hadPrevious {
		return Result{Skip: true}
	}

	elapsed := current.Total - previous.Total
	if elapsed <= 0 {
		return Result{Skip: true}
	}
	percent := min(max((current.Busy-previous.Busy)/elapsed*100, 0), 100)
	return Result{
		Status:       statusOf(percent, probe.Degraded, probe.Down),
		ValuePercent: percent,
		HasValue:     true,
		Detail:       fmt.Sprintf("%.0f%% CPU used", percent),
	}
}

type PercentProbe struct {
	Degraded, Down float64
	Read           func(context.Context) (float64, error)
	// What names the measurement in messages, such as "memory" or "of /data".
	What string
}

func (probe *PercentProbe) Run(ctx context.Context) Result {
	percent, err := probe.Read(ctx)
	if err != nil {
		return Result{Status: Down, Detail: fmt.Sprintf("cannot read %s: %s", probe.What, describe(err))}
	}
	return Result{
		Status:       statusOf(percent, probe.Degraded, probe.Down),
		ValuePercent: percent,
		HasValue:     true,
		Detail:       fmt.Sprintf("%.0f%% %s used", percent, probe.What),
	}
}

func ReadMemoryPercent(ctx context.Context) (float64, error) {
	stat, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return 0, err
	}
	return stat.UsedPercent, nil
}

func DiskPercentReader(path string) func(context.Context) (float64, error) {
	return func(ctx context.Context) (float64, error) {
		stat, err := disk.UsageWithContext(ctx, path)
		if err != nil {
			return 0, err
		}
		return stat.UsedPercent, nil
	}
}
