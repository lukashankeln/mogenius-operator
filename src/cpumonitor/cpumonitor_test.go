package cpumonitor

import (
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// buildStatLine builds a realistic 52 field stat line.
// utime=1111 stime=2222 cutime=3333 cstime=4444 starttime=5555 tpgid=-1
func buildStatLine(pid string, comm string, utime string) string {
	fields := []string{
		"S", "1", "1", "1", "0", "-1", // state ppid pgrp session tty_nr tpgid
		"4194560", "100", "0", "0", "0", // flags minflt cminflt majflt cmajflt
		utime, "2222", "3333", "4444", // utime stime cutime cstime
		"20", "0", "1", "0", "5555", // priority nice num_threads itrealvalue starttime
	}
	for len(fields)+2 < 52 {
		fields = append(fields, "0")
	}
	return pid + " (" + comm + ") " + strings.Join(fields, " ")
}

func TestGetCpuUsageInfo(t *testing.T) {
	const pid = "1280"

	tests := []struct {
		name    string
		content string
		wantErr bool
		want    ProcPidStat
	}{
		{
			name:    "plain comm",
			content: buildStatLine(pid, "bash", "1111"),
			want:    ProcPidStat{Pid: 1280, Comm: "(bash)", Utime: 1111, Stime: 2222, Cutime: 3333, Cstime: 4444, Starttime: 5555},
		},
		{
			name:    "nested parentheses",
			content: buildStatLine(pid, "(sd-pam)", "1111"),
			want:    ProcPidStat{Pid: 1280, Comm: "((sd-pam))", Utime: 1111, Stime: 2222, Cutime: 3333, Cstime: 4444, Starttime: 5555},
		},
		{
			name:    "comm with spaces",
			content: buildStatLine(pid, "my proc", "1111"),
			want:    ProcPidStat{Pid: 1280, Comm: "(my proc)", Utime: 1111, Stime: 2222, Cutime: 3333, Cstime: 4444, Starttime: 5555},
		},
		{
			name:    "comm containing closing paren and space",
			content: buildStatLine(pid, "a) b", "1111"),
			want:    ProcPidStat{Pid: 1280, Comm: "(a) b)", Utime: 1111, Stime: 2222, Cutime: 3333, Cstime: 4444, Starttime: 5555},
		},
		{
			name:    "comm mimicking additional fields",
			content: buildStatLine(pid, "a) b c d e f g", "1111"),
			want:    ProcPidStat{Pid: 1280, Comm: "(a) b c d e f g)", Utime: 1111, Stime: 2222, Cutime: 3333, Cstime: 4444, Starttime: 5555},
		},
		{
			name:    "truncated line",
			content: pid + " (bash) S 1 1 1",
			wantErr: true,
		},
		{
			name:    "non numeric utime",
			content: buildStatLine(pid, "bash", "abc"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procPath := t.TempDir()
			assert.NoError(t, os.MkdirAll(filepath.Join(procPath, pid), 0o755))
			assert.NoError(t, os.WriteFile(filepath.Join(procPath, pid, "stat"), []byte(tt.content+"\n"), 0o600))

			got, err := getCpuUsageInfo(procPath, pid)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func assertSaneMetrics(t *testing.T, m CpuMetrics) {
	t.Helper()
	for _, v := range []float64{m.User, m.System, m.Idle} {
		assert.False(t, math.IsNaN(v))
		assert.False(t, math.IsInf(v, 0))
		assert.GreaterOrEqual(t, v, 0.0)
		assert.LessOrEqual(t, v, 100.0)
	}
	_, err := json.Marshal(m)
	assert.NoError(t, err)
}

func TestCalculateGlobalMetrics(t *testing.T) {
	m := &cpuMonitor{}
	last := ProcStatCpu{User: 100, Nice: 10, System: 50, Idle: 800, Iowait: 40, Irq: 5, Softirq: 5, Steal: 0}

	tests := []struct {
		name    string
		last    ProcStatCpu
		current ProcStatCpu
		wantOk  bool
		want    CpuMetrics
	}{
		{
			name:    "normal advance",
			last:    last,
			current: ProcStatCpu{User: 120, Nice: 10, System: 60, Idle: 850, Iowait: 50, Irq: 5, Softirq: 5, Steal: 0},
			wantOk:  true,
			want:    CpuMetrics{User: 200.0 / 9, System: 100.0 / 9, Idle: 600.0 / 9},
		},
		{
			name:    "zero delta",
			last:    last,
			current: last,
			wantOk:  false,
		},
		{
			name:    "iowait decreases",
			last:    last,
			current: ProcStatCpu{User: 120, Nice: 10, System: 60, Idle: 870, Iowait: 30, Irq: 5, Softirq: 5, Steal: 0},
			wantOk:  true,
			want:    CpuMetrics{User: 20, System: 10, Idle: 70},
		},
		{
			name:    "all counters below last",
			last:    last,
			current: ProcStatCpu{},
			wantOk:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := m.calculateGlobalMetrics(tt.last, tt.current)
			assert.Equal(t, tt.wantOk, ok)
			assertSaneMetrics(t, got)
			if tt.wantOk {
				assert.InDelta(t, tt.want.User, got.User, 1e-9)
				assert.InDelta(t, tt.want.System, got.System, 1e-9)
				assert.InDelta(t, tt.want.Idle, got.Idle, 1e-9)
			} else {
				assert.Equal(t, CpuMetrics{}, got)
			}
		})
	}
}

func TestUpdateMetricsKeepsBaselineOnReadError(t *testing.T) {
	m := &cpuMonitor{logger: slog.New(slog.DiscardHandler)}
	dir := t.TempDir()
	baseline := ProcStatCpu{User: 100, Idle: 900}
	prevMetrics := CpuMetrics{User: 12, System: 3, Idle: 85}

	gotStat, gotMetrics := m.updateMetrics(filepath.Join(dir, "missing"), baseline, prevMetrics)
	assert.Equal(t, baseline, gotStat)
	assert.Equal(t, prevMetrics, gotMetrics)

	statPath := filepath.Join(dir, "stat")
	line := "cpu 200 0 100 1000 0 0 0 0 0 0\ncpu0 200 0 100 1000 0 0 0 0 0 0\n"
	assert.NoError(t, os.WriteFile(statPath, []byte(line), 0o600))

	gotStat, gotMetrics = m.updateMetrics(statPath, baseline, prevMetrics)
	assert.Equal(t, ProcStatCpu{User: 200, System: 100, Idle: 1000}, gotStat)
	assertSaneMetrics(t, gotMetrics)
	assert.InDelta(t, 100.0/3, gotMetrics.User, 1e-9)
	assert.InDelta(t, 100.0/3, gotMetrics.System, 1e-9)
	assert.InDelta(t, 100.0/3, gotMetrics.Idle, 1e-9)

	again, keptMetrics := m.updateMetrics(statPath, gotStat, gotMetrics)
	assert.Equal(t, gotStat, again)
	assert.Equal(t, gotMetrics, keptMetrics)
}
