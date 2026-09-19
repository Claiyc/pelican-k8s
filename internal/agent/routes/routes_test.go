package routes

import (
	"testing"

	"github.com/pelican/wings/environment"
)

func TestGameReady(t *testing.T) {
	for _, tc := range []struct {
		states []string
		want   bool
	}{
		{nil, false},
		{[]string{environment.ProcessOfflineState}, false},
		{[]string{environment.ProcessStartingState}, false},
		{[]string{environment.ProcessStoppingState}, false},
		{[]string{environment.ProcessRunningState}, true},
		{[]string{environment.ProcessRunningState, environment.ProcessStartingState}, false},
	} {
		if got := gameReady(tc.states); got != tc.want {
			t.Errorf("gameReady(%v) = %v, want %v", tc.states, got, tc.want)
		}
	}
}
