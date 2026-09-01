// Package resources generates and reads cgroup v2 resource controls.
package resources

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"the8020/kernel/sandbox/model"
)

func UnifiedSettings(limits model.ResourceLimits) (map[string]string, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return map[string]string{
		"pids.max": strconv.FormatInt(limits.PIDMaximum, 10),
	}, nil
}

func ReadMetrics(directory string) (model.ResourceMetrics, error) {
	var metrics model.ResourceMetrics
	var err error
	if metrics.MemoryCurrent, err = readInteger(filepath.Join(directory, "memory.current")); err != nil {
		return metrics, err
	}
	peak, peakErr := readInteger(filepath.Join(directory, "memory.peak"))
	if peakErr == nil {
		metrics.MemoryPeak = peak
	} else if !errors.Is(peakErr, os.ErrNotExist) {
		return metrics, peakErr
	}
	if metrics.PIDCurrent, err = readInteger(filepath.Join(directory, "pids.current")); err != nil {
		return metrics, err
	}
	if metrics.MemoryEvents, err = readKeyValues(filepath.Join(directory, "memory.events")); err != nil {
		return metrics, err
	}
	if metrics.PIDEvents, err = readKeyValues(filepath.Join(directory, "pids.events")); err != nil {
		return metrics, err
	}
	if metrics.CPUStat, err = readKeyValues(filepath.Join(directory, "cpu.stat")); err != nil {
		return metrics, err
	}
	if metrics.CgroupEvents, err = readKeyValues(filepath.Join(directory, "cgroup.events")); err != nil {
		return metrics, err
	}
	if usage, ok := metrics.CPUStat["usage_usec"]; ok {
		metrics.CPUUsageMicros = int64(usage)
	}
	return metrics, nil
}

func readInteger(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return value, nil
}

func readKeyValues(path string) (map[string]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("parse %s: invalid line %q", filepath.Base(path), scanner.Text())
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse %s field %s: %w", filepath.Base(path), fields[0], err)
		}
		values[fields[0]] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return values, nil
}
