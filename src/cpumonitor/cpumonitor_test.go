package cpumonitor

import (
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
