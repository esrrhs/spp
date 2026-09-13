package version

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	if Version == "" {
		t.Fatal("Version should not be empty")
	}
	full := Full()
	if !strings.Contains(full, Version) {
		t.Errorf("Full() does not contain Version: %s", full)
	}
	short := Short()
	if !strings.Contains(short, Version) {
		t.Errorf("Short() does not contain Version: %s", short)
	}
}
