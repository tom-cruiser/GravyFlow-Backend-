package main

import (
	"strings"
	"testing"
)

func TestValidateAppName(t *testing.T) {
	valid := []string{"nerva-frontend", "a", "app2", strings.Repeat("a", 63)}
	for _, name := range valid {
		if err := validateAppName(name); err != nil {
			t.Errorf("validateAppName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{"nerva-frontend-", "-nerva", "Nerva", "my_app", "my.app", "my app", strings.Repeat("a", 64)}
	for _, name := range invalid {
		if err := validateAppName(name); err == nil {
			t.Errorf("validateAppName(%q) = nil, want error", name)
		}
	}

	if err := validateAppName("Nerva-Frontend-"); err == nil || !strings.Contains(err.Error(), `"nerva-frontend"`) {
		t.Errorf("expected suggestion \"nerva-frontend\", got %v", err)
	}
}
