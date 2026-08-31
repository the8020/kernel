package runtime

import "testing"

func TestSelectModePrefersFullAndFallsBackToRootless(t *testing.T) {
	mode, _, err := SelectMode("auto", true, true)
	if err != nil || mode != ModeFull {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	mode, _, err = SelectMode("auto", false, true)
	if err != nil || mode != ModeRootless {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	if mode, _, err = SelectMode("full", false, true); err == nil || mode != ModeUnavailable {
		t.Fatalf("explicit unavailable full mode=%q err=%v", mode, err)
	}
	if mode, _, err = SelectMode("rootless", true, true); err != nil || mode != ModeRootless {
		t.Fatalf("explicit rootless mode=%q err=%v", mode, err)
	}
}

func TestRootlessIsolationReportDisclosesReducedGuarantees(t *testing.T) {
	report := NewIsolationReport("auto", ModeRootless, "fallback", false, true)
	if !report.Capabilities["gvisor"] || !report.Capabilities["rootless"] || report.Capabilities["network_namespace"] || report.Capabilities["hard_resource_limits"] || len(report.Limitations) != 3 {
		t.Fatalf("report=%#v", report)
	}
}
