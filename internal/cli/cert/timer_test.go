package cert

import (
	"strings"
	"testing"
)

func TestCertTimerUnits(t *testing.T) {
	service, timer := certTimerUnits("/usr/local/bin/kubemono", "/usr/local/kubemono/cluster.yaml")
	for _, want := range []string{
		"Type=oneshot",
		"cert renew -y -f",
		"/usr/local/kubemono/cluster.yaml",
		"PrivateTmp=true",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service 缺少 %q:\n%s", want, service)
		}
	}
	for _, want := range []string{
		"OnCalendar=*-*-* 03:17:00",
		"Persistent=true",
		"Unit=kubemono-cert-renew.service",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer 缺少 %q:\n%s", want, timer)
		}
	}
}
